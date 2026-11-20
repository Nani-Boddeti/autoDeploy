package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// EnvironmentOverviewReader is the optional read surface behind the Overview
// board and the Environments page (22-3d). *postgres.OperationsReadRepository
// satisfies it; like EnvironmentReleaseReader it is discovered on
// Config.OperationsReader. Without it /environments 404s and the Overview
// renders only the setup checklist, servers and GitHub sections.
type EnvironmentOverviewReader interface {
	ListEnvironmentOverview(ctx context.Context, principal auth.Principal) ([]postgres.EnvironmentOverview, error)
}

func (h *Handler) overviewReader() (EnvironmentOverviewReader, bool) {
	reader, ok := h.config.OperationsReader.(EnvironmentOverviewReader)
	return reader, ok
}

const (
	// heartbeatFreshFor is how recent a heartbeat must be to read as ok.
	heartbeatFreshFor = 2 * time.Minute
	// heartbeatAttentionAfter is the silence after which a server needs attention.
	heartbeatAttentionAfter = 5 * time.Minute
	// overviewRecentLimit is the number of recent deployments on the Overview.
	overviewRecentLimit = 10
)

var environmentsTemplate = parsePage("environments")

// returnToAllowlist is the exact set of post-action redirect targets a board
// form may request. Anything else (other paths, query strings, schemes,
// protocol-relative or backslash forms) falls back to the handler's default.
var returnToAllowlist = map[string]bool{"/": true, "/environments": true, "/servers": true}

// takeReturnTo removes the optional return_to field from v (so the strict
// field-count checks of the POST handlers stay unchanged) and returns it only
// when it is exactly one allowlisted target; "" means use the default.
func takeReturnTo(v url.Values) string {
	values := v["return_to"]
	delete(v, "return_to")
	if len(values) == 1 && returnToAllowlist[values[0]] {
		return values[0]
	}
	return ""
}

// redirectAfter redirects to returnTo when set, else to the default target.
func redirectAfter(w http.ResponseWriter, returnTo, fallback string) {
	if returnTo != "" {
		redirect(w, returnTo)
		return
	}
	redirect(w, fallback)
}

