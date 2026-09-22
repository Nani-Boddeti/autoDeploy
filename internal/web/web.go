// Package web provides the browser-only control-plane boundary.
package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/healthgate"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

const (
	loginCookieName   = "__Host-ad_login"
	sessionCookieName = "__Host-ad_session"
	handoffCookieName = "__Host-ad_github_handoff"
	maxBodyBytes      = 8 << 10
	maxForwardedFor   = 32
	// enrollmentTTLSeconds is the single-use enrollment token lifetime the
	// web admin surface always requests (Step 6 Decision 4: 15 minutes).
	enrollmentTTLSeconds = 900
)

var errBadRequest = errors.New("bad request")

// Repository is the narrow persistence surface used by browser authentication.
type Repository interface {
	Ping(context.Context) error
	FindLoginUserByCanonicalUsername(context.Context, string) (postgres.LoginUser, error)
	ReserveThrottle(context.Context, []postgres.ThrottleIdentity, int) (postgres.ThrottleReservation, error)
	CompleteLogin(context.Context, postgres.CompleteLoginInput) (postgres.Session, error)
	ValidateSession(context.Context, auth.SessionToken) (auth.Principal, error)
	RevokeSession(context.Context, auth.SessionToken, auth.Principal, string) error
}

// ServerAdmin is the narrow deployment-agent server administration surface
// the /servers and /environments/mark-reconciled routes use. Every method
// takes the session principal and is owner-scoped by the repository itself;
// this interface never exposes a credential or enrollment digest, only
// caller-supplied digests going in. A nil ServerAdmin disables all of these
// routes (same nil-disables convention as GitHubSetup below): they 404.
type ServerAdmin interface {
	ListServers(context.Context, auth.Principal) ([]postgres.ServerSummary, error)
	RegisterServer(context.Context, auth.Principal, postgres.RegisterServerInput) (server.Server, time.Time, error)
	IssueEnrollment(context.Context, auth.Principal, string, [32]byte, int) (time.Time, error)
	RevokeServer(context.Context, auth.Principal, string, string) error
	ListReconcilingEnvironments(context.Context, auth.Principal) ([]postgres.ReconcilingEnvironment, error)
	MarkReconciled(context.Context, auth.Principal, string, string) error
}

// RuntimeConfigAdmin is the narrow surface the runtime-configuration admin
// pages (/environments/config, /environments/bindings/*) use. Every method
// takes the session principal and is owner-scoped by the repository itself,
// exactly like ServerAdmin above. A nil RuntimeConfigAdmin disables all of
// these routes (same nil-disables convention): they 404. It never returns a
// secret value -- only a bound secret head's identity and current version
// number.
type RuntimeConfigAdmin interface {
	GetEnvironmentRuntimeConfig(context.Context, auth.Principal, string) (postgres.RuntimeConfig, []postgres.RuntimeBinding, bool, error)
	CreateEnvironmentRuntimeConfig(context.Context, auth.Principal, string, postgres.RuntimeConfigInput) (postgres.RuntimeConfig, error)
	UpdateEnvironmentRuntimeConfig(context.Context, auth.Principal, string, postgres.RuntimeConfigInput, int64) (postgres.RuntimeConfig, error)
	AddEnvironmentRuntimeBinding(context.Context, auth.Principal, string, postgres.BindingInput) (postgres.RuntimeBinding, error)
	RemoveEnvironmentRuntimeBinding(context.Context, auth.Principal, string, string) error
}

type Config struct {
	PublicOrigin          string
	CSRFKeys              auth.CSRFKeyRing
	UsernameThrottleKeys  auth.UsernameThrottleKeyRing
	TrustedProxyCIDRs     []netip.Prefix
	AuthThrottleMaxRows   int
	PasswordPolicy        auth.Argon2Policy
	DummyPassword         auth.DummyPasswordVerifier
	GitHubSetup           *githubapp.SetupService
	GitHubInstallationURL string
	ServerAdmin           ServerAdmin
	RuntimeConfigAdmin    RuntimeConfigAdmin
	Now                   func() time.Time
	Random                io.Reader
}

type Handler struct {
	repository Repository
	config     Config
}

