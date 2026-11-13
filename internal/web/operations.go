package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// OperationsReader is the narrow read-only surface behind /projects,
// /deployments and /deployments/detail (step 15c). A nil OperationsReader
// 404s all three routes. *postgres.OperationsReadRepository satisfies it.
type OperationsReader interface {
	ListProjects(ctx context.Context, principal auth.Principal) ([]postgres.ProjectSummary, error)
	ListDeployments(ctx context.Context, principal auth.Principal, filter postgres.DeploymentFilter, page postgres.DeploymentPage) (postgres.DeploymentList, error)
	GetDeploymentDetail(ctx context.Context, principal auth.Principal, deploymentID string) (postgres.DeploymentDetail, error)
}

// DeploymentCanceller is the narrow write surface behind POST
// /deployments/cancel (step 15d). A nil DeploymentCanceller 404s the route and
// hides the Cancel button. *postgres.AgentLeaseRepository satisfies it.
type DeploymentCanceller interface {
	RequestCancellation(ctx context.Context, principal auth.Principal, jobID int64) error
}

const (
	// maxOperationsParamBytes bounds every query value accepted here.
	maxOperationsParamBytes = 128
	// maxCursorBytes bounds the encoded cursor (RFC3339Nano + "|" + id, base64).
	maxCursorBytes = 512
	// detailRefreshSeconds is the meta-refresh period for non-terminal jobs.
	detailRefreshSeconds = 5
)

var (
	projectsTemplate         = parsePage("projects")
	deploymentsTemplate      = parsePage("deployments")
	deploymentDetailTemplate = parsePage("deployment-detail")
)

// terminalJobStates mirrors agent_jobs_state_check (migration 000013): the
// states with no outgoing transition. Every other state (queued, leased,
// retry_wait) can still change, so the detail page keeps refreshing.
var terminalJobStates = map[string]bool{"succeeded": true, "failed": true, "cancelled": true, "dead": true}

// stateTone maps a closed state enum to a CSS tone class. Unknown values map
// to "neutral" so no database text ever reaches a class attribute.
func stateTone(state string) string {
	switch state {
	case "healthy", "succeeded", "ready", "active", "running":
		return "ok"
	case "failed", "dead":
		return "bad"
	case "queued", "assigned", "fetching", "building", "starting", "health_checking", "activating", "leased", "retry_wait", "candidate", "started", "removing":
		return "busy"
	default:
		// "rolled_back" and the other terminal states are neutral.
		return "neutral"
	}
}