// humanSpan renders a duration coarsely ("5m", "3h", "2d").
func humanSpan(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// humanAge renders a coarse age ("just now", "5m ago", "3h ago", "2d ago").
func humanAge(now, t time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return humanSpan(d) + " ago"
}

func shortCommit(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// heartbeatState maps a server's status and last heartbeat to a pill tone and text.
func heartbeatState(now time.Time, status string, last time.Time) (tone, text string) {
	switch {
	case status == "revoked":
		return "bad", "revoked"
	case last.IsZero():
		return "warn", "never"
	case now.Sub(last) < heartbeatFreshFor:
		return "ok", "ok · " + humanAge(now, last)
	default:
		return "warn", "stale · " + humanAge(now, last)
	}
}

// boardLatest is the last deployment of an environment.
type boardLatest struct {
	Status, Tone, Commit, Time, DetailHref string
	Reason                                 reasonView
}

// boardRow is one environment on the Overview board and the Environments table.
// The action fields feed the shared "rowactions" partial.
type boardRow struct {
	EnvironmentID, ProjectID                 string
	ConfigHref, RouteHref, DeploymentsHref   string
	LogsHref                                 string
	Hostname, HostURL                        string
	HasServer                                bool
	ServerName, HeartbeatTone, HeartbeatText string
	Strategy, RouteRevision                  string
	HasActive                                bool
	ActiveReleaseID, ActiveCommit, ActiveAge string
	ActiveSince                              string
	Latest                                   *boardLatest
	ReconciliationRequired                   bool
	CSRF, ReturnTo                           string
	CanDeploy                                bool
	DeployNonce                              string
	CanRollback                              bool
	RollbackReleaseID, RollbackCommit        string
	RollbackNonce                            string
	CanResolve                               bool
	AttemptID                                string
	ResolveReasonMax                         int
}

// buildBoard converts store rows to board rows. Each Deploy and Roll back form
// gets its own fresh nonce. Forms are offered only when their dependency is
// configured and the store row allows them; the POST handlers re-check
// everything under locks.
func (h *Handler) buildBoard(rows []postgres.EnvironmentOverview, csrf, returnTo string) ([]boardRow, error) {
	now := h.config.Now()
	logsEnabled := h.config.DeploymentLogReader != nil
	board := make([]boardRow, 0, len(rows))
	for _, e := range rows {
		link := newEnvironmentLink(e.EnvironmentID)
		row := boardRow{
			EnvironmentID: e.EnvironmentID, ProjectID: e.ProjectID,
			ConfigHref: link.ConfigHref, RouteHref: link.RouteHref,
			DeploymentsHref:        deploymentsHref(postgres.DeploymentFilter{EnvironmentID: e.EnvironmentID}, ""),
			Hostname:               e.Hostname,
			ReconciliationRequired: e.ReconciliationRequired,
			CSRF:                   csrf, ReturnTo: returnTo, ResolveReasonMax: maxResolveReasonBytes,
		}
		if e.Hostname != "" {
			row.HostURL = "https://" + e.Hostname
		}
		if e.ServerID != "" {
			row.HasServer = true
			row.ServerName = e.ServerName
			if row.ServerName == "" {
				row.ServerName = e.ServerID
			}
			row.HeartbeatTone, row.HeartbeatText = heartbeatState(now, e.ServerStatus, e.LastHeartbeatAt)
		}
		row.Strategy = orDash(e.Strategy)
		row.RouteRevision = "-"
		if e.RouteRevision != 0 {
			row.RouteRevision = strconv.FormatInt(e.RouteRevision, 10)
		}
		if e.ActiveReleaseID != "" {
			row.HasActive = true
			row.ActiveReleaseID, row.ActiveCommit = e.ActiveReleaseID, shortCommit(e.ActiveCommit)
			if !e.ActiveSince.IsZero() {
				row.ActiveAge, row.ActiveSince = humanAge(now, e.ActiveSince), formatTimestamp(e.ActiveSince)
			}
		}
		if d := e.Latest; d != nil {
			class, reason := e.LatestFailureClass, e.LatestFailureReason
			if class == "" {
				class = d.FailureClass
			}
			if reason == "" {
				reason = d.FailureReason
			}
			row.Latest = &boardLatest{
				Status: d.Status, Tone: stateTone(d.Status), Commit: d.ShortCommit(),
				Time: formatTimestamp(d.CreatedAt), DetailHref: detailHref(d.DeploymentID),
				Reason: failedReason(d.Status, class, reason),
			}
			if logsEnabled {
				row.LogsHref = "/deployments/logs?" + url.Values{"id": {d.DeploymentID}}.Encode()
			}
		}
		var err error
		if h.manualDeployEnabled() && e.Bound && e.BindingActive {
			row.CanDeploy = true
			if row.DeployNonce, err = newManualNonce(); err != nil {
				return nil, fmt.Errorf("nonce: %w", err)
			}
		}
		if e.RollbackTargetReleaseID != "" && h.config.Rollback != nil {
			row.CanRollback = true
			row.RollbackReleaseID, row.RollbackCommit = e.RollbackTargetReleaseID, shortCommit(e.RollbackTargetCommit)
			if row.RollbackNonce, err = newManualNonce(); err != nil {
				return nil, fmt.Errorf("nonce: %w", err)
			}
		}
		if e.ReconciliationRequired && h.config.ServerAdmin != nil && e.ReconcilingAttemptID > 0 {
			row.CanResolve = true
			row.AttemptID = strconv.FormatInt(e.ReconcilingAttemptID, 10)
		}
		board = append(board, row)
	}
	return board, nil
}

// checklistStep and checklistView drive the "checklist" partial.
type checklistStep struct {
	Title, Hint, Href string
	Done              bool
}

type checklistView struct {
	Steps   []checklistStep
	Compact bool
}

// newChecklist returns the setup checklist, or nil when nothing is left to do
// and at least one environment exists. With zero environments it is the main
// content (never nil); otherwise it is compact while a step is undone.
func (h *Handler) newChecklist(githubDone, serverDone, boundDone bool, environments int) *checklistView {
	view := &checklistView{Steps: []checklistStep{
		{Title: "Connect GitHub", Hint: "Install the GitHub App so repositories can be bound.", Href: "/#github", Done: githubDone},
		{Title: "Register a server", Hint: "Enroll the machine that will run your deployments.", Href: "/servers", Done: serverDone},
		{Title: "Bind a repository", Hint: "Tie a repository branch to an environment and server.", Href: "/#github", Done: boundDone},
	}}
	allDone := githubDone && serverDone && boundDone
	if allDone && environments > 0 {
		return nil
	}
	view.Compact = environments > 0
	return view
}

// setupFacts are the optional reads behind the checklist and the servers table.
type setupFacts struct {
	servers       []postgres.ServerSummary
	serversFailed bool
	repoCount     int
	repos         []postgres.BindableRepository
}

func (h *Handler) loadSetupFacts(ctx context.Context, p auth.Principal) setupFacts {
	var f setupFacts
	if h.config.ServerAdmin != nil {
		servers, err := h.config.ServerAdmin.ListServers(ctx, p)
		if err != nil {
			f.serversFailed = true
		} else {
			f.servers = servers
		}
	}
	if h.config.RepositoryLister != nil {
		// Best effort: a failed lookup leaves the pick-list empty and the step undone.
		if repos, err := h.config.RepositoryLister.ListBindableRepositories(ctx, p); err == nil {
			f.repos, f.repoCount = repos, len(repos)
		}
	}
	return f
}

func (f setupFacts) hasActiveServer() bool {
	for _, s := range f.servers {
		if string(s.Status) == "active" {
			return true
		}
	}
	return false
}

func anyBound(rows []postgres.EnvironmentOverview) bool {
	for _, e := range rows {
		if e.Bound {
			return true
		}
	}
	return false
}

// attentionItem is one thing that needs the operator now.
type attentionItem struct {
	Kind, Subject, Text, Href, LinkText string
	Reason                              reasonView
}

// overviewServer is one row of the Overview servers table.
type overviewServer struct {
	ID, Name, Status, StatusTone, Heartbeat, HeartbeatTone, Credential string
}

func (h *Handler) overviewServers(servers []postgres.ServerSummary) []overviewServer {
	now := h.config.Now()
	out := make([]overviewServer, 0, len(servers))
	for _, s := range servers {
		tone, text := heartbeatState(now, string(s.Status), s.LastHeartbeatAt)
		tone2 := "ok"
		if string(s.Status) != "active" {
			tone2 = "bad"
		}
		out = append(out, overviewServer{
			ID: s.ID, Name: s.Name, Status: string(s.Status), StatusTone: tone2,
			Heartbeat: text, HeartbeatTone: tone, Credential: orDash(formatCredentialVersion(s.ActiveCredentialVersion)),
		})
	}
	return out
}

// attentionItems collects real problems only: blocked environments, revoked or
// silent servers and environments whose latest deployment failed. The blocked
// link targets /servers (where reconciliation lives) until /reconciliation exists.
func (h *Handler) attentionItems(rows []postgres.EnvironmentOverview, servers []postgres.ServerSummary) []attentionItem {
	now := h.config.Now()
	var items []attentionItem
	for _, e := range rows {
		if e.ReconciliationRequired {
			items = append(items, attentionItem{Kind: "Blocked", Subject: e.EnvironmentID, Text: "needs reconciliation before it can deploy again.", Href: "/servers", LinkText: "Resolve"})
		}
	}
	for _, s := range servers {
		switch {
		case string(s.Status) == "revoked":
			items = append(items, attentionItem{Kind: "Server", Subject: s.Name, Text: "is revoked.", Href: "/servers", LinkText: "Servers"})
		case s.LastHeartbeatAt.IsZero():
			items = append(items, attentionItem{Kind: "Server", Subject: s.Name, Text: "has never sent a heartbeat.", Href: "/servers", LinkText: "Servers"})
		case now.Sub(s.LastHeartbeatAt) > heartbeatAttentionAfter:
			items = append(items, attentionItem{Kind: "Server", Subject: s.Name, Text: "has sent no heartbeat for " + humanSpan(now.Sub(s.LastHeartbeatAt)) + ".", Href: "/servers", LinkText: "Servers"})
		}
	}
	for _, e := range rows {
		d := e.Latest
		if d == nil || d.Status != "failed" {
			continue
		}
		class, reason := e.LatestFailureClass, e.LatestFailureReason
		if class == "" {
			class = d.FailureClass
		}
		if reason == "" {
			reason = d.FailureReason
		}
		items = append(items, attentionItem{Kind: "Failed", Subject: e.EnvironmentID, Text: "latest deployment failed.", Href: detailHref(d.DeploymentID), LinkText: "Details", Reason: failedReason(d.Status, class, reason)})
	}
	return items
}

// ---- /environments ----

type environmentsView struct {
	Nav        navView
	ExecNotice bool
	Board      []boardRow
	Checklist  *checklistView
}

func (h *Handler) environmentsList(w http.ResponseWriter, r *http.Request) {
	p, ok := h.operationsPrelude(w, r)
	if !ok {
		return
	}
	rows, err := h.config.OperationsReader.(EnvironmentOverviewReader).ListEnvironmentOverview(r.Context(), p)
	if err != nil {
		operationsFailure(w, r, err)
		return
	}
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if err != nil {
		plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
		return
	}
	board, err := h.buildBoard(rows, csrf, "/environments")
	if err != nil {
		plain(w, http.StatusInternalServerError, "the page could not be rendered\n")
		return
	}
	view := environmentsView{Nav: h.navFor("environments", p), ExecNotice: !h.config.ActivationEnabled, Board: board}
	if len(rows) == 0 {
		facts := h.loadSetupFacts(r.Context(), p)
		view.Checklist = h.newChecklist(facts.repoCount > 0, facts.hasActiveServer(), anyBound(rows), 0)
	}
	renderOperations(w, environmentsTemplate, view)
}
