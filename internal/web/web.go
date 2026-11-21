// Package web provides the browser-only control-plane boundary.
package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/buildsecret"
	"autodeploy/internal/github"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/healthgate"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"

	"github.com/jackc/pgx/v5/pgconn"
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
// the /servers and /environments/resolve routes use. Every method
// takes the session principal and is owner-scoped by the repository itself;
// this interface never exposes a credential or enrollment digest, only
// caller-supplied digests going in. A nil ServerAdmin disables all of these
// routes (same nil-disables convention as GitHubSetup below): they 404.
// RepositoryLister feeds the repository-binding form on the landing page. nil hides
// the form's repository list (the numeric ID can still be typed).
type RepositoryLister interface {
	ListBindableRepositories(context.Context, auth.Principal) ([]postgres.BindableRepository, error)
}

type ServerAdmin interface {
	ListServers(context.Context, auth.Principal) ([]postgres.ServerSummary, error)
	RegisterServer(context.Context, auth.Principal, postgres.RegisterServerInput) (server.Server, time.Time, error)
	IssueEnrollment(context.Context, auth.Principal, string, [32]byte, int) (time.Time, error)
	IssueReenrollment(context.Context, auth.Principal, string, [32]byte, int, string) (time.Time, error)
	RevokeServer(context.Context, auth.Principal, string, string) error
	ListReconcilingEnvironments(context.Context, auth.Principal) ([]postgres.ReconcilingEnvironment, error)
	ResolveActivation(ctx context.Context, principal auth.Principal, environmentID string, expectedAttemptID int64, outcome, reason string) error
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

// RouteHeadAdmin is the narrow surface the /environments/route admin page
// (Slice 10 Step 10a) uses. Mirrors ServerAdmin/RuntimeConfigAdmin's
// nil-disables convention exactly: a nil RouteHeadAdmin 404s the route. It
// never sets active_release_id -- Step 10a ships no activation logic, only
// hostname assignment.
type RouteHeadAdmin interface {
	GetRouteHead(context.Context, auth.Principal, string) (postgres.RouteHead, bool, error)
	AssignRouteHostname(context.Context, auth.Principal, string, string, *int64) (postgres.RouteHead, error)
}

type Config struct {
	// ActivationEnabled hides the "activation disabled" notice when true.
	ActivationEnabled     bool
	PublicOrigin          string
	CSRFKeys              auth.CSRFKeyRing
	UsernameThrottleKeys  auth.UsernameThrottleKeyRing
	TrustedProxyCIDRs     []netip.Prefix
	AuthThrottleMaxRows   int
	PasswordPolicy        auth.Argon2Policy
	DummyPassword         auth.DummyPasswordVerifier
	GitHubSetup           *githubapp.SetupService
	RepositoryLister      RepositoryLister
	GitHubInstallationURL string
	ServerAdmin           ServerAdmin
	RuntimeConfigAdmin    RuntimeConfigAdmin
	RouteHeadAdmin        RouteHeadAdmin
	OperationsReader      OperationsReader
	DeploymentCanceller   DeploymentCanceller
	DeploymentLogReader   DeploymentLogReader
	ManualDeploy          ManualDeployStore
	Rollback              RollbackStore
	CommitResolver        githubapp.CommitResolver
	Now                   func() time.Time
	Random                io.Reader
	// Logger receives process-log lines. Nil discards them: it never
	// defaults to standard error, so every line goes through the redacting
	// logger the caller injects.
	Logger *slog.Logger
}

type Handler struct {
	repository Repository
	config     Config

	// Live-log streaming state (logs_stream.go). The timing fields are
	// unexported test seams; zero means the production value.
	streams             streamLimiter
	streamTick          time.Duration
	streamHeartbeat     time.Duration
	streamMaxDuration   time.Duration
	streamTerminalCheck time.Duration
	streamMaxPerSession int
	streamMaxTotal      int
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

// discardLogger is the logger used when none is injected.
var discardLogger = slog.New(slog.DiscardHandler)

// logger returns the injected process logger, or a discarding one when unset.
func (h *Handler) logger() *slog.Logger {
	if h.config.Logger == nil {
		return discardLogger
	}
	return h.config.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.securityHeaders(w)
	if r.URL == nil || r.URL.RawPath != "" || r.URL.ForceQuery {
		http.NotFound(w, r)
		return
	}
	if asset, ok := staticAssets[r.URL.Path]; ok {
		serveStatic(w, r, asset)
		return
	}
	if r.URL.RawQuery != "" && r.URL.Path != "/github/setup/callback" && r.URL.Path != "/environments/config" && r.URL.Path != "/environments/route" && r.URL.Path != "/deployments" && r.URL.Path != "/deployments/detail" && r.URL.Path != "/deployments/logs" && r.URL.Path != "/deployments/logs/stream" {
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
	case "/github":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.githubPage(w, r)
	case "/reconciliation":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.reconciliationPage(w, r)
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
	case "/environments":
		if _, ok := h.overviewReader(); !ok {
			http.NotFound(w, r)
			return
		}
		h.environmentsList(w, r)
	case "/projects", "/deployments", "/deployments/detail":
		if h.config.OperationsReader == nil {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/projects":
			if r.URL.RawQuery != "" {
				http.NotFound(w, r)
				return
			}
			h.projectsList(w, r)
		case "/deployments":
			h.deploymentsList(w, r)
		default:
			h.deploymentDetail(w, r)
		}
	case "/deployments/logs":
		if h.config.DeploymentLogReader == nil {
			http.NotFound(w, r)
			return
		}
		h.deploymentLogs(w, r)
	case "/deployments/logs/stream":
		// The stream re-reads the job state to know when to stop, so it needs
		// both readers.
		if h.config.DeploymentLogReader == nil || h.config.OperationsReader == nil {
			http.NotFound(w, r)
			return
		}
		h.deploymentLogStream(w, r)
	case "/deployments/cancel":
		if h.config.OperationsReader == nil || h.config.DeploymentCanceller == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.deploymentCancel(w, r)
	case "/deployments/manual":
		if r.URL.RawQuery != "" || !h.manualDeployEnabled() {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.deploymentManual(w, r)
	case "/deployments/rollback":
		if r.URL.RawQuery != "" || h.config.Rollback == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.deploymentRollback(w, r)
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
	case "/servers/reenroll":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.serversReenroll(w, r)
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
	case "/environments/resolve":
		if h.config.ServerAdmin == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.environmentsResolve(w, r)
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
	case "/environments/route":
		if h.config.RouteHeadAdmin == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.environmentRouteGet(w, r)
		case http.MethodPost:
			h.environmentRoutePost(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
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
	_, principal, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, principal.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	view := landingView{Nav: h.navFor(r.Context(), "home", principal), CSRF: csrf, ExecNotice: !h.config.ActivationEnabled}
	// The environment board is the one required read: its failure maps like any operations read.
	var rows []postgres.EnvironmentOverview
	if reader, ok := h.overviewReader(); ok {
		var readErr error
		if rows, readErr = reader.ListEnvironmentOverview(r.Context(), principal); readErr != nil {
			if errors.Is(readErr, postgres.ErrServerAuthorizationDenied) {
				operationsFailure(w, r, readErr)
				return
			}
			// Any other failure degrades the board only; the rest of the page still renders.
			rows, view.BoardUnavailable = nil, true
		} else {
			if view.Board, err = h.buildBoard(rows, csrf, "/"); err != nil {
				plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
				return
			}
			view.HasBoard = true
		}
	}
	// Everything below is optional: a failed read degrades its own section with a notice.
	facts := h.loadSetupFacts(r.Context(), principal)
	if h.config.ServerAdmin != nil {
		view.HasServers, view.ServersUnavailable = true, facts.serversFailed
		view.ServerRows = h.overviewServers(facts.servers)
	}
	if h.config.OperationsReader != nil {
		list, listErr := h.config.OperationsReader.ListDeployments(r.Context(), principal, postgres.DeploymentFilter{}, postgres.DeploymentPage{Limit: overviewRecentLimit})
		view.HasRecent, view.RecentUnavailable = true, listErr != nil
		for _, d := range list.Items {
			view.Recent = append(view.Recent, newDeploymentRow(d))
		}
	}
	view.Attention = h.attentionItems(rows, facts.servers)
	view.Checklist = h.newChecklist(facts.repoCount > 0, facts.hasActiveServer(), anyBound(rows), len(rows))
	renderOperations(w, landingTemplate, view)
}

// landingView is the Overview page's data (the type keeps its original name: the
// parity guard builds it directly). The GitHub section moved to /github in 22-3g.
type landingView struct {
	Nav  navView
	CSRF string

	ExecNotice         bool
	Attention          []attentionItem
	Checklist          *checklistView
	HasBoard           bool
	BoardUnavailable   bool
	Board              []boardRow
	HasRecent          bool
	Recent             []deploymentRow
	RecentUnavailable  bool
	HasServers         bool
	ServerRows         []overviewServer
	ServersUnavailable bool
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
		var stage githubapp.SetupStageError
		if errors.As(err, &stage) {
			// Fixed step name only (githubapp.SetupStageError); diagnosable from the journal.
			h.logger().Error("github setup callback failed", "stage", stage.Stage)
		}
		badRequest(w)
		return
	}
	http.SetCookie(w, handoffCookie(handoff, h.config.Now()))
	// No redirect: this request began on github.com, so a redirect chain stays
	// cross-site and browsers withhold the SameSite=Strict session cookie at
	// /github/setup/complete (observed on the acer: bounced to /login). A link on
	// this same-origin page makes the next navigation same-site.
	// The callback arrives cross-site, so the session cookie is usually absent: nav without sign-out.
	var nav navView
	if _, p, ok := h.session(r.Context(), r); ok {
		nav = h.navFor(r.Context(), "github", p)
	} else {
		nav = h.navFor(r.Context(), "github", auth.Principal{})
	}
	renderOperations(w, githubContinueTemplate, githubContinueView{Nav: nav})
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
	renderOperations(w, githubCompleteTemplate, githubCompleteView{Nav: h.navFor(r.Context(), "github", p), CSRF: csrf})
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
		var stage githubapp.SetupStageError
		if errors.As(err, &stage) {
			h.logger().Error("github setup complete failed", "stage", stage.Stage)
		}
		badRequest(w)
		return
	}
	http.SetCookie(w, expiredHandoffCookie())
	redirect(w, "/github")
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
	redirect(w, "/github")
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
	redirect(w, "/github")
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
		operationsFailure(w, r, err)
		return
	}
	environments, err := h.config.ServerAdmin.ListReconcilingEnvironments(r.Context(), p)
	if err != nil {
		operationsFailure(w, r, err)
		return
	}
	view := serversView{Nav: h.navForCount("servers", p, len(environments)), CSRF: csrf, ReconcilingText: reconcilingText(len(environments)), EnrollHint: enrollCommand(h.config.PublicOrigin, "<SERVER_ID>")}
	for _, s := range summaries {
		view.Servers = append(view.Servers, serverRow{ID: s.ID, Name: s.Name, Status: string(s.Status), CreatedAt: formatTimestamp(s.CreatedAt), LastHeartbeatAt: formatTimestamp(s.LastHeartbeatAt), ActiveCredentialVersion: formatCredentialVersion(s.ActiveCredentialVersion), Enrollment: string(s.Enrollment)})
	}
	renderOperations(w, serversTemplate, view)
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
	h.renderEnrollmentToken(w, r, p, registered.ID, token, expiresAt)
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
	h.renderEnrollmentToken(w, r, p, serverID, token, expiresAt)
}

// serversReenroll issues a single-use re-enrollment token for an already
// enrolled server (e.g. after a restore left its agent with a credential the
// database does not know). Like revoke it needs CSRF, provenance, a reason and
// an explicit confirm=reenroll, because consuming the token replaces the
// server's live credential. The token is displayed once, exactly as for
// enrollment.
func (h *Handler) serversReenroll(w http.ResponseWriter, r *http.Request) {
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
	serverID := v.Get("server_id")
	if v.Get("confirm") != "reenroll" || v.Get("reason") == "" || !github.ValidOpaqueID(serverID) {
		badRequest(w)
		return
	}
	token, digest, err := h.newEnrollmentToken()
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	expiresAt, err := h.config.ServerAdmin.IssueReenrollment(r.Context(), p, serverID, digest, enrollmentTTLSeconds, v.Get("reason"))
	if err != nil {
		badRequest(w)
		return
	}
	h.renderEnrollmentToken(w, r, p, serverID, token, expiresAt)
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

// maxResolveReasonBytes is the longest reason the resolve form accepts. The
// repository records "outcome=<outcome>; <reason>" as the audit reason and
// refuses more than 512 bytes of it (validReason), so the reason gets what the
// longest outcome prefix leaves. The form's maxlength counts UTF-16 units, not
// bytes, so the byte check in environmentsResolve is the authoritative one.
const maxResolveReasonBytes = 512 - len("outcome="+postgres.ActivationOutcomeNotActivated+"; ")

// environmentsResolve is the audited administrator path (ADR 0004 Decision 6)
// for an environment whose owning server cannot (or has not) reconciled it
// itself. The administrator declares what the block really was: an activation
// that did (activated) or did not (not_activated) switch the route, or a block
// no activation caused (no_activation). There is no default outcome.
func (h *Handler) environmentsResolve(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil {
		badRequest(w)
		return
	}
	returnTo := takeReturnTo(v)
	if len(v) != 5 || len(v["csrf"]) != 1 || len(v["environment_id"]) != 1 || len(v["attempt_id"]) != 1 || len(v["outcome"]) != 1 || len(v["reason"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	outcome, reason := v.Get("outcome"), v.Get("reason")
	// The attempt the operator's form was rendered for; the repository refuses
	// the request if a different attempt is pending now.
	attemptID, err := strconv.ParseInt(v.Get("attempt_id"), 10, 64)
	if err != nil || attemptID <= 0 {
		badRequest(w)
		return
	}
	if !github.ValidOpaqueID(v.Get("environment_id")) || !validResolveOutcome(outcome) || len(reason) > maxResolveReasonBytes || strings.TrimSpace(reason) == "" {
		badRequest(w)
		return
	}
	if err := h.config.ServerAdmin.ResolveActivation(r.Context(), p, v.Get("environment_id"), attemptID, outcome, reason); err != nil {
		resolveError(w, err, outcome)
		return
	}
	redirectAfter(w, returnTo, "/reconciliation")
}

func validResolveOutcome(outcome string) bool {
	switch outcome {
	case postgres.ActivationOutcomeActivated, postgres.ActivationOutcomeNotActivated, postgres.ActivationOutcomeNoActivation:
		return true
	}
	return false
}

// resolveError renders a fixed, operator-actionable text per refusal; it never
// echoes err. CorruptState is checked first because the repository wraps it
// with %w and a detail that must not reach the page. The web package has no
// logger, so the detail is not recorded here.
func resolveError(w http.ResponseWriter, err error, outcome string) {
	switch {
	case errors.Is(err, postgres.ErrAgentLeaseCorruptState):
		plain(w, http.StatusInternalServerError, "environment is in an inconsistent state; it cannot be resolved from this form\n")
	case errors.Is(err, postgres.ErrAgentLeaseOutcomeMismatch):
		if outcome == postgres.ActivationOutcomeNoActivation {
			plain(w, http.StatusConflict, "this block was caused by an activation; declare activated or not_activated\n")
			return
		}
		plain(w, http.StatusConflict, "this block was not caused by an activation; declare no_activation\n")
	case errors.Is(err, postgres.ErrAgentLeaseActivationUnresolved):
		plain(w, http.StatusConflict, "the activation switch is still authorized and cannot be resolved yet; retry once it requires reconciliation\n")
	case errors.Is(err, postgres.ErrAgentLeaseActivationRouteMoved):
		plain(w, http.StatusConflict, "the route moved since the activation was authorized, so activated is impossible; declare not_activated\n")
	case errors.Is(err, postgres.ErrAgentLeaseIntegrityConflict):
		plain(w, http.StatusConflict, "this environment was already resolved with a different outcome\n")
	case errors.Is(err, postgres.ErrActivationResolveStale):
		plain(w, http.StatusConflict, "this form is for a different activation than the one now blocking the environment; reload the servers page and review it\n")
	case errors.Is(err, postgres.ErrServerAuthorizationDenied), errors.Is(err, postgres.ErrAgentLeaseInvalidInput):
		badRequest(w)
	default:
		// Not a refusal of the operator's input (a database failure, a
		// cancelled context, a failed commit): saying "bad request" would
		// send the operator to fix a form that is fine.
		storeFailure(w, err, "the environment could not be resolved; retry\n")
	}
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
	policy, err := parseHealthPolicyForm(v)
	if err != nil {
		return postgres.RuntimeConfigInput{}, err
	}
	return postgres.RuntimeConfigInput{ContainerPort: port, ContainerScheme: scheme, ReleaseStrategy: strategy, DrainSeconds: drain, CPUMillis: cpu, MemoryBytes: memory, RestartPolicy: restart, HealthGatePath: healthGatePath, HealthGateHost: healthGateHost, HealthPolicy: policy}, nil
}

// healthPolicyFormFields are the five health policy form fields, all required
// in the (strict) form; health_tcp_port may be empty (no TCP check).
var healthPolicyFormFields = []string{"health_success_threshold", "health_interval_seconds", "health_timeout_seconds", "health_deadline_seconds", "health_tcp_port"}

var errHealthPolicy = errors.New("health policy must be whole numbers within bounds (successes 1-10, interval 1-30s, timeout 1-30s and no longer than the interval, deadline 10-600s and at least successes x interval, optional TCP port 1-65535)")

// parseFormDigits parses an unsigned decimal; unlike strconv.Atoi it refuses
// signs, spaces, and anything but ASCII digits.
func parseFormDigits(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// parseHealthPolicyForm parses the five health policy fields and validates
// them with internal/healthgate (the single source of the bounds and cross
// rules). No defaults are applied: the rendered form always carries values.
func parseHealthPolicyForm(v url.Values) (healthgate.Policy, error) {
	var nums [4]int
	for i, name := range healthPolicyFormFields[:4] {
		n, ok := parseFormDigits(v.Get(name))
		if !ok {
			return healthgate.Policy{}, errHealthPolicy
		}
		nums[i] = n
	}
	policy := healthgate.Policy{SuccessThreshold: nums[0], IntervalSeconds: nums[1], TimeoutSeconds: nums[2], DeadlineSeconds: nums[3]}
	if raw := v.Get("health_tcp_port"); raw != "" {
		n, ok := parseFormDigits(raw)
		if !ok || !healthgate.ValidTCPPort(n) {
			return healthgate.Policy{}, errHealthPolicy
		}
		policy.TCPPort = n
	}
	if !policy.Valid() {
		return healthgate.Policy{}, errHealthPolicy
	}
	return policy, nil
}

// healthBoundsView carries healthgate's bounds to the template so the input
// min/max attributes have one source of truth.
type healthBoundsView struct {
	MinThreshold, MaxThreshold, MinInterval, MaxInterval, MinTimeout, MaxTimeout, MinDeadline, MaxDeadline, MinTCPPort, MaxTCPPort int
}

func newHealthBoundsView() healthBoundsView {
	return healthBoundsView{
		MinThreshold: healthgate.MinSuccessThreshold, MaxThreshold: healthgate.MaxSuccessThreshold,
		MinInterval: healthgate.MinIntervalSeconds, MaxInterval: healthgate.MaxIntervalSeconds,
		MinTimeout: healthgate.MinTimeoutSeconds, MaxTimeout: healthgate.MaxTimeoutSeconds,
		MinDeadline: healthgate.MinDeadlineSeconds, MaxDeadline: healthgate.MaxDeadlineSeconds,
		MinTCPPort: healthgate.MinTCPPort, MaxTCPPort: healthgate.MaxTCPPort,
	}
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
		HealthThreshold: v.Get("health_success_threshold"), HealthInterval: v.Get("health_interval_seconds"),
		HealthTimeout: v.Get("health_timeout_seconds"), HealthDeadline: v.Get("health_deadline_seconds"), HealthTCPPort: v.Get("health_tcp_port"),
		HealthBounds: newHealthBoundsView(),
	}
}

// buildRuntimeConfigView renders the stored configuration and bindings
// exactly as read back from the repository (the normal GET path, and the
// fallback view after a rejected mutation once bindings are re-fetched).
func buildRuntimeConfigView(csrf, environmentID string, cfg postgres.RuntimeConfig, bindings []postgres.RuntimeBinding, found bool) runtimeConfigView {
	view := runtimeConfigView{CSRF: csrf, EnvironmentID: environmentID, Exists: found, Bindings: buildBindingRows(bindings), HealthBounds: newHealthBoundsView()}
	// Zero is never a valid stored value, so defaulting only affects views of
	// rows that predate or omit a policy (and the create form).
	policy := healthgate.Policy{}
	if found {
		policy = cfg.HealthPolicy
	}
	policy = policy.WithDefaults()
	view.HealthThreshold = strconv.Itoa(policy.SuccessThreshold)
	view.HealthInterval = strconv.Itoa(policy.IntervalSeconds)
	view.HealthTimeout = strconv.Itoa(policy.TimeoutSeconds)
	view.HealthDeadline = strconv.Itoa(policy.DeadlineSeconds)
	if policy.TCPPort != 0 {
		view.HealthTCPPort = strconv.Itoa(policy.TCPPort)
	}
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
	var buildSecretNames []string
	for _, b := range bindings {
		if b.BindingKind == "secret" && b.DeliveryScope == "build" {
			buildSecretNames = append(buildSecretNames, b.VariableName)
		}
	}
	rows := make([]bindingRow, 0, len(bindings))
	for _, b := range bindings {
		row := bindingRow{VariableName: b.VariableName, Kind: b.BindingKind, Scope: b.DeliveryScope, CreatedAt: formatTimestamp(b.CreatedAt)}
		if b.BindingKind == "secret" {
			if b.SecretVersionNumber > 0 {
				row.Identity = "secret head " + b.SecretHeadID + " (version " + strconv.FormatInt(b.SecretVersionNumber, 10) + ")"
			} else {
				row.Identity = "secret head " + b.SecretHeadID + " (no active version)"
			}
			if row.Scope == "build" {
				if id, ok := buildsecret.ID(buildSecretNames, b.VariableName); ok {
					row.DockerfileID = "Dockerfile id: " + id
				}
			}
		} else {
			row.Identity = b.LiteralValue
		}
		rows = append(rows, row)
	}
	return rows
}

func (h *Handler) renderEnvironmentConfig(w http.ResponseWriter, r *http.Request, p auth.Principal, view runtimeConfigView) {
	view.Nav = h.navFor(r.Context(), "config", p)
	if view.Strip == nil && view.Empty == nil {
		view.Strip = h.stripFor(r.Context(), p, "/environments/config", view.EnvironmentID)
	}
	renderOperations(w, environmentConfigTemplate, view)
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
	var environmentID string
	var strip []envChoice
	if _, overview := h.overviewReader(); overview && len(q) == 0 {
		// 22-3h: environment_id is optional; default to the first owned environment.
		choices, empty, ok := h.defaultEnvironment(w, r, p, "/environments/config")
		if !ok {
			return
		}
		if empty != nil {
			h.renderEnvironmentConfig(w, r, p, runtimeConfigView{Empty: empty})
			return
		}
		environmentID, strip = choices[0].ID, choices
	} else {
		if len(q) != 1 || len(q["environment_id"]) != 1 {
			badRequest(w)
			return
		}
		environmentID = q.Get("environment_id")
		if !github.ValidOpaqueID(environmentID) {
			badRequest(w)
			return
		}
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
	view := buildRuntimeConfigView(csrf, environmentID, cfg, bindings, found)
	view.Strip = strip
	h.renderEnvironmentConfig(w, r, p, view)
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
	h.renderEnvironmentConfig(w, r, p, submitted)
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
	if err != nil || len(v) != 17 {
		badRequest(w)
		return
	}
	for _, name := range []string{"environment_id", "csrf", "expected_revision", "container_port", "container_scheme", "release_strategy", "drain_seconds", "cpu_millis", "memory_bytes", "restart_policy", "health_gate_path", "health_gate_host", "health_success_threshold", "health_interval_seconds", "health_timeout_seconds", "health_deadline_seconds", "health_tcp_port"} {
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
		h.renderEnvironmentConfig(w, r, p, view)
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

// buildRouteHeadView builds the sticky route view for a GET (from the
// stored row) or a freshly re-read row after a rejected mutation.
func buildRouteHeadView(csrf, environmentID string, head postgres.RouteHead, found bool) routeHeadView {
	view := routeHeadView{CSRF: csrf, EnvironmentID: environmentID, Exists: found}
	if found {
		view.Hostname = head.Hostname
		view.Revision = strconv.FormatInt(head.RouteRevision, 10)
		view.ActiveReleaseID = head.ActiveReleaseID
		view.ReconciliationRequired = head.ReconciliationRequired
		view.UpdatedAt = formatTimestamp(head.UpdatedAt)
	}
	return view
}

func (h *Handler) renderRouteHead(w http.ResponseWriter, r *http.Request, p auth.Principal, view routeHeadView) {
	view.Nav = h.navFor(r.Context(), "routes", p)
	if view.Strip == nil && view.Empty == nil {
		view.Strip = h.stripFor(r.Context(), p, "/environments/route", view.EnvironmentID)
	}
	renderOperations(w, routeHeadTemplate, view)
}

// environmentRouteGet renders one environment's route head. GET is
// safe/idempotent, matching environmentConfigGet exactly: no CSRF check,
// only a fresh token minted for the form this page renders.
func (h *Handler) environmentRouteGet(w http.ResponseWriter, r *http.Request) {
	_, p, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return
	}
	q := r.URL.Query()
	var environmentID string
	var strip []envChoice
	if _, overview := h.overviewReader(); overview && len(q) == 0 {
		// 22-3h: environment_id is optional; default to the first owned environment.
		choices, empty, ok := h.defaultEnvironment(w, r, p, "/environments/route")
		if !ok {
			return
		}
		if empty != nil {
			h.renderRouteHead(w, r, p, routeHeadView{Empty: empty})
			return
		}
		environmentID, strip = choices[0].ID, choices
	} else {
		if len(q) != 1 || len(q["environment_id"]) != 1 {
			badRequest(w)
			return
		}
		environmentID = q.Get("environment_id")
		if !github.ValidOpaqueID(environmentID) {
			badRequest(w)
			return
		}
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	head, found, err := h.config.RouteHeadAdmin.GetRouteHead(r.Context(), p, environmentID)
	if err != nil {
		badRequest(w)
		return
	}
	view := buildRouteHeadView(csrf, environmentID, head, found)
	view.Releases = h.environmentReleaseRows(r.Context(), p, environmentID)
	view.Strip = strip
	h.renderRouteHead(w, r, p, view)
}

// renderRouteRejection re-fetches the current route head so a rejected
// submission still shows accurate Exists/ActiveReleaseID/Revision state
// beside the operator's just-rejected (sticky) form values, matching
// renderConfigRejection's pattern.
func (h *Handler) renderRouteRejection(w http.ResponseWriter, r *http.Request, p auth.Principal, submitted routeHeadView) {
	head, found, err := h.config.RouteHeadAdmin.GetRouteHead(r.Context(), p, submitted.EnvironmentID)
	if err != nil {
		badRequest(w)
		return
	}
	submitted.Exists = found
	if found {
		submitted.Revision = strconv.FormatInt(head.RouteRevision, 10)
		submitted.ActiveReleaseID = head.ActiveReleaseID
		submitted.ReconciliationRequired = head.ReconciliationRequired
	}
	submitted.Releases = h.environmentReleaseRows(r.Context(), p, submitted.EnvironmentID)
	h.renderRouteHead(w, r, p, submitted)
}

// environmentRoutePost creates (expected_revision empty) or CAS-assigns
// (expected_revision set) one environment's route hostname. ADR 0005
// ("Domains, TLS, and servers") requires an authenticated administrator to
// explicitly acknowledge authority to use the hostname before it is bound;
// acknowledge_authority is an HTML checkbox, which browsers omit entirely
// from the submitted body when unchecked (not merely send it as "off"), so
// this handler treats it as optional in the form shape and only checks its
// value, rather than requiring it as a fifth always-present field the way
// every other field here is required.
func (h *Handler) environmentRoutePost(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || (len(v) != 4 && len(v) != 5) {
		badRequest(w)
		return
	}
	for _, name := range []string{"environment_id", "csrf", "expected_revision", "hostname"} {
		if len(v[name]) != 1 {
			badRequest(w)
			return
		}
	}
	// A 5th key, if present at all, must be exactly acknowledge_authority
	// with exactly its one legitimate value ("on", the value the checkbox
	// in routeHeadTemplate submits when checked) -- any other 5th key name,
	// or any other value, is an unrecognized/tampered field and is rejected
	// outright rather than silently ignored or treated as an unchecked box.
	if len(v) == 5 {
		ack, ok := v["acknowledge_authority"]
		if !ok || len(ack) != 1 || ack[0] != "on" {
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
	submitted := routeHeadView{CSRF: csrf, EnvironmentID: environmentID, Hostname: v.Get("hostname"), Revision: v.Get("expected_revision")}

	if v.Get("acknowledge_authority") != "on" {
		submitted.RouteError = "you must acknowledge that you have authority to use this hostname"
		h.renderRouteRejection(w, r, p, submitted)
		return
	}
	hostname := strings.ToLower(v.Get("hostname"))
	if !healthgate.ValidRouteHostname(hostname) {
		submitted.RouteError = "hostname must be a lowercase DNS hostname with no port, at most 253 bytes"
		h.renderRouteRejection(w, r, p, submitted)
		return
	}

	revisionRaw := v.Get("expected_revision")
	var expected *int64
	if revisionRaw != "" {
		parsed, convErr := strconv.ParseInt(revisionRaw, 10, 64)
		if convErr != nil || parsed < 0 {
			submitted.RouteError = "invalid expected revision"
			h.renderRouteRejection(w, r, p, submitted)
			return
		}
		expected = &parsed
	}
	if _, opErr := h.config.RouteHeadAdmin.AssignRouteHostname(r.Context(), p, environmentID, hostname, expected); opErr != nil {
		switch {
		case errors.Is(opErr, postgres.ErrRouteHeadConflict):
			submitted.RouteError = "this route changed underneath you since the page was loaded; the values below have been refreshed -- review and resubmit"
		case errors.Is(opErr, postgres.ErrRouteHeadNotFound):
			submitted.RouteError = "this route no longer exists; reload the page"
		case errors.Is(opErr, postgres.ErrRouteHeadAlreadyExists):
			submitted.RouteError = "a route already exists for this environment; reload the page and edit it instead"
		case errors.Is(opErr, postgres.ErrRouteHeadHostnameTaken):
			submitted.RouteError = "this hostname is already assigned to another environment"
		case errors.Is(opErr, postgres.ErrRouteHeadHostnameImmutable):
			submitted.RouteError = "this route has an active release; the hostname cannot change until it is deactivated"
		case errors.Is(opErr, postgres.ErrRouteHeadReconciliationRequired):
			submitted.RouteError = "this route has an unresolved activation; resolve it on the environment's reconciliation page before changing the hostname"
		default:
			submitted.RouteError = "route rejected: invalid values, or you are not authorized to configure this environment"
		}
		h.renderRouteRejection(w, r, p, submitted)
		return
	}
	redirect(w, "/environments/route?environment_id="+url.QueryEscape(environmentID))
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src 'self'; script-src 'self'; connect-src 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// HSTS: PublicOrigin is always https (CanonicalHTTPSOrigin rejects
	// anything else), so every response from this handler is HTTPS-only.
	// One day for now (owner decision); raise to one year in 22-6 once public
	// ACME certs are in place. No includeSubDomains/preload: V1 is a single host, and
	// subdomains carry deployed apps whose TLS the operator controls;
	// preload is irreversible in practice and must be an explicit decision.
	w.Header().Set("Strict-Transport-Security", "max-age=86400")
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

// storeFailure is the shared rendering point for an unexpected store error.
// SQLSTATE 55P03 (lock_not_available: the pool's lock_timeout fired) and
// 57014 (query_canceled: statement_timeout fired) mean the database was
// busy, not that the request was wrong or the server broken, so they answer
// 503 with Retry-After; anything else keeps the caller's 500 message. The
// error text is never echoed (it can carry SQL or connection detail).
func storeFailure(w http.ResponseWriter, err error, internalMsg string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014") {
		w.Header().Set("Retry-After", "5")
		plain(w, http.StatusServiceUnavailable, "unavailable; retry\n")
		return
	}
	plain(w, http.StatusInternalServerError, internalMsg)
}
func badRequest(w http.ResponseWriter) { plain(w, http.StatusBadRequest, "bad request\n") }
func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	plain(w, http.StatusMethodNotAllowed, "method not allowed\n")
}

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
	EnvironmentID, ConfigHref, RouteHref, AttemptID, JobID, ServerID, Since, Activation string
}
type serversView struct {
	Nav     navView
	CSRF    string
	Servers []serverRow
	// ReconcilingText is the attention banner ("2 environments need
	// reconciliation"); empty when nothing is blocked.
	ReconcilingText string
	// EnrollHint is the host enrollment command shown when no server exists yet.
	EnrollHint string
}

// reconcilingText is the banner sentence for n blocked environments, "" for none.
func reconcilingText(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 environment needs reconciliation"
	default:
		return strconv.Itoa(n) + " environments need reconciliation"
	}
}

// activationLabel renders "" (a block no activation caused) as "none".
func activationLabel(state string) string {
	if state == "" {
		return "none"
	}
	return state
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
func (h *Handler) renderEnrollmentToken(w http.ResponseWriter, r *http.Request, p auth.Principal, serverID, token string, expiresAt time.Time) {
	minutes := 0
	if left := expiresAt.Sub(h.config.Now()); left > 0 {
		minutes = int((left + time.Minute - 1) / time.Minute)
	}
	renderOperations(w, enrollmentTokenTemplate, enrollmentView{Nav: h.navFor(r.Context(), "servers", p), ServerID: serverID, Token: token, ExpiresAt: formatTimestamp(expiresAt), ValidMinutes: minutes, Command: enrollCommand(h.config.PublicOrigin, serverID)})
}

var (
	enrollOriginPattern   = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	enrollServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// enrollCommand is the host-side command that redeems an enrollment token. It
// mirrors the "agent enroll" step install-app.sh prints (deploy/host/install-app.sh,
// "Next, as root"): the token is typed at a hidden prompt and piped on stdin, never
// placed in argv or the environment, so the command carries no token. The agent
// user, binary and state directory are the installer's fixed layout.
func enrollCommand(origin, serverID string) string {
	// Both values land in a shell command the operator pastes as root, so a value
	// that is not plainly a host or an id is replaced by its placeholder.
	if !enrollOriginPattern.MatchString(origin) {
		origin = "https://<CP_HOST>"
	}
	if !enrollServerIDPattern.MatchString(serverID) {
		serverID = "<SERVER_ID>"
	}
	return "read -rs -p 'enrollment token: ' T; echo\n" +
		"printf '%s\\n' \"$T\" | sudo -u autodeploy-agent /usr/local/lib/autodeploy/app/agent enroll \\\n" +
		"  --base-url " + origin + " --server-id " + serverID + " --state-dir /var/lib/autodeploy/agent\n" +
		"unset T"
}

// serversTemplate lists registered servers with a registration form, a
// reissue-enrollment and revoke form per server, and reconciling
// environments with a resolve form per environment. Rotate is
// intentionally absent here: Step 6 Decision 4 changed credential issuance
// so the agent, not the control plane, generates its own credential and
// sends only its digest at enrollment (internal/agent/http Enroll); a
// control-plane "rotate" can no longer hand back a usable credential.
// Re-enrolling an already-enrolled server goes through /servers/reenroll
// (migration 000029): an administrator-issued, single-use re-enrollment
// token the agent presents to `agent enroll`, which then installs a new
// credential. IssueEnrollment stays blocked once a server's first
// enrollment token has been consumed (schema 000012).

// runtimeConfigView and bindingRow are pre-formatted the same way
// serverRow/reconcilingRow are: every field is already rendered to text
// (including a rejected submission's raw, possibly-invalid input), so
// environmentConfigTemplate stays free of template functions or logic.
// html/template auto-escapes every field.
type bindingRow struct {
	VariableName, Kind, Scope, Identity, CreatedAt string
	// DockerfileID is set only for build-scoped secret bindings (buildsecret.ID).
	DockerfileID string
}
type runtimeConfigView struct {
	Nav navView
	// Strip lists the owned environments (nil without an overview reader);
	// Empty replaces the page body when the owner has none (22-3h).
	Strip                                                                 []envChoice
	Empty                                                                 *envEmptyView
	CSRF, EnvironmentID                                                   string
	Exists                                                                bool
	Revision                                                              string
	Port, Scheme, Strategy, Drain, CPU, Memory, Restart                   string
	HealthGatePath, HealthGateHost                                        string
	HealthPolicyRevision, UpdatedAt                                       string
	HealthThreshold, HealthInterval, HealthTimeout, HealthDeadline        string
	HealthTCPPort                                                         string
	HealthBounds                                                          healthBoundsView
	ConfigError                                                           string
	Bindings                                                              []bindingRow
	BindingError                                                          string
	BindVariable, BindKind, BindScope, BindSecretHeadID, BindLiteralValue string
}

// environmentConfigTemplate renders one environment's runtime configuration
// form (create or CAS-update, depending on whether Exists is true) and its
// variable bindings with add/remove forms (templates/environment-config.html).
// No JavaScript and no inline styles; styling comes only from the shared
// layout's same-origin stylesheet (templates.go).

type routeHeadView struct {
	Nav                    navView
	Strip                  []envChoice
	Empty                  *envEmptyView
	CSRF, EnvironmentID    string
	Exists                 bool
	Hostname               string
	Revision               string
	ActiveReleaseID        string
	ReconciliationRequired bool
	UpdatedAt              string
	RouteError             string
	Releases               []releaseRow
}

// routeHeadTemplate renders one environment's route head (hostname
// assignment only, Slice 10 Step 10a's scope): plain HTML, no JavaScript,
// matching environmentConfigTemplate's own conventions exactly, including
// html/template's automatic contextual escaping of every field. The
// acknowledgement checkbox is ADR 0005's requirement ("Domains, TLS, and
// servers": "An authenticated owner/administrator explicitly assigns the
// hostname and acknowledges authority to use it") -- it is never
// pre-checked and carries no stored value, only a one-time attestation the
// server requires on every submission.

// enrollmentTokenTemplate is rendered exactly once per register/reissue
// response: the raw enrollment token appears here and nowhere else.
