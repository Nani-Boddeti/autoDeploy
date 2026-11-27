package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
)

// GitHub App creation through the manifest flow (24-E). Pages: the create form
// on /github, the hand-off page that POSTs the manifest to github.com, the
// cross-site callback ("continue"), the same-site confirm page and the result
// pages of the confirm POST.

const (
	// createHandoffCookieName carries "state.code" from the cross-site callback
	// to the same-site confirm POST. Distinct from the setup flow's
	// "__Host-ad_github_handoff" so the two flows can never read each other's cookie.
	createHandoffCookieName = "__Host-ad_github_create"
	createStateTTL          = 10 * time.Minute
	// createCompleteDeadline bounds consume, conversion and insert. It stays
	// well inside the store's 2-minute in-flight window.
	createCompleteDeadline = 60 * time.Second
	// createMinConvertWindow is the least time that must remain before GitHub is called.
	createMinConvertWindow = 20 * time.Second
	githubSettingsAppsURL  = "https://github.com/settings/apps"
)

var (
	githubCreateTemplate         = parsePage("github-create")
	githubCreateContinueTemplate = parsePage("github-create-continue")
	githubCreateCompleteTemplate = parsePage("github-create-complete")
	githubCreateResultTemplate   = parsePage("github-create-result")

	createCodePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	createStatePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

const (
	msgCreateExpired     = "This link expired or was already used. Start again."
	msgCreateInFlight    = "Another App creation is in progress or just failed. Wait 2 minutes, then start again."
	msgCreateRate        = "GitHub is rate limiting requests. Try again later; a new attempt may need to wait 2 minutes."
	msgCreateOrphan      = "GitHub may have created the App, but AutoDeploy could not finish. Delete it, then start again."
	msgCreateUnavail     = "AutoDeploy could not finish the setup. Wait 2 minutes, then start again."
	msgCreateInput       = "Check the account type, the organization login (letters, digits and hyphens) and the App name (up to 34 characters)."
	msgCreateNoKEK       = "AutoDeploy has no encryption key configured, so it cannot store the App. Run the key setup of the host installer, then try again."
	msgCreatePriorApp    = "An App was configured on this installation before. Creating a new one here is disabled; see 'Break-glass' and 'Wrong App' in docs/operations/vps-install.md."
	msgCreateCheckFailed = "GitHub setup is unavailable right now. Try again shortly."
	msgCreateBusy        = "Could not start the App creation. Wait a few minutes, then try again."
)

type githubLink struct{ Href, Text string }

type githubCreateView struct {
	Nav              navView
	Action, Manifest string
}

type githubCreateResultView struct {
	Nav        navView
	Title      string
	Heading    string
	Body, Note string
	IsError    bool
	Refresh    bool
	Step       int
	Links      []githubLink
}

// manifestEnabled reports whether App creation is offered: no App from any
// source and every creation dependency wired (main.go wires them only then).
func (h *Handler) manifestEnabled() bool {
	c := h.config
	return (c.GitHubAppSource == "" || c.GitHubAppSource == GitHubAppSourceNone) && c.GitHubSetup == nil &&
		c.GitHubManifestStore != nil && c.GitHubKEKProvider != nil && c.GitHubManifestConverter != nil && c.RequestRestart != nil
}

func createHandoffCookie(value string, now time.Time) *http.Cookie {
	return &http.Cookie{Name: createHandoffCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 600, Expires: now.Add(10 * time.Minute)}
}

func expiredCreateHandoffCookie() *http.Cookie {
	return &http.Cookie{Name: createHandoffCookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1}
}

// splitCreateHandoff parses "state.code" and validates both parts.
func splitCreateHandoff(value string) (state, code string, ok bool) {
	state, code, found := strings.Cut(value, ".")
	if !found || !createStatePattern.MatchString(state) || !createCodePattern.MatchString(code) {
		return "", "", false
	}
	return state, code, true
}

// writeCreatePage executes the template into a buffer first so a template
// failure never emits a partial page.
func writeCreatePage(w http.ResponseWriter, status int, execute func(*bytes.Buffer) error) {
	var buf bytes.Buffer
	if err := execute(&buf); err != nil {
		plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// kekAvailable probes the KEK provider and clears the probed key. Clearing is
// safe because the provider is loaded fresh per call (no resident ring shares
// the returned copy). It never reports the cause.
func (h *Handler) kekAvailable() bool {
	provider, err := h.config.GitHubKEKProvider()
	if err != nil || provider == nil {
		return false
	}
	_, key, err := provider.Active()
	clear(key)
	return err == nil
}

// githubCreateForm renders /github in create mode.
func (h *Handler) githubCreateForm(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, view githubView) {
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	view.Nav, view.CSRF, view.CreateMode = h.navFor(r.Context(), "github", p), csrf, true
	if view.AccountType == "" {
		view.AccountType = "personal"
	}
	if view.AppName == "" {
		view.AppName = h.defaultAppName()
	}
	writeCreatePage(w, status, func(b *bytes.Buffer) error { return githubTemplate.Execute(b, view) })
}

func (h *Handler) defaultAppName() string {
	u, err := url.Parse(h.config.PublicOrigin)
	if err != nil {
		return githubapp.DefaultAppName("")
	}
	return githubapp.DefaultAppName(u.Hostname())
}

// githubCreate handles POST /github/create: validates the form, stores a
// state and answers the page that POSTs the manifest to GitHub.
func (h *Handler) githubCreate(w http.ResponseWriter, r *http.Request) {
	if !h.manifestEnabled() {
		http.NotFound(w, r)
		return
	}
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 4 {
		badRequest(w)
		return
	}
	for _, name := range []string{"csrf", "account_type", "org_login", "app_name"} {
		if len(v[name]) != 1 || len(v[name][0]) > 256 {
			badRequest(w)
			return
		}
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	retained := githubView{AccountType: v.Get("account_type"), OrgLogin: v.Get("org_login"), AppName: v.Get("app_name")}
	fail := func(status int, msg string) {
		retained.CreateError = msg
		h.githubCreateForm(w, r, p, status, retained)
	}
	input := githubapp.ManifestInput{Origin: h.config.PublicOrigin, AccountType: retained.AccountType, OrgLogin: retained.OrgLogin, Name: retained.AppName}
	if input.AccountType == "personal" {
		input.OrgLogin = ""
	}
	state, err := githubapp.NewCapability(h.config.Random)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	form, err := githubapp.BuildManifest(input, state)
	if err != nil {
		fail(http.StatusBadRequest, msgCreateInput)
		return
	}
	digest, err := githubapp.SetupDigest(state)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	if !h.kekAvailable() {
		fail(http.StatusServiceUnavailable, msgCreateNoKEK)
		return
	}
	switch err = h.config.GitHubManifestStore.CreateManifestState(r.Context(), digest, p, createStateTTL); {
	case err == nil:
	case errors.Is(err, postgres.ErrManifestAppExists):
		redirect(w, "/github")
		return
	case errors.Is(err, postgres.ErrManifestPriorAppRegistered):
		h.logger().Info("github app create: refused", "category", createCategory(err))
		retained.CreateBlocked, retained.CreateBlockedText = true, msgCreatePriorApp
		h.githubCreateForm(w, r, p, http.StatusConflict, retained)
		return
	case errors.Is(err, postgres.ErrManifestStateInvalid):
		// The session is no longer live in the store.
		redirect(w, "/login")
		return
	case errors.Is(err, postgres.ErrManifestInputInvalid):
		fail(http.StatusTooManyRequests, msgCreateBusy)
		return
	default:
		storeFailure(w, err, "unavailable\n")
		return
	}
	// Only this response may submit a form to github.com.
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'self' https://github.com"))
	view := githubCreateView{Nav: h.navFor(r.Context(), "github", p), Action: form.ActionURL, Manifest: string(form.JSON)}
	writeCreatePage(w, http.StatusOK, func(b *bytes.Buffer) error { return githubCreateTemplate.Execute(b, view) })
}

// githubCreateCallback handles GET /github/create/callback. It arrives
// cross-site (no session cookie), never exchanges the code, and only parks
// code+state in a Strict cookie for the same-site confirm POST.
func (h *Handler) githubCreateCallback(w http.ResponseWriter, r *http.Request) {
	if !h.manifestEnabled() {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if len(q) != 2 || len(r.URL.RawQuery) > 2048 || len(q["code"]) != 1 || len(q["state"]) != 1 ||
		!createCodePattern.MatchString(q.Get("code")) || !createStatePattern.MatchString(q.Get("state")) {
		badRequest(w)
		return
	}
	http.SetCookie(w, createHandoffCookie(q.Get("state")+"."+q.Get("code"), h.config.Now()))
	// A link, not a redirect: a redirect chain from github.com stays cross-site
	// and the Strict session cookie would be withheld (same as the setup callback).
	nav := h.navFor(r.Context(), "github", auth.Principal{})
	if _, p, ok := h.session(r.Context(), r); ok {
		nav = h.navFor(r.Context(), "github", p)
	}
	writeCreatePage(w, http.StatusOK, func(b *bytes.Buffer) error {
		return githubCreateContinueTemplate.Execute(b, githubContinueView{Nav: nav})
	})
}

// githubCreateCompleteGet renders the same-site confirm page (it can mint a
// session CSRF token because this navigation carries the Strict session cookie).
func (h *Handler) githubCreateCompleteGet(w http.ResponseWriter, r *http.Request) {
	if !h.manifestEnabled() {
		http.NotFound(w, r)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return
	}
	c, err := r.Cookie(createHandoffCookieName)
	if err != nil {
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
		return
	}
	if _, _, valid := splitCreateHandoff(c.Value); !valid {
		http.SetCookie(w, expiredCreateHandoffCookie())
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	view := githubCompleteView{Nav: h.navFor(r.Context(), "github", p), CSRF: csrf}
	writeCreatePage(w, http.StatusOK, func(b *bytes.Buffer) error { return githubCreateCompleteTemplate.Execute(b, view) })
}

func expiredResult() githubCreateResultView {
	return githubCreateResultView{Title: "Link expired", Heading: "Start again", Body: msgCreateExpired, IsError: true, Step: 1, Links: []githubLink{{"/github", "Back to GitHub setup"}}}
}

func (h *Handler) createResult(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, view githubCreateResultView) {
	view.Nav = h.navFor(r.Context(), "github", p)
	writeCreatePage(w, status, func(b *bytes.Buffer) error { return githubCreateResultTemplate.Execute(b, view) })
}

// trustedGitHubURL accepts only an https://github.com/ URL without userinfo.
func trustedGitHubURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasPrefix(raw, "https://github.com/") && u.Scheme == "https" && u.Host == "github.com" && u.User == nil
}

func orphanResult(htmlURL string) githubCreateResultView {
	view := githubCreateResultView{Title: "GitHub App not finished", Heading: "Delete the App on GitHub", Body: msgCreateOrphan, IsError: true, Step: 1}
	if trustedGitHubURL(htmlURL) {
		view.Links = append(view.Links, githubLink{htmlURL, "Open the App on GitHub"})
	} else {
		view.Links = append(view.Links, githubLink{githubSettingsAppsURL, "Your GitHub Apps"})
		view.Note = "For an organization, open the organization's Developer settings > GitHub Apps instead."
	}
	view.Links = append(view.Links, githubLink{"/github", "Back to GitHub setup"})
	return view
}

// githubCreateCompletePost handles POST /github/create/complete: consume the
// state, exchange the code, store the App, then restart. The handoff cookie is
// cleared on every outcome once session and CSRF passed.
func (h *Handler) githubCreateCompletePost(w http.ResponseWriter, r *http.Request) {
	if !h.manifestEnabled() {
		http.NotFound(w, r)
		return
	}
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	form, err := parseLogoutForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, form.csrf, p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	http.SetCookie(w, expiredCreateHandoffCookie())
	c, err := r.Cookie(createHandoffCookieName)
	if err != nil {
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
		return
	}
	state, code, valid := splitCreateHandoff(c.Value)
	if !valid {
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
		return
	}
	digest, err := githubapp.SetupDigest(state)
	if err != nil {
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
		return
	}
	// Detached from the request: a closed tab must not abandon a conversion
	// that already created the App on GitHub.
	budget := h.createBudget
	if budget <= 0 {
		budget = createCompleteDeadline
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), budget)
	defer cancel()

	switch err = h.config.GitHubManifestStore.ConsumeManifestStateForCreate(ctx, digest, p); {
	case err == nil:
	case errors.Is(err, postgres.ErrManifestAppExists):
		h.logger().Info("github app create: consume refused", "category", createCategory(err))
		redirect(w, "/github")
		return
	case errors.Is(err, postgres.ErrManifestPriorAppRegistered):
		h.logger().Info("github app create: consume refused", "category", createCategory(err))
		h.githubCreateForm(w, r, p, http.StatusConflict, githubView{CreateBlocked: true, CreateBlockedText: msgCreatePriorApp})
		return
	case errors.Is(err, postgres.ErrManifestCreateInFlight):
		h.logger().Info("github app create: consume refused", "category", createCategory(err))
		h.createResult(w, r, p, http.StatusConflict, inFlightResult())
		return
	case errors.Is(err, postgres.ErrManifestStateInvalid), errors.Is(err, postgres.ErrManifestStateExpired), errors.Is(err, postgres.ErrManifestStateConsumed):
		h.logger().Info("github app create: consume refused", "category", createCategory(err))
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
		return
	default:
		h.logger().Error("github app create: consume failed", "category", createCategory(err))
		h.createResult(w, r, p, http.StatusServiceUnavailable, unavailableResult())
		return
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < createMinConvertWindow {
		h.createResult(w, r, p, http.StatusServiceUnavailable, inFlightResult())
		return
	}

	conv, err := h.config.GitHubManifestConverter.ConvertManifestCode(ctx, code)
	// The converter hands back caller-owned secret buffers; clear on every path
	// (InsertRegistration clears them too).
	defer clear(conv.PEM)
	defer clear(conv.WebhookSecret)
	if err != nil {
		h.createConvertFailure(w, r, p, err)
		return
	}
	provider, err := h.config.GitHubKEKProvider()
	if err != nil || provider == nil {
		h.createOrphan(w, r, p, conv.AppID, conv.HTMLURL, "kek")
		return
	}
	_, err = h.config.GitHubManifestStore.InsertRegistration(ctx, postgres.ManifestRegistrationInput{
		Principal: p, StateDigest: digest,
		AppID: conv.AppID, GitHubOwnerID: conv.OwnerID,
		Slug: conv.Slug, ClientID: conv.ClientID, GitHubOwnerType: conv.OwnerType, GitHubOwnerLogin: conv.OwnerLogin, HTMLURL: conv.HTMLURL,
		PrivateKeyPEM: conv.PEM, WebhookSecret: conv.WebhookSecret,
	}, provider)
	if err != nil {
		h.createOrphan(w, r, p, conv.AppID, conv.HTMLURL, createCategory(err))
		return
	}
	h.logger().Info("github app created", "app_id", conv.AppID)
	view := githubCreateResultView{Title: "Applying GitHub App", Heading: "Applying", Body: "Applying... AutoDeploy is restarting to load the App.", Refresh: true, Step: 2,
		Links: []githubLink{{"/github", "Open GitHub setup"}}}
	h.createResult(w, r, p, http.StatusOK, view)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	h.config.RequestRestart()
}

func inFlightResult() githubCreateResultView {
	return githubCreateResultView{Title: "Creation in progress", Heading: "Wait, then start again", Body: msgCreateInFlight, IsError: true, Step: 1, Links: []githubLink{{"/github", "Back to GitHub setup"}}}
}

func unavailableResult() githubCreateResultView {
	return githubCreateResultView{Title: "Setup unavailable", Heading: "Could not finish", Body: msgCreateUnavail, IsError: true, Step: 1, Links: []githubLink{{"/github", "Back to GitHub setup"}}}
}

// createConvertFailure maps a conversion error. Any failure after the request
// may have reached GitHub (response invalid, transport, deadline) is treated as
// a possible orphan App.
func (h *Handler) createConvertFailure(w http.ResponseWriter, r *http.Request, p auth.Principal, err error) {
	var orphan *githubapp.ManifestOrphanError
	switch {
	case errors.Is(err, githubapp.ErrManifestCodeRejected):
		h.createResult(w, r, p, http.StatusBadRequest, expiredResult())
	case errors.Is(err, githubapp.ErrRateLimited):
		view := expiredResult()
		view.Title, view.Heading, view.Body = "GitHub is busy", "Try again later", msgCreateRate
		h.createResult(w, r, p, http.StatusTooManyRequests, view)
	case errors.As(err, &orphan):
		h.createOrphan(w, r, p, orphan.AppID, orphan.HTMLURL, "convert")
	default:
		h.createOrphan(w, r, p, 0, "", "convert")
	}
}

// createCategory names an error class for logs. Never log the error text.
func createCategory(err error) string {
	switch {
	case errors.Is(err, postgres.ErrManifestPriorAppRegistered):
		return "prior_app"
	case errors.Is(err, postgres.ErrManifestAppExists):
		return "app_exists"
	case errors.Is(err, postgres.ErrManifestCreateInFlight):
		return "in_flight"
	case errors.Is(err, postgres.ErrManifestStateInvalid), errors.Is(err, postgres.ErrManifestStateExpired), errors.Is(err, postgres.ErrManifestStateConsumed):
		return "state"
	case errors.Is(err, postgres.ErrManifestInputInvalid):
		return "insert_conflict"
	default:
		return "store"
	}
}

func (h *Handler) createOrphan(w http.ResponseWriter, r *http.Request, p auth.Principal, appID int64, htmlURL, category string) {
	if appID > 0 {
		h.logger().Error("github app orphaned", "app_id", appID, "category", category)
	} else {
		h.logger().Error("github app creation failed after request; app id unknown", "category", category)
	}
	h.createResult(w, r, p, http.StatusBadGateway, orphanResult(htmlURL))
}