func New(repository Repository, config Config) (*Handler, error) {
	if repository == nil || config.PasswordPolicy.Validate() != nil || config.AuthThrottleMaxRows < 8 || config.AuthThrottleMaxRows > 100000 {
		return nil, errors.New("invalid web configuration")
	}
	origin, err := auth.CanonicalHTTPSOrigin(config.PublicOrigin)
	if err != nil {
		return nil, errors.New("invalid web configuration")
	}
	config.PublicOrigin = origin
	if config.GitHubSetup != nil {
		u, parseErr := url.Parse(config.GitHubInstallationURL)
		if parseErr != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return nil, errors.New("invalid web configuration")
		}
	}
	if _, err = auth.NewSessionCSRF(config.CSRFKeys, "configuration-check", origin); err != nil {
		return nil, errors.New("invalid web configuration")
	}
	if _, err = config.UsernameThrottleKeys.UsernameDigests("administrator"); err != nil {
		return nil, errors.New("invalid web configuration")
	}
	if err = auth.VerifyDummyPassword("autodeploy-web-dummy", config.DummyPassword); err != nil {
		return nil, errors.New("invalid web configuration")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Handler{repository: repository, config: config}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.securityHeaders(w)
	if r.URL == nil || r.URL.RawPath != "" || r.URL.ForceQuery {
		http.NotFound(w, r)
		return
	}
	if r.URL.RawQuery != "" && r.URL.Path != "/github/setup/callback" && r.URL.Path != "/environments/config" {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case "/github/setup/callback":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.githubCallback(w, r)
	case "/github/setup/complete":
		if r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			h.githubCompleteGet(w, r)
		} else if r.Method == http.MethodPost {
			h.githubCompletePost(w, r)
		} else {
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case "/github/setup":
		if r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.githubSetup(w, r)
	case "/github/bindings":
		if r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.githubBind(w, r)
	case "/github/installations/reconcile":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.githubReconcile(w, r)
	case "/servers":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.serversList(w, r)
	case "/servers/register":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.serversRegister(w, r)
	case "/servers/enrollment":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.serversEnrollment(w, r)
	case "/servers/revoke":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.serversRevoke(w, r)
	case "/environments/mark-reconciled":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.environmentsMarkReconciled(w, r)
	case "/environments/config":
		if h.config.RuntimeConfigAdmin == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.environmentConfigGet(w, r)
		case http.MethodPost:
			h.environmentConfigPost(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case "/environments/bindings/add":
		if h.config.RuntimeConfigAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.environmentBindingAdd(w, r)
	case "/environments/bindings/remove":
		if h.config.RuntimeConfigAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.environmentBindingRemove(w, r)
	case "/livez":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		plain(w, http.StatusOK, "ok\n")
	case "/readyz":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if err := h.ready(r.Context()); err != nil {
			plain(w, http.StatusServiceUnavailable, "unavailable\n")
			return
		}
		plain(w, http.StatusOK, "ok\n")
	case "/login":
		switch r.Method {
		case http.MethodGet:
			h.getLogin(w, r)
		case http.MethodPost:
			h.postLogin(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case "/logout":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.logout(w, r)
	case "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.landing(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) ready(ctx context.Context) error { return h.repository.Ping(ctx) }

func (h *Handler) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.session(r.Context(), r); ok {
		redirect(w, "/")
		return
	}
	envelope, err := h.loginEnvelope(r)
	if err != nil {
		var nonce auth.LoginNonce
		nonce, err = auth.NewLoginNonce(h.config.Random)
		if err != nil {
			plain(w, http.StatusServiceUnavailable, "unavailable\n")
			return
		}
		value, signErr := auth.NewLoginEnvelope(h.config.CSRFKeys, nonce, h.config.Now(), h.config.PublicOrigin)
		if signErr != nil {
			plain(w, http.StatusServiceUnavailable, "unavailable\n")
			return
		}
		http.SetCookie(w, loginCookie(value, h.config.Now()))
		envelope = auth.LoginEnvelope{Nonce: nonce}
	}
	csrf, err := auth.NewLoginCSRF(h.config.CSRFKeys, envelope.Nonce, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	renderLogin(w, csrf)
}

func (h *Handler) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := h.provenance(r); err != nil {
		badRequest(w)
		return
	}
	form, err := parseLoginForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	envelope, err := h.loginEnvelope(r)
	if err != nil || !auth.ValidateLoginCSRF(h.config.CSRFKeys, form.csrf, envelope.Nonce, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	ip, err := h.clientIP(r)
	if err != nil {
		h.reserveInvalidForward(r.Context(), r)
		redirect(w, "/login")
		return
	}
	username, canonicalErr := auth.CanonicalUsername(form.username)
	canonical := canonicalErr == nil
	identities, err := h.identities(ip.String(), username, "", !canonical)
	if err != nil {
		redirect(w, "/login")
		return
	}
	reservation, err := h.repository.ReserveThrottle(r.Context(), identities, h.config.AuthThrottleMaxRows)
	if err != nil || !reservation.Allowed {
		_ = auth.VerifyDummyPassword(form.password, h.config.DummyPassword)
		redirect(w, "/login")
		return
	}
	var user postgres.LoginUser
	findErr := postgres.ErrNotFound
	if canonical {
		user, findErr = h.repository.FindLoginUserByCanonicalUsername(r.Context(), username)
	}
	valid := false
	var verification auth.PasswordVerification
	if canonical && findErr == nil && user.Active {
		verification, err = auth.VerifyPassword(form.password, user.Verifier, h.config.PasswordPolicy)
		valid = err == nil && verification.Valid
	} else {
		err = auth.VerifyDummyPassword(form.password, h.config.DummyPassword)
	}
	if !valid || err != nil {
		redirect(w, "/login")
		return
	}
	identities, err = h.identities(ip.String(), username, user.ID, false)
	if err != nil {
		redirect(w, "/login")
		return
	}
	token, err := auth.NewSessionToken(h.config.Random)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	replacement := (*auth.PasswordVerifier)(nil)
	if verification.RehashNeeded {
		value, hashErr := auth.HashPassword(form.password, h.config.PasswordPolicy, h.config.Random)
		if hashErr != nil {
			plain(w, http.StatusServiceUnavailable, "unavailable\n")
			return
		}
		replacement = &value
	}
	sessionID, err := h.newID()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	auditID, err := h.newID()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	limitAuditID, err := h.newID()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	if _, err = h.repository.CompleteLogin(r.Context(), postgres.CompleteLoginInput{SessionID: sessionID, AuditID: auditID, SessionLimitAuditID: limitAuditID, Token: token, User: user, Replacement: replacement, ThrottleIdentities: identities}); err != nil {
		redirect(w, "/login")
		return
	}
	http.SetCookie(w, sessionCookie(token.String()))
	http.SetCookie(w, expiredLoginCookie())
	redirect(w, "/")
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.provenance(r); err != nil {
		badRequest(w)
		return
	}
	form, err := parseLogoutForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	token, principal, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, form.csrf, principal.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	auditID, err := h.newID()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	if err = h.repository.RevokeSession(r.Context(), token, principal, auditID); err != nil {
		redirect(w, "/login")
		return
	}
	http.SetCookie(w, expiredSessionCookie())
	redirect(w, "/login")
}

func (h *Handler) landing(w http.ResponseWriter, r *http.Request) {
	token, principal, ok := h.session(r.Context(), r)
	_ = token
	if !ok {
		redirect(w, "/login")
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, principal.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = landingTemplate.Execute(w, struct{ CSRF string }{csrf})
}

func (h *Handler) githubSetup(w http.ResponseWriter, r *http.Request) {
	if h.config.GitHubSetup == nil || h.provenance(r) != nil {
		badRequest(w)
		return
	}
	form, err := parseLogoutForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	_, principal, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, form.csrf, principal.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	state, err := h.config.GitHubSetup.Start(r.Context(), principal)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	u, _ := url.Parse(h.config.GitHubInstallationURL)
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	redirect(w, u.String())
}
func (h *Handler) githubCallback(w http.ResponseWriter, r *http.Request) {
	if h.config.GitHubSetup == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if len(q) != 3 || len(r.URL.RawQuery) > 2048 || len(q["state"]) != 1 || len(q["installation_id"]) != 1 || len(q["setup_action"]) != 1 || len(q.Get("state")) != 43 || q.Get("setup_action") != "install" {
		badRequest(w)
		return
	}
	installationID, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
	if err != nil || installationID <= 0 || strconv.FormatInt(installationID, 10) != q.Get("installation_id") {
		badRequest(w)
		return
	}
	handoff, err := h.config.GitHubSetup.Callback(r.Context(), q.Get("state"), installationID, q.Get("setup_action"))
	if err != nil {
		badRequest(w)
		return
	}
	http.SetCookie(w, handoffCookie(handoff, h.config.Now()))
	redirect(w, "/github/setup/complete")
}
func (h *Handler) githubCompleteGet(w http.ResponseWriter, r *http.Request) {
	if h.config.GitHubSetup == nil {
		http.NotFound(w, r)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	c, err := r.Cookie(handoffCookieName)
	if !ok || err != nil || !h.config.GitHubSetup.HandoffValid(r.Context(), c.Value, p) {
		redirect(w, "/login")
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = githubCompleteTemplate.Execute(w, struct{ CSRF string }{csrf})
}
func (h *Handler) githubCompletePost(w http.ResponseWriter, r *http.Request) {
	if h.config.GitHubSetup == nil || h.provenance(r) != nil {
		badRequest(w)
		return
	}
	form, err := parseLogoutForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	c, cerr := r.Cookie(handoffCookieName)
	if !ok || cerr != nil || !auth.ValidateSessionCSRF(h.config.CSRFKeys, form.csrf, p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	if err = h.config.GitHubSetup.Complete(r.Context(), c.Value, p); err != nil {
		badRequest(w)
		return
	}
	http.SetCookie(w, expiredHandoffCookie())
	redirect(w, "/")
}
func (h *Handler) githubBind(w http.ResponseWriter, r *http.Request) {
	if h.config.GitHubSetup == nil || h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 6 || len(v["csrf"]) != 1 || len(v["project_id"]) != 1 || len(v["repository_id"]) != 1 || len(v["ref"]) != 1 || len(v["environment_id"]) != 1 || len(v["server_id"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	id, e := strconv.ParseInt(v.Get("repository_id"), 10, 64)
	input := githubapp.BindingInput{ProjectID: v.Get("project_id"), RepositoryID: id, Ref: v.Get("ref"), EnvironmentID: v.Get("environment_id"), ServerID: v.Get("server_id")}
	if e != nil || id <= 0 || strconv.FormatInt(id, 10) != v.Get("repository_id") || !input.Valid() {
		badRequest(w)
		return
	}
	audit, e := h.newID()
	if e != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	if h.config.GitHubSetup.Bind(r.Context(), p, input, audit) != nil {
		badRequest(w)
		return
	}
	redirect(w, "/")
}
func (h *Handler) githubReconcile(w http.ResponseWriter, r *http.Request) {
	if h.config.GitHubSetup == nil || h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 2 || len(v["csrf"]) != 1 || len(v["installation_id"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	id, err := strconv.ParseInt(v.Get("installation_id"), 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != v.Get("installation_id") || h.config.GitHubSetup.ReconcileInstallation(r.Context(), p, id) != nil {
		badRequest(w)
		return
	}
	redirect(w, "/")
}

// serversList renders the registered-servers list, the registration form,
// and any environments blocked on reconciliation. GET only; it mutates
// nothing.
func (h *Handler) serversList(w http.ResponseWriter, r *http.Request) {
	_, p, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	summaries, err := h.config.ServerAdmin.ListServers(r.Context(), p)
	if err != nil {
		badRequest(w)
		return
	}
	environments, err := h.config.ServerAdmin.ListReconcilingEnvironments(r.Context(), p)
	if err != nil {
		badRequest(w)
		return
	}
	view := serversView{CSRF: csrf}
	for _, s := range summaries {
		view.Servers = append(view.Servers, serverRow{ID: s.ID, Name: s.Name, Status: string(s.Status), CreatedAt: formatTimestamp(s.CreatedAt), LastHeartbeatAt: formatTimestamp(s.LastHeartbeatAt), ActiveCredentialVersion: formatCredentialVersion(s.ActiveCredentialVersion), Enrollment: string(s.Enrollment)})
	}
	for _, e := range environments {
		view.ReconcilingEnvironments = append(view.ReconcilingEnvironments, reconcilingRow{EnvironmentID: e.EnvironmentID, AttemptID: strconv.FormatInt(e.AttemptID, 10), JobID: strconv.FormatInt(e.JobID, 10), ServerID: e.ServerID, Since: formatTimestamp(e.Since)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = serversTemplate.Execute(w, view)
}

// serversRegister mints a fresh server ID and single-use enrollment token
// (crypto/rand via server.NewEnrollmentToken), stores only the token's
// digest, and displays the raw token exactly once. It never redirects with
// the token in a URL and never logs it; the response carries the process-
// wide Cache-Control: no-store header set by securityHeaders above.
func (h *Handler) serversRegister(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 2 || len(v["csrf"]) != 1 || len(v["name"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	name := v.Get("name")
	if !server.ValidName(name) {
		badRequest(w)
		return
	}
	serverID, err := server.NewID(h.config.Random)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	token, digest, err := h.newEnrollmentToken()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	registered, expiresAt, err := h.config.ServerAdmin.RegisterServer(r.Context(), p, postgres.RegisterServerInput{ServerID: serverID, Name: name, ProtocolVersion: server.ProtocolVersion, EnrollmentDigest: digest, EnrollmentTTLSeconds: enrollmentTTLSeconds})
	if err != nil {
		badRequest(w)
		return
	}
	renderEnrollmentToken(w, registered.ID, token, expiresAt)
}

// serversEnrollment reissues a fresh enrollment token for an existing,
// not-yet-enrolled server, under the exact same one-time display rules as
// serversRegister.
func (h *Handler) serversEnrollment(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 2 || len(v["csrf"]) != 1 || len(v["server_id"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	serverID := v.Get("server_id")
	if !github.ValidOpaqueID(serverID) {
		badRequest(w)
		return
	}
	token, digest, err := h.newEnrollmentToken()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	expiresAt, err := h.config.ServerAdmin.IssueEnrollment(r.Context(), p, serverID, digest, enrollmentTTLSeconds)
	if err != nil {
		badRequest(w)
		return
	}
	renderEnrollmentToken(w, serverID, token, expiresAt)
}

// serversRevoke revokes a server and its active credential. Revocation
// requires an explicit confirmation field (a checkbox submitted as
// confirm=revoke) in addition to CSRF and provenance, so a revoke can never
// happen as a side effect of a forged form missing that field.
func (h *Handler) serversRevoke(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 4 || len(v["csrf"]) != 1 || len(v["server_id"]) != 1 || len(v["reason"]) != 1 || len(v["confirm"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	if v.Get("confirm") != "revoke" || !github.ValidOpaqueID(v.Get("server_id")) {
		badRequest(w)
		return
	}
	if h.config.ServerAdmin.RevokeServer(r.Context(), p, v.Get("server_id"), v.Get("reason")) != nil {
		badRequest(w)
		return
	}
	redirect(w, "/servers")
}

// environmentsMarkReconciled is the audited administrator break-glass path
// (ADR 0004 Decision 6) for an environment whose owning server cannot (or
// has not) reconciled it itself.
func (h *Handler) environmentsMarkReconciled(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 3 || len(v["csrf"]) != 1 || len(v["environment_id"]) != 1 || len(v["reason"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	if !github.ValidOpaqueID(v.Get("environment_id")) {
		badRequest(w)
		return
	}
	if h.config.ServerAdmin.MarkReconciled(r.Context(), p, v.Get("environment_id"), v.Get("reason")) != nil {
		badRequest(w)
		return
	}
	redirect(w, "/servers")
}

// validRuntimeConfigFields mirrors migration 000014/000017's
// environment_runtime_configs CHECK constraints exactly (also mirrored,
// independently, by postgres.RuntimeConfigInput.valid -- the repository is
// the authoritative last check; this lets the web layer reject bad input
// with a specific message before ever calling it). Keep both in sync with
// the migration if its bounds ever change. health_gate_path/health_gate_host
// are validated via internal/healthgate, the one shared place that mirror
// lives (unlike the simple numeric bounds here, a regex-shaped rule is not
// safe to hand-duplicate a third time).
func validRuntimeConfigFields(port, drain, cpu int, scheme, strategy, restart, healthGatePath, healthGateHost string, memory int64) bool {
	return port >= 1 && port <= 65535 &&
		(scheme == "http" || scheme == "https") &&
		(strategy == "blue_green" || strategy == "stop_then_start") &&
		drain >= 1 && drain <= 300 &&
		cpu > 0 && cpu <= 256000 &&
		memory > 0 && memory <= 549755813888 &&
		(restart == "unless-stopped" || restart == "on-failure") &&
		healthgate.ValidPath(healthGatePath) &&
		healthgate.ValidHost(healthGateHost)
}

// parseRuntimeConfigForm parses and validates the /environments/config POST
// body's configuration fields, returning a descriptive error (never a bare
// parse failure) so the caller can show the operator exactly what was
// wrong, before any repository call and therefore before any write.
func parseRuntimeConfigForm(v url.Values) (postgres.RuntimeConfigInput, error) {
	port, err1 := strconv.Atoi(v.Get("container_port"))
	drain, err2 := strconv.Atoi(v.Get("drain_seconds"))
	cpu, err3 := strconv.Atoi(v.Get("cpu_millis"))
	memory, err4 := strconv.ParseInt(v.Get("memory_bytes"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return postgres.RuntimeConfigInput{}, errors.New("port, drain seconds, cpu millis, and memory bytes must be whole numbers")
	}
	scheme, strategy, restart := v.Get("container_scheme"), v.Get("release_strategy"), v.Get("restart_policy")
	healthGatePath, healthGateHost := v.Get("health_gate_path"), v.Get("health_gate_host")
	if !validRuntimeConfigFields(port, drain, cpu, scheme, strategy, restart, healthGatePath, healthGateHost, memory) {
		return postgres.RuntimeConfigInput{}, errors.New("container port must be 1-65535, scheme http/https, strategy blue_green/stop_then_start, drain seconds 1-300, cpu millis 1-256000, memory bytes 1-549755813888, restart policy unless-stopped/on-failure, health gate path must be an absolute path with no whitespace/control characters/#, health gate host must be a valid hostname with an optional :port")
	}
	return postgres.RuntimeConfigInput{ContainerPort: port, ContainerScheme: scheme, ReleaseStrategy: strategy, DrainSeconds: drain, CPUMillis: cpu, MemoryBytes: memory, RestartPolicy: restart, HealthGatePath: healthGatePath, HealthGateHost: healthGateHost}, nil
}

// runtimeConfigFormView builds a sticky config view directly from the
// operator's just-submitted form values (not from the database), so a
// rejected submission re-displays exactly what was typed rather than
// discarding it. Numeric fields are carried as the raw submitted text, not
// reformatted, so an invalid (non-numeric) entry is shown back unchanged
// too.
func runtimeConfigFormView(csrf, environmentID string, v url.Values) runtimeConfigView {
	return runtimeConfigView{
		CSRF: csrf, EnvironmentID: environmentID, Revision: v.Get("expected_revision"),
		Port: v.Get("container_port"), Scheme: v.Get("container_scheme"), Strategy: v.Get("release_strategy"),
		Drain: v.Get("drain_seconds"), CPU: v.Get("cpu_millis"), Memory: v.Get("memory_bytes"), Restart: v.Get("restart_policy"),
		HealthGatePath: v.Get("health_gate_path"), HealthGateHost: v.Get("health_gate_host"),
	}
}

// buildRuntimeConfigView renders the stored configuration and bindings
// exactly as read back from the repository (the normal GET path, and the
// fallback view after a rejected mutation once bindings are re-fetched).
func buildRuntimeConfigView(csrf, environmentID string, cfg postgres.RuntimeConfig, bindings []postgres.RuntimeBinding, found bool) runtimeConfigView {
	view := runtimeConfigView{CSRF: csrf, EnvironmentID: environmentID, Exists: found, Bindings: buildBindingRows(bindings)}
	if found {
		view.Revision = strconv.FormatInt(cfg.Revision, 10)
		view.Port = strconv.Itoa(cfg.ContainerPort)
		view.Scheme = cfg.ContainerScheme
		view.Strategy = cfg.ReleaseStrategy
		view.Drain = strconv.Itoa(cfg.DrainSeconds)
		view.CPU = strconv.Itoa(cfg.CPUMillis)
		view.Memory = strconv.FormatInt(cfg.MemoryBytes, 10)
		view.Restart = cfg.RestartPolicy
		view.HealthGatePath = cfg.HealthGatePath
		view.HealthGateHost = cfg.HealthGateHost
		view.HealthPolicyRevision = strconv.Itoa(cfg.HealthPolicyRevision)
		view.UpdatedAt = formatTimestamp(cfg.UpdatedAt)
	} else {
		// Sensible starting defaults for the create form; not stored
		// anywhere until the operator submits them. HealthGateHost has no
		// sensible default (decision D2: required, no fallback), so it is
		// left blank for the operator to fill in.
		view.Scheme, view.Strategy, view.Restart = "http", "stop_then_start", "unless-stopped"
		view.HealthGatePath = "/"
	}
	return view
}

// buildBindingRows never carries a secret value: a secret binding shows
// only the head's identity and current active version number (0/"no active
// version" if the head currently has none -- the same eligibility gap that
// fails Claim). A literal_nonsecret binding's value is shown as-is: ADR
// 0003 classifies it as non-secret protected metadata, not a secret.
func buildBindingRows(bindings []postgres.RuntimeBinding) []bindingRow {
	rows := make([]bindingRow, 0, len(bindings))
	for _, b := range bindings {
		row := bindingRow{VariableName: b.VariableName, Kind: b.BindingKind, Scope: b.DeliveryScope, CreatedAt: formatTimestamp(b.CreatedAt)}
		if b.BindingKind == "secret" {
			if b.SecretVersionNumber > 0 {
				row.Identity = "secret head " + b.SecretHeadID + " (version " + strconv.FormatInt(b.SecretVersionNumber, 10) + ")"
			} else {
				row.Identity = "secret head " + b.SecretHeadID + " (no active version)"
			}
		} else {
			row.Identity = b.LiteralValue
		}
		rows = append(rows, row)
	}
	return rows
}

func renderEnvironmentConfig(w http.ResponseWriter, view runtimeConfigView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = environmentConfigTemplate.Execute(w, view)
}

// environmentConfigGet renders one environment's runtime configuration and
// its bindings. GET is safe/idempotent, matching serversList: no CSRF
// check, only a fresh token minted for the forms this page renders.
func (h *Handler) environmentConfigGet(w http.ResponseWriter, r *http.Request) {
	_, p, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return
	}
	q := r.URL.Query()
	if len(q) != 1 || len(q["environment_id"]) != 1 {
		badRequest(w)
		return
	}
	environmentID := q.Get("environment_id")
	if !github.ValidOpaqueID(environmentID) {
		badRequest(w)
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	cfg, bindings, found, err := h.config.RuntimeConfigAdmin.GetEnvironmentRuntimeConfig(r.Context(), p, environmentID)
	if err != nil {
		badRequest(w)
		return
	}
	renderEnvironmentConfig(w, buildRuntimeConfigView(csrf, environmentID, cfg, bindings, found))
}

// renderConfigRejection re-fetches only the bindings table (never the
// config, which the caller has already populated into submitted with the
// operator's just-rejected values) so the page still shows the current
// binding list beside the sticky, errored configuration form. The
// freshly-read revision replaces submitted.Revision so the operator's next
// attempt starts from a valid CAS value instead of repeating the same
// stale one.
func (h *Handler) renderConfigRejection(w http.ResponseWriter, r *http.Request, p auth.Principal, submitted runtimeConfigView) {
	cfg, bindings, found, err := h.config.RuntimeConfigAdmin.GetEnvironmentRuntimeConfig(r.Context(), p, submitted.EnvironmentID)
	if err != nil {
		badRequest(w)
		return
	}
	submitted.Exists = found
	submitted.Bindings = buildBindingRows(bindings)
	if found {
		submitted.Revision = strconv.FormatInt(cfg.Revision, 10)
	}
	renderEnvironmentConfig(w, submitted)
}

// environmentConfigPost creates (expected_revision empty) or CAS-updates
// (expected_revision set) one environment's runtime configuration. Every
// field is validated before any repository call; a rejection at any stage
// re-displays the operator's submitted values rather than discarding them.
func (h *Handler) environmentConfigPost(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 12 {
		badRequest(w)
		return
	}
	for _, name := range []string{"environment_id", "csrf", "expected_revision", "container_port", "container_scheme", "release_strategy", "drain_seconds", "cpu_millis", "memory_bytes", "restart_policy", "health_gate_path", "health_gate_host"} {
		if len(v[name]) != 1 {
			badRequest(w)
			return
		}
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	environmentID := v.Get("environment_id")
	if !github.ValidOpaqueID(environmentID) {
		badRequest(w)
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	submitted := runtimeConfigFormView(csrf, environmentID, v)

	in, parseErr := parseRuntimeConfigForm(v)
	if parseErr != nil {
		submitted.ConfigError = parseErr.Error()
		h.renderConfigRejection(w, r, p, submitted)
		return
	}

	revisionRaw := v.Get("expected_revision")
	var opErr error
	if revisionRaw == "" {
		_, opErr = h.config.RuntimeConfigAdmin.CreateEnvironmentRuntimeConfig(r.Context(), p, environmentID, in)
	} else {
		expected, convErr := strconv.ParseInt(revisionRaw, 10, 64)
		if convErr != nil || expected < 0 {
			submitted.ConfigError = "invalid expected revision"
			h.renderConfigRejection(w, r, p, submitted)
			return
		}
		_, opErr = h.config.RuntimeConfigAdmin.UpdateEnvironmentRuntimeConfig(r.Context(), p, environmentID, in, expected)
	}
	if opErr != nil {
		switch {
		case errors.Is(opErr, postgres.ErrEnvironmentRuntimeConfigConflict):
			submitted.ConfigError = "this configuration changed underneath you since the page was loaded; the values below have been refreshed -- review and resubmit"
		case errors.Is(opErr, postgres.ErrEnvironmentRuntimeConfigNotFound):
			submitted.ConfigError = "this configuration no longer exists; reload the page"
		case errors.Is(opErr, postgres.ErrEnvironmentRuntimeConfigAlreadyExists):
			submitted.ConfigError = "a configuration already exists for this environment; reload the page and edit it instead"
		default:
			submitted.ConfigError = "configuration rejected: invalid values, or you are not authorized to configure this environment"
		}
		h.renderConfigRejection(w, r, p, submitted)
		return
	}
	redirect(w, "/environments/config?environment_id="+url.QueryEscape(environmentID))
}

// parseBindingForm parses the /environments/bindings/add POST body.
func parseBindingForm(v url.Values) postgres.BindingInput {
	return postgres.BindingInput{
		VariableName:  v.Get("variable_name"),
		BindingKind:   v.Get("binding_kind"),
		DeliveryScope: v.Get("delivery_scope"),
		SecretHeadID:  v.Get("secret_head_id"),
		LiteralValue:  v.Get("literal_value"),
	}
}

// environmentBindingAdd adds one variable binding. A rejection (invalid
// shape, or the schema's bind-time scope trigger refusing a cross-
// environment/scope-mismatched/provider-purpose secret head) re-displays
// the configuration page with the current configuration, the current
// bindings, and the operator's just-submitted (rejected) binding fields
// still filled in.
func (h *Handler) environmentBindingAdd(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 7 {
		badRequest(w)
		return
	}
	for _, name := range []string{"environment_id", "csrf", "variable_name", "binding_kind", "delivery_scope"} {
		if len(v[name]) != 1 {
			badRequest(w)
			return
		}
	}
	if len(v["secret_head_id"]) != 1 || len(v["literal_value"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	environmentID := v.Get("environment_id")
	if !github.ValidOpaqueID(environmentID) {
		badRequest(w)
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	in := parseBindingForm(v)
	_, opErr := h.config.RuntimeConfigAdmin.AddEnvironmentRuntimeBinding(r.Context(), p, environmentID, in)
	if opErr != nil {
		cfg, bindings, found, err := h.config.RuntimeConfigAdmin.GetEnvironmentRuntimeConfig(r.Context(), p, environmentID)
		if err != nil {
			badRequest(w)
			return
		}
		view := buildRuntimeConfigView(csrf, environmentID, cfg, bindings, found)
		view.BindVariable, view.BindKind, view.BindScope, view.BindSecretHeadID, view.BindLiteralValue = in.VariableName, in.BindingKind, in.DeliveryScope, in.SecretHeadID, in.LiteralValue
		switch {
		case errors.Is(opErr, postgres.ErrEnvironmentRuntimeBindingInvalid):
			view.BindingError = "binding rejected: the secret head does not exist, belongs to a different environment or delivery scope, is a provider-purpose head, or the variable is already bound"
		default:
			view.BindingError = "binding rejected: invalid values, or you are not authorized to configure this environment"
		}
		renderEnvironmentConfig(w, view)
		return
	}
	redirect(w, "/environments/config?environment_id="+url.QueryEscape(environmentID))
}

// environmentBindingRemove deletes one variable binding.
func (h *Handler) environmentBindingRemove(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 3 || len(v["csrf"]) != 1 || len(v["environment_id"]) != 1 || len(v["variable_name"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	environmentID := v.Get("environment_id")
	if !github.ValidOpaqueID(environmentID) {
		badRequest(w)
		return
	}
	if err := h.config.RuntimeConfigAdmin.RemoveEnvironmentRuntimeBinding(r.Context(), p, environmentID, v.Get("variable_name")); err != nil {
		badRequest(w)
		return
	}
	redirect(w, "/environments/config?environment_id="+url.QueryEscape(environmentID))
}

// newEnrollmentToken mints a fresh single-use enrollment token and its
// digest together, so every caller mints and digests it the same way and
// the raw token never touches anything but the one response that displays
// it.
func (h *Handler) newEnrollmentToken() (string, [32]byte, error) {
	token, err := server.NewEnrollmentToken(h.config.Random)
	if err != nil {
		return "", [32]byte{}, err
	}
	digest, err := server.EnrollmentTokenDigest(token)
	if err != nil {
		return "", [32]byte{}, err
	}
	return token.String(), digest, nil
}

func (h *Handler) session(ctx context.Context, r *http.Request) (auth.SessionToken, auth.Principal, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", auth.Principal{}, false
	}
	token, err := auth.ParseSessionToken(cookie.Value)
	if err != nil {
		return "", auth.Principal{}, false
	}
	p, err := h.repository.ValidateSession(ctx, token)
	if err != nil {
		return "", auth.Principal{}, false
	}
	return token, p, true
}
func (h *Handler) loginEnvelope(r *http.Request) (auth.LoginEnvelope, error) {
	c, err := r.Cookie(loginCookieName)
	if err != nil {
		return auth.LoginEnvelope{}, err
	}
	return auth.ParseLoginEnvelope(h.config.CSRFKeys, c.Value, h.config.Now(), h.config.PublicOrigin)
}

func (h *Handler) identities(ip, username, userID string, malformedUsername bool) ([]postgres.ThrottleIdentity, error) {
	keys := make([]string, 0, len(h.config.CSRFKeys.Keys))
	for version := range h.config.CSRFKeys.Keys {
		keys = append(keys, version)
	}
	sort.Strings(keys)
	primary := h.config.CSRFKeys.ActiveVersion
	pairDomain, pairValue := "pair", username
	if malformedUsername {
		pairDomain, pairValue = "invalid-username-pair", "malformed"
	}
	pairAliases, ipAliases := []postgres.ThrottleAlias{}, []postgres.ThrottleAlias{}
	for _, version := range keys {
		if version == primary {
			continue
		}
		digest, err := auth.ThrottleDigest(h.config.CSRFKeys.Keys[version], pairDomain, ip, pairValue)
		if err != nil {
			return nil, err
		}
		pairAliases = append(pairAliases, postgres.ThrottleAlias{KeyVersion: version, Digest: digest})
		ipDigest, err := auth.ThrottleDigest(h.config.CSRFKeys.Keys[version], "ip", ip)
		if err != nil {
			return nil, err
		}
		ipAliases = append(ipAliases, postgres.ThrottleAlias{KeyVersion: version, Digest: ipDigest})
	}
	pairDigest, err := auth.ThrottleDigest(h.config.CSRFKeys.Keys[primary], pairDomain, ip, pairValue)
	if err != nil {
		return nil, err
	}
	ipDigest, err := auth.ThrottleDigest(h.config.CSRFKeys.Keys[primary], "ip", ip)
	if err != nil {
		return nil, err
	}
	var usernameIdentity postgres.ThrottleIdentity
	if malformedUsername {
		digest, digestErr := auth.ThrottleDigest(h.config.CSRFKeys.Keys[primary], "invalid-username", "malformed")
		if digestErr != nil {
			return nil, digestErr
		}
		aliases := make([]postgres.ThrottleAlias, 0, len(keys)-1)
		for _, version := range keys {
			if version != primary {
				value, digestErr := auth.ThrottleDigest(h.config.CSRFKeys.Keys[version], "invalid-username", "malformed")
				if digestErr != nil {
					return nil, digestErr
				}
				aliases = append(aliases, postgres.ThrottleAlias{KeyVersion: version, Digest: value})
			}
		}
		usernameIdentity = postgres.ThrottleIdentity{Kind: postgres.ThrottleUsername, KeyVersion: primary, Digest: digest, Aliases: aliases}
	} else {
		usernameDigests, digestErr := h.config.UsernameThrottleKeys.UsernameDigests(username)
		if digestErr != nil {
			return nil, digestErr
		}
		usernameAliases := make([]postgres.ThrottleAlias, 0, len(usernameDigests)-1)
		for index, digest := range usernameDigests[1:] {
			usernameAliases = append(usernameAliases, postgres.ThrottleAlias{KeyVersion: h.config.UsernameThrottleKeys.RetainedVersions[index], Digest: digest})
		}
		usernameIdentity = postgres.ThrottleIdentity{Kind: postgres.ThrottleUsername, KeyVersion: h.config.UsernameThrottleKeys.ActiveVersion, Digest: usernameDigests[0], Aliases: usernameAliases}
	}
	return []postgres.ThrottleIdentity{{Kind: postgres.ThrottlePair, KeyVersion: primary, Digest: pairDigest, Aliases: pairAliases, RecoveryUserID: userID}, usernameIdentity, {Kind: postgres.ThrottleIP, KeyVersion: primary, Digest: ipDigest, Aliases: ipAliases}}, nil
}
func (h *Handler) reserveInvalidForward(ctx context.Context, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return
	}
	peer = peer.Unmap()
	if !h.trusted(peer) {
		return
	}
	primary := h.config.CSRFKeys.ActiveVersion
	d, err := auth.ThrottleDigest(h.config.CSRFKeys.Keys[primary], "invalid-forward", peer.String())
	if err != nil {
		return
	}
	versions := make([]string, 0, len(h.config.CSRFKeys.Keys))
	for version := range h.config.CSRFKeys.Keys {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	aliases := make([]postgres.ThrottleAlias, 0, len(versions)-1)
	for _, version := range versions {
		if version == primary {
			continue
		}
		value, digestErr := auth.ThrottleDigest(h.config.CSRFKeys.Keys[version], "invalid-forward", peer.String())
		if digestErr != nil {
			return
		}
		aliases = append(aliases, postgres.ThrottleAlias{KeyVersion: version, Digest: value})
	}
	_, _ = h.repository.ReserveThrottle(ctx, []postgres.ThrottleIdentity{{Kind: postgres.ThrottleInvalidForward, KeyVersion: primary, Digest: d, Aliases: aliases}}, h.config.AuthThrottleMaxRows)
}
func (h *Handler) clientIP(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, errBadRequest
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, errBadRequest
	}
	peer = peer.Unmap()
	trusted := h.trusted(peer)
	xff := r.Header.Values("X-Forwarded-For")
	if !trusted {
		return peer, nil
	}
	if len(xff) != 1 {
		return netip.Addr{}, errBadRequest
	}
	values := strings.Split(xff[0], ",")
	if len(values) == 0 || len(values) > maxForwardedFor {
		return netip.Addr{}, errBadRequest
	}
	chain := make([]netip.Addr, len(values))
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return netip.Addr{}, errBadRequest
		}
		addr, parseErr := netip.ParseAddr(value)
		if parseErr != nil || addr.Zone() != "" || addr.String() != value {
			return netip.Addr{}, errBadRequest
		}
		chain[i] = addr.Unmap()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !h.trusted(chain[i]) {
			return chain[i], nil
		}
	}
	return netip.Addr{}, errBadRequest
}
func (h *Handler) trusted(address netip.Addr) bool {
	for _, prefix := range h.config.TrustedProxyCIDRs {
		if prefix.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}
func (h *Handler) provenance(r *http.Request) error {
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return errBadRequest
	}
	if len(origins) == 1 {
		origin := origins[0]
		if origin == "" || origin != h.config.PublicOrigin {
			return errBadRequest
		}
		value, err := auth.CanonicalHTTPSOrigin(origin)
		if err != nil || value != h.config.PublicOrigin {
			return errBadRequest
		}
		return nil
	}
	referers := r.Header.Values("Referer")
	if len(referers) != 1 {
		return errBadRequest
	}
	referer := referers[0]
	u, err := url.Parse(referer)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Scheme+"://"+u.Host != h.config.PublicOrigin {
		return errBadRequest
	}
	value, err := auth.CanonicalHTTPSOrigin(u.Scheme + "://" + u.Host)
	if err != nil || value != h.config.PublicOrigin {
		return errBadRequest
	}
	return nil
}
func parseLoginForm(r *http.Request) (struct{ username, password, csrf string }, error) {
	var result struct{ username, password, csrf string }
	mediaType := r.Header.Get("Content-Type")
	if mediaType != "application/x-www-form-urlencoded" {
		return result, errBadRequest
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return result, errBadRequest
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 3 {
		return result, errBadRequest
	}
	for _, name := range []string{"username", "password", "csrf"} {
		value, ok := values[name]
		if !ok || len(value) != 1 {
			return result, errBadRequest
		}
	}
	result.username, result.password, result.csrf = values.Get("username"), values.Get("password"), values.Get("csrf")
	if len(result.username) > 1024 || len(result.password) > 1024 || len(result.csrf) > 256 {
		return result, errBadRequest
	}
	return result, nil
}
func parseLogoutForm(r *http.Request) (struct{ csrf string }, error) {
	var result struct{ csrf string }
	values, err := parseURLForm(r)
	if err != nil || len(values) != 1 {
		return result, errBadRequest
	}
	value, ok := values["csrf"]
	if !ok || len(value) != 1 || len(value[0]) > 256 {
		return result, errBadRequest
	}
	result.csrf = value[0]
	return result, nil
}
func parseURLForm(r *http.Request) (url.Values, error) {
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return nil, errBadRequest
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return nil, errBadRequest
	}
	return url.ParseQuery(string(body))
}
func (h *Handler) newID() (string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(h.config.Random, value); err != nil {
		return "", err
	}
	return "w1_" + base64.RawURLEncoding.EncodeToString(value), nil
}
func (h *Handler) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
func loginCookie(value string, now time.Time) *http.Cookie {
	return &http.Cookie{Name: loginCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(auth.LoginEnvelopeMaxAge.Seconds()), Expires: now.Add(auth.LoginEnvelopeMaxAge)}
}
func sessionCookie(value string) *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}
func expiredLoginCookie() *http.Cookie {
	return &http.Cookie{Name: loginCookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1}
}
func expiredSessionCookie() *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1}
}
func handoffCookie(value string, now time.Time) *http.Cookie {
	return &http.Cookie{Name: handoffCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 600, Expires: now.Add(10 * time.Minute)}
}
func expiredHandoffCookie() *http.Cookie {
	return &http.Cookie{Name: handoffCookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1}
}
func redirect(w http.ResponseWriter, target string) {
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
}
func plain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
func badRequest(w http.ResponseWriter) { plain(w, http.StatusBadRequest, "bad request\n") }
func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>AutoDeploy sign in</title></head><body><main><h1>AutoDeploy</h1><form method="post" action="/login"><label>Username <input name="username" autocomplete="username" required></label><label>Password <input name="password" type="password" autocomplete="current-password" required></label><input type="hidden" name="csrf" value="{{.}}"><button type="submit">Sign in</button></form></main></body></html>`))
var landingTemplate = template.Must(template.New("landing").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>AutoDeploy</title></head><body><main><h1>AutoDeploy</h1><p>Control plane ready. Projects, environments, and deployments will appear here.</p><p><a href="/servers">Servers</a></p><form method="post" action="/github/installations/reconcile"><label>GitHub installation ID <input name="installation_id" inputmode="numeric" required></label><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Refresh GitHub repositories</button></form><form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Sign out</button></form></main></body></html>`))
var githubCompleteTemplate = template.Must(template.New("github-complete").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Confirm GitHub App</title></head><body><main><h1>Confirm GitHub App installation</h1><form method="post" action="/github/setup/complete"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Confirm</button></form></main></body></html>`))

func renderLogin(w http.ResponseWriter, csrf string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTemplate.Execute(w, csrf)
}

// serverRow, reconcilingRow, and serversView are pre-formatted (all
// timestamps already rendered to text) so serversTemplate itself stays free
// of template functions or logic, matching loginTemplate/landingTemplate/
// githubCompleteTemplate above. html/template auto-escapes every field, so a
// server name containing markup is rendered inert.
type serverRow struct {
	ID, Name, Status, CreatedAt, LastHeartbeatAt, ActiveCredentialVersion, Enrollment string
}
type reconcilingRow struct {
	EnvironmentID, AttemptID, JobID, ServerID, Since string
}
type serversView struct {
	CSRF                    string
	Servers                 []serverRow
	ReconcilingEnvironments []reconcilingRow
}

// formatTimestamp renders a possibly-zero time.Time as UTC RFC 3339, or "" -
// e.g. a server with no heartbeat yet.
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// formatCredentialVersion renders "" for a server with no active credential
// (revoked, or enrolled but somehow uncredentialed) instead of "0".
func formatCredentialVersion(version int) string {
	if version == 0 {
		return ""
	}
	return strconv.Itoa(version)
}

// renderEnrollmentToken displays a freshly minted enrollment token exactly
// once: this is the only place the raw token value is ever written, and it
// goes only into this response body -- never a redirect Location, never a
// log. Cache-Control: no-store is already set process-wide by
// securityHeaders above.
func renderEnrollmentToken(w http.ResponseWriter, serverID, token string, expiresAt time.Time) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = enrollmentTokenTemplate.Execute(w, struct{ ServerID, Token, ExpiresAt string }{serverID, token, formatTimestamp(expiresAt)})
}

// serversTemplate lists registered servers with a registration form, a
// reissue-enrollment and revoke form per server, and reconciling
// environments with a mark-reconciled form per environment. Rotate is
// intentionally absent here: Step 6 Decision 4 changed credential issuance
// so the agent, not the control plane, generates its own credential and
// sends only its digest at enrollment (internal/agent/http Enroll); a
// control-plane "rotate" can no longer hand back a usable credential.
// Revoke-and-re-enroll is not a substitute either, since IssueEnrollment is
// blocked once a server's enrollment token has been consumed (schema
// 000012). Rotation is deferred to a follow-up agent-initiated rotation
// protocol; this slice ships register, reissue-before-enrollment, and
// revoke only.
var serversTemplate = template.Must(template.New("servers").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>AutoDeploy servers</title></head><body><main><h1>Servers</h1><form method="post" action="/servers/register"><label>Name <input name="name" required maxlength="128"></label><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Register server</button></form><table><thead><tr><th>ID</th><th>Name</th><th>Status</th><th>Created</th><th>Last heartbeat</th><th>Active credential</th><th>Enrollment</th><th>Actions</th></tr></thead><tbody>{{range .Servers}}<tr><td>{{.ID}}</td><td>{{.Name}}</td><td>{{.Status}}</td><td>{{.CreatedAt}}</td><td>{{.LastHeartbeatAt}}</td><td>{{.ActiveCredentialVersion}}</td><td>{{.Enrollment}}</td><td><form method="post" action="/servers/enrollment"><input type="hidden" name="server_id" value="{{.ID}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Reissue enrollment token</button></form><form method="post" action="/servers/revoke"><input type="hidden" name="server_id" value="{{.ID}}"><label>Reason <input name="reason" required maxlength="512"></label><label><input type="checkbox" name="confirm" value="revoke" required> Confirm revoke</label><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Revoke</button></form></td></tr>{{end}}</tbody></table><h2>Reconciling environments</h2><table><thead><tr><th>Environment</th><th>Attempt</th><th>Job</th><th>Server</th><th>Since</th><th>Actions</th></tr></thead><tbody>{{range .ReconcilingEnvironments}}<tr><td>{{.EnvironmentID}}</td><td>{{.AttemptID}}</td><td>{{.JobID}}</td><td>{{.ServerID}}</td><td>{{.Since}}</td><td><form method="post" action="/environments/mark-reconciled"><input type="hidden" name="environment_id" value="{{.EnvironmentID}}"><label>Reason <input name="reason" required maxlength="512"></label><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Mark reconciled</button></form></td></tr>{{end}}</tbody></table><h2>Runtime configuration</h2><form method="get" action="/environments/config"><label>Environment ID <input name="environment_id" required maxlength="128"></label><button type="submit">View / edit configuration</button></form><p><a href="/">Back</a></p></main></body></html>`))

// runtimeConfigView and bindingRow are pre-formatted the same way
// serverRow/reconcilingRow are: every field is already rendered to text
// (including a rejected submission's raw, possibly-invalid input), so
// environmentConfigTemplate stays free of template functions or logic.
// html/template auto-escapes every field.
type bindingRow struct {
	VariableName, Kind, Scope, Identity, CreatedAt string
}
type runtimeConfigView struct {
	CSRF, EnvironmentID                                                   string
	Exists                                                                bool
	Revision                                                              string
	Port, Scheme, Strategy, Drain, CPU, Memory, Restart                   string
	HealthGatePath, HealthGateHost                                        string
	HealthPolicyRevision, UpdatedAt                                       string
	ConfigError                                                           string
	Bindings                                                              []bindingRow
	BindingError                                                          string
	BindVariable, BindKind, BindScope, BindSecretHeadID, BindLiteralValue string
}

// environmentConfigTemplate renders one environment's runtime configuration
// form (create or CAS-update, depending on whether Exists is true) and its
// variable bindings with add/remove forms. Plain HTML, no JavaScript, no
// design system (deferred to the operations UI milestone) -- matches
// serversTemplate's own conventions exactly.
var environmentConfigTemplate = template.Must(template.New("environment-config").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Environment runtime configuration</title></head><body><main><h1>Runtime configuration: {{.EnvironmentID}}</h1>{{if .ConfigError}}<p><strong>{{.ConfigError}}</strong></p>{{end}}{{if not .Exists}}<p>No configuration exists yet for this environment. Fill in the form below to create one.</p>{{end}}<form method="post" action="/environments/config"><input type="hidden" name="environment_id" value="{{.EnvironmentID}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="expected_revision" value="{{.Revision}}"><label>Container port <input name="container_port" required maxlength="5" value="{{.Port}}"></label><label>Container scheme <select name="container_scheme"><option value="http" {{if eq .Scheme "http"}}selected{{end}}>http</option><option value="https" {{if eq .Scheme "https"}}selected{{end}}>https</option></select></label><label>Release strategy <select name="release_strategy"><option value="blue_green" {{if eq .Strategy "blue_green"}}selected{{end}}>blue_green</option><option value="stop_then_start" {{if eq .Strategy "stop_then_start"}}selected{{end}}>stop_then_start</option></select></label><label>Drain seconds <input name="drain_seconds" required maxlength="3" value="{{.Drain}}"></label><label>CPU millis <input name="cpu_millis" required maxlength="6" value="{{.CPU}}"></label><label>Memory bytes <input name="memory_bytes" required maxlength="12" value="{{.Memory}}"></label><label>Restart policy <select name="restart_policy"><option value="unless-stopped" {{if eq .Restart "unless-stopped"}}selected{{end}}>unless-stopped</option><option value="on-failure" {{if eq .Restart "on-failure"}}selected{{end}}>on-failure</option></select></label><label>Health gate path <input name="health_gate_path" required maxlength="2048" value="{{.HealthGatePath}}"></label><label>Health gate host <input name="health_gate_host" required maxlength="269" value="{{.HealthGateHost}}"></label><button type="submit">{{if .Exists}}Save configuration{{else}}Create configuration{{end}}</button></form><h2>Variable bindings</h2>{{if .BindingError}}<p><strong>{{.BindingError}}</strong></p>{{end}}<table><thead><tr><th>Variable</th><th>Kind</th><th>Scope</th><th>Value / identity</th><th>Created</th><th>Actions</th></tr></thead><tbody>{{range .Bindings}}<tr><td>{{.VariableName}}</td><td>{{.Kind}}</td><td>{{.Scope}}</td><td>{{.Identity}}</td><td>{{.CreatedAt}}</td><td><form method="post" action="/environments/bindings/remove"><input type="hidden" name="environment_id" value="{{$.EnvironmentID}}"><input type="hidden" name="variable_name" value="{{.VariableName}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Remove</button></form></td></tr>{{end}}</tbody></table><h3>Add binding</h3><form method="post" action="/environments/bindings/add"><input type="hidden" name="environment_id" value="{{.EnvironmentID}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>Variable name <input name="variable_name" required maxlength="128" value="{{.BindVariable}}"></label><label>Kind <select name="binding_kind"><option value="secret" {{if eq .BindKind "secret"}}selected{{end}}>secret</option><option value="literal_nonsecret" {{if eq .BindKind "literal_nonsecret"}}selected{{end}}>literal_nonsecret</option></select></label><label>Delivery scope <select name="delivery_scope"><option value="runtime" {{if eq .BindScope "runtime"}}selected{{end}}>runtime</option><option value="build" {{if eq .BindScope "build"}}selected{{end}}>build</option></select></label><label>Secret head ID (kind=secret only) <input name="secret_head_id" maxlength="128" value="{{.BindSecretHeadID}}"></label><label>Literal value (kind=literal_nonsecret only) <input name="literal_value" maxlength="4096" value="{{.BindLiteralValue}}"></label><button type="submit">Add binding</button></form><p><a href="/servers">Back to servers</a></p></main></body></html>`))

// enrollmentTokenTemplate is rendered exactly once per register/reissue
// response: the raw enrollment token appears here and nowhere else.
var enrollmentTokenTemplate = template.Must(template.New("server-enrollment-token").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Server enrollment token</title></head><body><main><h1>Enrollment token issued</h1><p>This token is shown once and cannot be retrieved again. Copy it now.</p><p>Server ID: {{.ServerID}}</p><p>Token: {{.Token}}</p><p>Expires: {{.ExpiresAt}}</p><p>On the target host, run the agent enrollment command with this server ID and this token before it expires. If it expires or is lost, reissue a new one from the servers page.</p><p><a href="/servers">Back to servers</a></p></main></body></html>`))
