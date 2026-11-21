package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// SetupStore is the narrow persistence surface of the browser first-run
// administrator setup (ADR 0006 amendment, 2026-11-21). A nil SetupStore
// disables /setup entirely (same nil-disables convention as the other
// optional surfaces): it 404s.
type SetupStore interface {
	// SetupAvailable reports whether no administrator exists yet.
	SetupAvailable(context.Context) (bool, error)
	// SetupCodeLive reports whether digest matches a live (unexpired,
	// unconsumed) code. A cheap pre-check so a bad code never costs an Argon2
	// hash; the transaction re-verifies and consumes under the row lock.
	SetupCodeLive(context.Context, [32]byte) (bool, error)
	// BootstrapWithSetupToken verifies and consumes the setup code and creates
	// the administrator in one transaction.
	BootstrapWithSetupToken(context.Context, [32]byte, postgres.BootstrapInput) error
}

// setupHashSlots bounds concurrent password hashes on /setup (each is a
// memory-hard Argon2 run); excess requests get a neutral 503.
const setupHashSlots = 2

// setupThrottleDomain is the throttle digest domain for /setup attempts.
// Throttling is per client IP only (no global bucket an attacker could fill
// to lock the real operator out). The domain is its own HMAC namespace, so it
// can never collide with a login IP/pair/username bucket.
const setupThrottleDomain = "setup-ip"

const (
	setupErrorCode     = "That setup code is not valid or has expired. Run autodeploy-run admin setup-token on the host for a new one."
	setupErrorMismatch = "The passwords do not match."
	setupErrorPassword = "Choose a password of 15 to 1024 characters."
	setupErrorUsername = "Username must be 3 to 64 characters: lowercase letters, digits, dot, underscore or hyphen, starting with a letter or digit."
	setupErrorThrottle = "Too many attempts. Wait a few minutes and try again."
)

type setupView struct {
	CSRF  string
	Error string
}

