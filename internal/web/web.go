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
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
)

const (
	loginCookieName   = "__Host-ad_login"
	sessionCookieName = "__Host-ad_session"
	handoffCookieName = "__Host-ad_github_handoff"
	maxBodyBytes      = 8 << 10
	maxForwardedFor   = 32
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
	if r.URL.RawQuery != "" && r.URL.Path != "/github/setup/callback" {
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
var landingTemplate = template.Must(template.New("landing").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>AutoDeploy</title></head><body><main><h1>AutoDeploy</h1><p>Control plane ready. Projects, environments, and deployments will appear here.</p><form method="post" action="/github/installations/reconcile"><label>GitHub installation ID <input name="installation_id" inputmode="numeric" required></label><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Refresh GitHub repositories</button></form><form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Sign out</button></form></main></body></html>`))
var githubCompleteTemplate = template.Must(template.New("github-complete").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Confirm GitHub App</title></head><body><main><h1>Confirm GitHub App installation</h1><form method="post" action="/github/setup/complete"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">Confirm</button></form></main></body></html>`))

func renderLogin(w http.ResponseWriter, csrf string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTemplate.Execute(w, csrf)
}