// encodeDeploymentCursor renders the opaque keyset cursor:
// base64url(RFC3339Nano UTC created_at + "|" + deployment id), no padding.
func encodeDeploymentCursor(c postgres.DeploymentCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.DeploymentID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeDeploymentCursor strictly decodes a cursor: canonical base64url, a UTC
// RFC3339Nano timestamp that re-formats identically, and a bounded id with no
// control characters. Anything else is rejected.
func decodeDeploymentCursor(s string) (postgres.DeploymentCursor, bool) {
	if s == "" || len(s) > maxCursorBytes {
		return postgres.DeploymentCursor{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return postgres.DeploymentCursor{}, false
	}
	ts, id, found := strings.Cut(string(raw), "|")
	if !found || !validOperationsID(id) {
		return postgres.DeploymentCursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || t.UTC().Format(time.RFC3339Nano) != ts {
		return postgres.DeploymentCursor{}, false
	}
	return postgres.DeploymentCursor{CreatedAt: t, DeploymentID: id}, true
}

func validOperationsID(s string) bool {
	if s == "" || len(s) > maxOperationsParamBytes {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

// singleParam returns the only value of key; ok is false for repeats.
func singleParam(q url.Values, key string) (string, bool) {
	v := q[key]
	switch len(v) {
	case 0:
		return "", true
	case 1:
		return v[0], true
	}
	return "", false
}

func onlyParams(q url.Values, allowed ...string) bool {
next:
	for k := range q {
		for _, a := range allowed {
			if k == a {
				continue next
			}
		}
		return false
	}
	return true
}

// operationsPrelude applies the shared route rules: method allowlist, then the
// session. It returns false after writing the response.
func (h *Handler) operationsPrelude(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return auth.Principal{}, false
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok {
		redirect(w, "/login")
		return auth.Principal{}, false
	}
	return p, true
}

// operationsFailure maps a repository error. A denied principal (session
// revoked between validation and the read) is treated like no session.
func operationsFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, postgres.ErrServerAuthorizationDenied):
		redirect(w, "/login")
	case errors.Is(err, postgres.ErrOperationsNotFound):
		http.NotFound(w, r)
	case errors.Is(err, postgres.ErrOperationsInvalidInput):
		badRequest(w)
	default:
		plain(w, http.StatusInternalServerError, "the page could not be loaded; retry\n")
	}
}

func renderOperations(w http.ResponseWriter, t *template.Template, view any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, view); err != nil {
		plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func detailHref(deploymentID string) string {
	return "/deployments/detail?" + url.Values{"id": {deploymentID}}.Encode()
}

func deploymentsHref(filter postgres.DeploymentFilter, after string) string {
	v := url.Values{}
	if filter.ProjectID != "" {
		v.Set("project_id", filter.ProjectID)
	}
	if filter.EnvironmentID != "" {
		v.Set("environment_id", filter.EnvironmentID)
	}
	if after != "" {
		v.Set("after", after)
	}
	if len(v) == 0 {
		return "/deployments"
	}
	return "/deployments?" + v.Encode()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortDigest(hex string) string {
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

// ---- /projects ----

type projectRow struct {
	ProjectID       string
	ProjectHref     string
	Repository      string
	DeployRef       string
	BindingState    string
	Environments    []environmentLink
	NoEnvironments  bool
	HasLatest       bool
	LatestStatus    string
	LatestTone      string
	LatestCommit    string
	LatestTime      string
	LatestDetailURL string
	// CanDeploy renders the manual Deploy form; Nonce is fresh per render.
	CanDeploy bool
	Nonce     string
}

// environmentLink is an environment id with the hrefs of its runtime config
// and route pages; ids are query-escaped here so the templates stay dumb.
type environmentLink struct{ ID, ConfigHref, RouteHref string }

func newEnvironmentLink(id string) environmentLink {
	q := "?environment_id=" + url.QueryEscape(id)
	return environmentLink{ID: id, ConfigHref: "/environments/config" + q, RouteHref: "/environments/route" + q}
}

func environmentLinks(ids []string) []environmentLink {
	links := make([]environmentLink, 0, len(ids))
	for _, id := range ids {
		links = append(links, newEnvironmentLink(id))
	}
	return links
}

type projectsView struct {
	ExecNotice bool
	Projects   []projectRow
	CanDeploy  bool
	CSRF       string
}

func (h *Handler) projectsList(w http.ResponseWriter, r *http.Request) {
	p, ok := h.operationsPrelude(w, r)
	if !ok {
		return
	}
	projects, err := h.config.OperationsReader.ListProjects(r.Context(), p)
	if err != nil {
		operationsFailure(w, r, err)
		return
	}
	view := projectsView{ExecNotice: !h.config.ActivationEnabled}
	if h.manualDeployEnabled() {
		csrf, csrfErr := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
		if csrfErr != nil {
			plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
			return
		}
		view.CanDeploy, view.CSRF = true, csrf
	}
	for _, pr := range projects {
		row := projectRow{
			ProjectID:      pr.ProjectID,
			ProjectHref:    deploymentsHref(postgres.DeploymentFilter{ProjectID: pr.ProjectID}, ""),
			Repository:     orDash(pr.RepositoryFullName),
			DeployRef:      orDash(pr.RepositoryDefaultRef),
			BindingState:   "no binding",
			Environments:   environmentLinks(pr.EnvironmentIDs),
			NoEnvironments: len(pr.EnvironmentIDs) == 0,
		}
		if pr.HasBinding {
			row.BindingState = "inactive"
			if pr.BindingActive {
				row.BindingState = "active"
			}
		}
		if d := pr.LatestDeployment; d != nil {
			row.HasLatest = true
			row.LatestStatus = d.Status
			row.LatestTone = stateTone(d.Status)
			row.LatestCommit = d.ShortCommit()
			row.LatestTime = formatTimestamp(d.CreatedAt)
			row.LatestDetailURL = detailHref(d.DeploymentID)
		}
		if view.CanDeploy && pr.HasBinding && pr.BindingActive && pr.RepositoryDefaultRef != "" {
			nonce, nonceErr := newManualNonce()
			if nonceErr != nil {
				plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
				return
			}
			row.CanDeploy, row.Nonce = true, nonce
		}
		view.Projects = append(view.Projects, row)
	}
	renderOperations(w, projectsTemplate, view)
}

// ---- /deployments ----

type deploymentRow struct {
	ID              string
	DetailHref      string
	ProjectID       string
	EnvironmentID   string
	ConfigHref      string
	RouteHref       string
	ServerID        string
	Commit          string
	Status          string
	StatusTone      string
	JobState        string
	JobTone         string
	CancelRequested bool
	CreatedAt       string
	UpdatedAt       string
}

type deploymentsView struct {
	ExecNotice    bool
	Deployments   []deploymentRow
	ProjectID     string
	EnvironmentID string
	HasOlder      bool
	OlderHref     string
	ClearHref     string
	HasFilter     bool
}

func newDeploymentRow(d postgres.DeploymentSummary) deploymentRow {
	return deploymentRow{
		ID: d.DeploymentID, DetailHref: detailHref(d.DeploymentID),
		ProjectID: d.ProjectID, EnvironmentID: d.EnvironmentID, ServerID: d.ServerID,
		ConfigHref: newEnvironmentLink(d.EnvironmentID).ConfigHref, RouteHref: newEnvironmentLink(d.EnvironmentID).RouteHref,
		Commit: d.ShortCommit(), Status: d.Status, StatusTone: stateTone(d.Status),
		JobState: d.JobState, JobTone: stateTone(d.JobState), CancelRequested: d.CancelRequested,
		CreatedAt: formatTimestamp(d.CreatedAt), UpdatedAt: formatTimestamp(d.UpdatedAt),
	}
}

func (h *Handler) deploymentsList(w http.ResponseWriter, r *http.Request) {
	p, ok := h.operationsPrelude(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if !onlyParams(q, "project_id", "environment_id", "after") {
		http.NotFound(w, r)
		return
	}
	projectID, ok1 := singleParam(q, "project_id")
	environmentID, ok2 := singleParam(q, "environment_id")
	after, ok3 := singleParam(q, "after")
	if !ok1 || !ok2 || !ok3 || len(projectID) > maxOperationsParamBytes || len(environmentID) > maxOperationsParamBytes {
		badRequest(w)
		return
	}
	filter := postgres.DeploymentFilter{ProjectID: projectID, EnvironmentID: environmentID}
	page := postgres.DeploymentPage{}
	if _, present := q["after"]; present {
		cursor, valid := decodeDeploymentCursor(after)
		if !valid {
			badRequest(w)
			return
		}
		page.After = &cursor
	}
	list, err := h.config.OperationsReader.ListDeployments(r.Context(), p, filter, page)
	if err != nil {
		operationsFailure(w, r, err)
		return
	}
	view := deploymentsView{ExecNotice: !h.config.ActivationEnabled, ProjectID: projectID, EnvironmentID: environmentID, HasFilter: projectID != "" || environmentID != "", ClearHref: "/deployments"}
	for _, d := range list.Items {
		view.Deployments = append(view.Deployments, newDeploymentRow(d))
	}
	if list.Next != nil {
		view.HasOlder = true
		view.OlderHref = deploymentsHref(filter, encodeDeploymentCursor(*list.Next))
	}
	renderOperations(w, deploymentsTemplate, view)
}

// ---- /deployments/detail ----

type statusEventRow struct{ At, From, To, Revision string }
type jobEventRow struct{ At, Sequence, Type, Attempt string }
type attemptRow struct {
	Number, State, Tone, FailureClass, Fence, Strategy, Created, Closed string
}
type instanceRow struct{ ID, Attempt, Fence, State, Tone, Created string }

type deploymentDetailView struct {
	ExecNotice   bool
	Title        string
	Refresh      bool
	RefreshEvery string
	Summary      deploymentRow
	Active       bool
	Drain        *drainResultView
	Cleanup      *cleanupView
	JobID        string
	JobAttempts  string
	JobCreated   string
	JobUpdated   string
	StatusEvents []statusEventRow
	JobEvents    []jobEventRow
	Attempts     []attemptRow
	HasRelease   bool
	Release      struct{ ID, State, Tone, Strategy, Commit, Digest, Created, Updated string }
	Instances    []instanceRow
	BackHref     string
	// LogsHref is set only when a DeploymentLogReader is configured.
	LogsHref string
	// CanCancel renders the Cancel form; CSRF is set only then.
	CanCancel         bool
	CSRF              string
	CancelRequestedAt string
	// CanRetry renders the Retry form (CSRF and RetryNonce are set); RetryMoved
	// renders the explanatory note instead when the project's binding no longer
	// targets this deployment's environment.
	CanRetry    bool
	RetryMoved  bool
	RetryNonce  string
	RetryCommit string
	// CanRollback renders the Roll back form (step 20D) for a retained release
	// the store reports eligible.
	CanRollback       bool
	RollbackCSRF      string
	RollbackNonce     string
	RollbackReleaseID string
}

// retryableStatuses are the terminal statuses from which a deployment may be
// retried: the same commit is re-deployed through POST /deployments/manual.
var retryableStatuses = map[string]bool{"failed": true, "cancelled": true, "superseded": true}

// cancellableJobStates are the job states RequestCancellation accepts as a
// new request: queued/retry_wait cancel immediately, leased cooperatively.
// Terminal states are refused by the store and so never offered.
var cancellableJobStates = map[string]bool{"queued": true, "retry_wait": true, "leased": true}

// canOfferCancel reports whether the detail page should show the Cancel form.
// A deployment already activating is refused by the store, so it is hidden.
func canOfferCancel(d postgres.DeploymentDetail) bool {
	return cancellableJobStates[d.Job.State] && d.Job.CancelRequestedAt == nil && d.Deployment.Status != "activating"
}

// offerRetry decides whether the Retry form is shown (current binding only,
// owner decision A-D2) and fills the view. It returns false after writing an
// error response.
func (h *Handler) offerRetry(w http.ResponseWriter, r *http.Request, p auth.Principal, d postgres.DeploymentDetail, view *deploymentDetailView) bool {
	projects, err := h.config.OperationsReader.ListProjects(r.Context(), p)
	if err != nil {
		operationsFailure(w, r, err)
		return false
	}
	current := false
	for _, pr := range projects {
		if pr.ProjectID == d.Deployment.ProjectID && pr.HasBinding && pr.BindingActive && pr.BindingEnvironmentID == d.Deployment.EnvironmentID {
			current = true
			break
		}
	}
	if !current {
		view.RetryMoved = true
		return true
	}
	csrf, csrfErr := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	nonce, nonceErr := newManualNonce()
	if csrfErr != nil || nonceErr != nil {
		plain(w, http.StatusInternalServerError, "the page could not be loaded; retry\n")
		return false
	}
	view.CanRetry, view.CSRF, view.RetryNonce, view.RetryCommit = true, csrf, nonce, d.Deployment.CommitSHA
	return true
}

func (h *Handler) deploymentDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := h.operationsPrelude(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if !onlyParams(q, "id") {
		http.NotFound(w, r)
		return
	}
	id, single := singleParam(q, "id")
	if !single || !validOperationsID(id) {
		http.NotFound(w, r)
		return
	}
	d, err := h.config.OperationsReader.GetDeploymentDetail(r.Context(), p, id)
	if err != nil {
		if errors.Is(err, postgres.ErrOperationsInvalidInput) {
			err = postgres.ErrOperationsNotFound // never distinguish malformed from absent
		}
		operationsFailure(w, r, err)
		return
	}
	view := deploymentDetailView{
		ExecNotice: !h.config.ActivationEnabled,
		Title:      "Deployment " + d.Deployment.DeploymentID, Summary: newDeploymentRow(d.Deployment),
		Refresh: !terminalJobStates[d.Job.State], RefreshEvery: strconv.Itoa(detailRefreshSeconds),
		Active: d.ReleaseIsActive, Drain: newDrainResultView(d.PredecessorDrain), Cleanup: newCleanupView(d.ReleaseCleanup), JobID: strconv.FormatInt(d.Job.ID, 10), JobAttempts: strconv.Itoa(d.Job.AttemptCount),
		JobCreated: formatTimestamp(d.Job.CreatedAt), JobUpdated: formatTimestamp(d.Job.UpdatedAt),
		BackHref: deploymentsHref(postgres.DeploymentFilter{ProjectID: d.Deployment.ProjectID}, ""),
	}
	if h.config.DeploymentLogReader != nil {
		view.LogsHref = logsHref(id, 0)
	}
	if d.Job.CancelRequestedAt != nil {
		view.CancelRequestedAt = formatTimestamp(*d.Job.CancelRequestedAt)
	}
	if h.config.DeploymentCanceller != nil && canOfferCancel(d) {
		csrf, csrfErr := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
		if csrfErr != nil {
			plain(w, http.StatusInternalServerError, "the page could not be loaded; retry\n")
			return
		}
		view.CanCancel, view.CSRF = true, csrf
	}
	if h.manualDeployEnabled() && retryableStatuses[d.Deployment.Status] {
		if !h.offerRetry(w, r, p, d, &view) {
			return
		}
	}
	if !h.offerRollback(w, r, p, d, &view) {
		return
	}
	for _, e := range d.StatusEvents {
		view.StatusEvents = append(view.StatusEvents, statusEventRow{At: formatTimestamp(e.OccurredAt), From: orDash(e.FromStatus), To: e.ToStatus, Revision: strconv.FormatInt(e.Revision, 10)})
	}
	for _, e := range d.JobEvents {
		attempt := "-"
		if e.AttemptID != nil {
			attempt = strconv.FormatInt(*e.AttemptID, 10)
		}
		view.JobEvents = append(view.JobEvents, jobEventRow{At: formatTimestamp(e.OccurredAt), Sequence: strconv.Itoa(e.Sequence), Type: e.EventType, Attempt: attempt})
	}
	for _, a := range d.Attempts {
		closed := "open"
		if a.ClosedAt != nil {
			closed = formatTimestamp(*a.ClosedAt)
		}
		view.Attempts = append(view.Attempts, attemptRow{Number: strconv.Itoa(a.AttemptNumber), State: a.State, Tone: stateTone(a.State), FailureClass: orDash(a.FailureClass), Fence: strconv.FormatInt(a.Fence, 10), Strategy: orDash(a.ReleaseStrategy), Created: formatTimestamp(a.CreatedAt), Closed: closed})
	}
	if rel := d.Release; rel != nil {
		view.HasRelease = true
		view.Release.ID, view.Release.State, view.Release.Tone = rel.ID, rel.State, stateTone(rel.State)
		view.Release.Strategy, view.Release.Commit = orDash(rel.Strategy), rel.CommitSHA
		view.Release.Digest = orDash(shortDigest(rel.ImageDigest))
		view.Release.Created, view.Release.Updated = formatTimestamp(rel.CreatedAt), formatTimestamp(rel.UpdatedAt)
	}
	for _, in := range d.RuntimeInstances {
		view.Instances = append(view.Instances, instanceRow{ID: strconv.FormatInt(in.ID, 10), Attempt: strconv.FormatInt(in.AttemptID, 10), Fence: strconv.FormatInt(in.Fence, 10), State: in.ObservedState, Tone: stateTone(in.ObservedState), Created: formatTimestamp(in.CreatedAt)})
	}
	renderOperations(w, deploymentDetailTemplate, view)
}

// deploymentCancel requests cancellation of a deployment's job. The job id is
// resolved server-side through the owner-scoped detail read, never taken from
// the client. Checks mirror serversRevoke: provenance, strict form, session,
// CSRF, explicit confirm. Failures are fixed generic texts.
func (h *Handler) deploymentCancel(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 3 || len(v["csrf"]) != 1 || len(v["deployment_id"]) != 1 || len(v["confirm"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	id := v.Get("deployment_id")
	if v.Get("confirm") != "cancel" || !validOperationsID(id) {
		badRequest(w)
		return
	}
	d, err := h.config.OperationsReader.GetDeploymentDetail(r.Context(), p, id)
	if err != nil {
		if errors.Is(err, postgres.ErrOperationsInvalidInput) {
			err = postgres.ErrOperationsNotFound
		}
		operationsFailure(w, r, err)
		return
	}
	switch err = h.config.DeploymentCanceller.RequestCancellation(r.Context(), p, d.Job.ID); {
	case err == nil:
		redirect(w, detailHref(id))
	case errors.Is(err, postgres.ErrAgentJobTerminal):
		plain(w, http.StatusConflict, "this deployment has already finished and cannot be cancelled\n")
	case errors.Is(err, postgres.ErrAgentJobActivating):
		plain(w, http.StatusConflict, "this deployment is activating and can no longer be cancelled\n")
	case errors.Is(err, postgres.ErrServerAuthorizationDenied):
		badRequest(w)
	default:
		plain(w, http.StatusInternalServerError, "the deployment could not be cancelled; retry\n")
	}
}