// setupOpen reports whether /setup should answer. Anything else (disabled,
// already bootstrapped) is indistinguishable from an unknown path. A store
// failure answers 503 and returns false as well.
func (h *Handler) setupOpen(w http.ResponseWriter, r *http.Request) bool {
	if h.config.Setup == nil {
		http.NotFound(w, r)
		return false
	}
	open, err := h.config.Setup.SetupAvailable(r.Context())
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return false
	}
	if !open {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (h *Handler) getSetup(w http.ResponseWriter, r *http.Request) {
	if !h.setupOpen(w, r) {
		return
	}
	csrf, err := h.issueLoginCSRF(w, r)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	renderSetup(w, http.StatusOK, setupView{CSRF: csrf})
}

func (h *Handler) postSetup(w http.ResponseWriter, r *http.Request) {
	if !h.setupOpen(w, r) {
		return
	}
	if err := h.provenance(r); err != nil {
		badRequest(w)
		return
	}
	form, err := parseSetupForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	envelope, err := h.loginEnvelope(r)
	if err != nil || !auth.ValidateLoginCSRF(h.config.CSRFKeys, form.csrf, envelope.Nonce, h.config.PublicOrigin) {
		redirect(w, "/setup")
		return
	}
	// Re-issue the form (fresh CSRF from the same envelope) with an error.
	fail := func(status int, message string) {
		csrf, csrfErr := auth.NewLoginCSRF(h.config.CSRFKeys, envelope.Nonce, h.config.PublicOrigin)
		if csrfErr != nil {
			plain(w, http.StatusServiceUnavailable, "unavailable\n")
			return
		}
		renderSetup(w, status, setupView{CSRF: csrf, Error: message})
	}
	ip, err := h.clientIP(r)
	if err != nil {
		h.reserveInvalidForward(r.Context(), r)
		redirect(w, "/setup")
		return
	}
	identities, err := h.setupIdentities(ip.String())
	if err != nil {
		redirect(w, "/setup")
		return
	}
	reservation, err := h.repository.ReserveThrottle(r.Context(), identities, h.config.AuthThrottleMaxRows)
	if err != nil {
		storeFailure(w, err, "unavailable\n")
		return
	}
	if !reservation.Allowed {
		w.Header().Set("Retry-After", "300")
		fail(http.StatusTooManyRequests, setupErrorThrottle)
		return
	}
	username, err := auth.CanonicalUsername(form.username)
	if err != nil {
		fail(http.StatusUnprocessableEntity, setupErrorUsername)
		return
	}
	if form.password != form.confirm {
		fail(http.StatusUnprocessableEntity, setupErrorMismatch)
		return
	}
	if auth.ValidatePassword(form.password) != nil {
		fail(http.StatusUnprocessableEntity, setupErrorPassword)
		return
	}
	// Check the code before any hashing: a bad code costs one indexed read,
	// never an Argon2 run. Wrong, malformed, expired and consumed codes all
	// answer with the same uniform 403.
	token, err := auth.ParseSetupToken(form.code)
	if err != nil {
		fail(http.StatusForbidden, setupErrorCode)
		return
	}
	digest, err := auth.SetupTokenDigest(token)
	if err != nil {
		fail(http.StatusForbidden, setupErrorCode)
		return
	}
	live, err := h.config.Setup.SetupCodeLive(r.Context(), digest)
	if err != nil {
		storeFailure(w, err, "unavailable\n")
		return
	}
	if !live {
		fail(http.StatusForbidden, setupErrorCode)
		return
	}
	select {
	case h.setupSlots <- struct{}{}:
		defer func() { <-h.setupSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		plain(w, http.StatusServiceUnavailable, "unavailable; retry\n")
		return
	}
	policy, dummy := h.passwordPolicy(r.Context())
	verifier, err := h.hashPassword(form.password, policy, h.config.Random)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	ownerID, userID, auditID := "", "", ""
	for _, id := range []*string{&ownerID, &userID, &auditID} {
		if *id, err = h.newID(); err != nil {
			plain(w, http.StatusServiceUnavailable, "unavailable\n")
			return
		}
	}
	if ownerID == userID || userID == auditID || ownerID == auditID {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	err = h.config.Setup.BootstrapWithSetupToken(r.Context(), digest, postgres.BootstrapInput{OwnerID: ownerID, UserID: userID, Username: username, AuditID: auditID, Verifier: verifier, Policy: policy})
	switch {
	case err == nil:
		h.policyPersisted(policy, dummy)
		http.SetCookie(w, expiredLoginCookie())
		redirect(w, "/login?setup=done")
	case errors.Is(err, postgres.ErrSetupCodeInvalid):
		fail(http.StatusForbidden, setupErrorCode)
	case errors.Is(err, postgres.ErrAlreadyBootstrapped):
		// Lost a race (or setup closed meanwhile): the route is gone.
		http.NotFound(w, r)
	default:
		storeFailure(w, err, "setup failed\n")
	}
}

type setupForm struct{ code, username, password, confirm, csrf string }

func parseSetupForm(r *http.Request) (setupForm, error) {
	var result setupForm
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return result, errBadRequest
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return result, errBadRequest
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 5 {
		return result, errBadRequest
	}
	for _, name := range []string{"code", "username", "password", "confirm", "csrf"} {
		if value, ok := values[name]; !ok || len(value) != 1 {
			return result, errBadRequest
		}
	}
	result = setupForm{code: values.Get("code"), username: values.Get("username"), password: values.Get("password"), confirm: values.Get("confirm"), csrf: values.Get("csrf")}
	if len(result.code) > 256 || len(result.username) > 1024 || len(result.password) > 1024 || len(result.confirm) > 1024 || len(result.csrf) > 256 {
		return result, errBadRequest
	}
	return result, nil
}

func renderSetup(w http.ResponseWriter, status int, view setupView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = setupTemplate.Execute(w, view)
}

// passwordPolicy returns the policy and dummy verifier in force. Normally the
// startup ones. When the control plane started before any administrator
// existed (PolicyLoader set), the durable policy is loaded once it exists
// (terminal or browser bootstrap) and cached, so logins verify against what
// was actually persisted rather than the startup calibration.
func (h *Handler) passwordPolicy(ctx context.Context) (auth.Argon2Policy, auth.DummyPasswordVerifier) {
	if h.config.PolicyLoader == nil {
		return h.config.PasswordPolicy, h.config.DummyPassword
	}
	h.policyMu.Lock()
	defer h.policyMu.Unlock()
	if !h.policyDurable {
		if durable, err := h.config.PolicyLoader(ctx); err == nil && durable.Validate() == nil {
			dummy := h.config.DummyPassword
			if durable != h.config.PasswordPolicy {
				dummy, err = auth.BuildDummyPasswordVerifier(durable)
			}
			if err == nil {
				h.livePolicy, h.liveDummy, h.policyDurable = durable, dummy, true
			}
		}
	}
	if h.policyDurable {
		return h.livePolicy, h.liveDummy
	}
	return h.config.PasswordPolicy, h.config.DummyPassword
}

// policyPersisted records that policy (with its dummy verifier) was just
// committed as the durable one by /setup.
func (h *Handler) policyPersisted(policy auth.Argon2Policy, dummy auth.DummyPasswordVerifier) {
	if h.config.PolicyLoader == nil {
		return
	}
	h.policyMu.Lock()
	defer h.policyMu.Unlock()
	if !h.policyDurable {
		h.livePolicy, h.liveDummy, h.policyDurable = policy, dummy, true
	}
}

// setupIdentities is the single per-client-IP throttle identity for /setup,
// with aliases for every retained CSRF key version (same rotation handling as
// the login identities).
func (h *Handler) setupIdentities(ip string) ([]postgres.ThrottleIdentity, error) {
	primary := h.config.CSRFKeys.ActiveVersion
	digest, err := auth.ThrottleDigest(h.config.CSRFKeys.Keys[primary], setupThrottleDomain, ip)
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(h.config.CSRFKeys.Keys))
	for version := range h.config.CSRFKeys.Keys {
		if version != primary {
			versions = append(versions, version)
		}
	}
	sort.Strings(versions)
	aliases := make([]postgres.ThrottleAlias, 0, len(versions))
	for _, version := range versions {
		value, aliasErr := auth.ThrottleDigest(h.config.CSRFKeys.Keys[version], setupThrottleDomain, ip)
		if aliasErr != nil {
			return nil, aliasErr
		}
		aliases = append(aliases, postgres.ThrottleAlias{KeyVersion: version, Digest: value})
	}
	return []postgres.ThrottleIdentity{{Kind: postgres.ThrottleIP, KeyVersion: primary, Digest: digest, Aliases: aliases}}, nil
}
