package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// DeploymentLogReader is the narrow read-only surface behind
// /deployments/logs (step 15g-5). A nil reader 404s the route and hides the
// Logs link. *postgres.DeploymentLogRepository satisfies it.
type DeploymentLogReader interface {
	ListDeploymentLogs(ctx context.Context, principal auth.Principal, deploymentID string, afterID int64, limit int) (postgres.DeploymentLogPage, error)
}

var logsTemplate = parsePage("deployment-logs")

// knownLogStreams is the closed set of stream names allowed into a class
// attribute; anything else renders as the neutral "other".
var knownLogStreams = map[string]bool{"build": true, "phase": true, "health": true, "activation": true, "drain": true, "system": true}

func logStreamClass(stream string) string {
	if knownLogStreams[stream] {
		return stream
	}
	return "other"
}

// isLogMarker reports whether a system-stream row is a truncation or
// dropped-lines marker (store cap marker; agent overflow marker).
// agentDropMarkerPrefix mirrors logship.DroppedMarkerPrefix; a test pins them
// equal so the control plane need not import agent code.
const agentDropMarkerPrefix = "[logs dropped: "

func isLogMarker(stream, body string) bool {
	if stream != "system" {
		return false
	}
	return body == postgres.LogTruncationMarker || strings.HasPrefix(body, agentDropMarkerPrefix)
}

func logsHref(deploymentID string, after int64) string {
	v := url.Values{"id": {deploymentID}}
	if after > 0 {
		v.Set("after", strconv.FormatInt(after, 10))
	}
	return "/deployments/logs?" + v.Encode()
}

type logLineRow struct {
	Stamp, Clock, StreamClass, StreamLabel, Body string
	Marker                                       bool
}

type logGroup struct {
	Number string
	Lines  []logLineRow
}

type deploymentLogsView struct {
	Nav        navView
	ExecNotice bool
	Title      string
	ID         string
	DetailHref string
	Groups     []logGroup
	NewerHref  string
	StartHref  string
	FollowHref string
	Refresh    bool
	// Picker is the newest deployments as <select> options (nil when unavailable);
	// Empty carries the zero-deployments state.
	Picker []deploymentOption
	Empty  *checklistView
}

// logPickerLimit is the number of newest deployments the log picker offers.
const logPickerLimit = 20

// deploymentOption is one entry of the log picker.
type deploymentOption struct {
	ID, Label string
	Selected  bool
}

// deploymentOptions labels deployments "environment · commit · status · created".
// The current deployment is always present and selected, even when it is older
// than the newest list.
func deploymentOptions(items []postgres.DeploymentSummary, current string) []deploymentOption {
	if len(items) > logPickerLimit {
		items = items[:logPickerLimit]
	}
	options := make([]deploymentOption, 0, len(items)+1)
	found := false
	for _, d := range items {
		sel := d.DeploymentID == current
		found = found || sel
		options = append(options, deploymentOption{
			ID: d.DeploymentID, Selected: sel,
			Label: d.EnvironmentID + " \u00b7 " + d.ShortCommit() + " \u00b7 " + d.Status + " \u00b7 " + formatTimestamp(d.CreatedAt),
		})
	}
	if !found {
		options = append([]deploymentOption{{ID: current, Label: current, Selected: true}}, options...)
	}
	return options
}

// logPicker lists the newest deployments around an explicit id. The picker is a
// convenience, so any failure (or a missing reader) only omits it.
func (h *Handler) logPicker(r *http.Request, p auth.Principal, current string) []deploymentOption {
	if h.config.OperationsReader == nil {
		return nil
	}
	list, err := h.config.OperationsReader.ListDeployments(r.Context(), p, postgres.DeploymentFilter{}, postgres.DeploymentPage{Limit: logPickerLimit})
	if err != nil {
		h.logger().Debug("log picker: listing deployments failed", "error", err)
		return nil
	}
	return deploymentOptions(list.Items, current)
}

// renderLogsEmpty renders the page for an owner with no deployments: the setup
// checklist, never a bare sentence.
func (h *Handler) renderLogsEmpty(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	facts := h.loadSetupFacts(r.Context(), p)
	bound := false
	if reader, ok := h.overviewReader(); ok {
		if rows, err := reader.ListEnvironmentOverview(r.Context(), p); err == nil {
			bound = anyBound(rows)
		}
	}
	view := deploymentLogsView{Nav: h.navFor(r.Context(), "logs", p), ExecNotice: !h.config.ActivationEnabled, Title: "Logs", Empty: h.newChecklist(facts.repoCount > 0, facts.hasActiveServer(), bound, 0)}
	renderOperations(w, logsTemplate, view)
}

// deploymentLogs renders one page of a deployment's log chunks.
//
// Tail refresh rule: meta-refresh (3 s) only when the job is non-terminal AND
// this is the last page (Next == nil). A bare meta-refresh reloads the current
// URL, so the page keeps its own `after` (not Next) and rows that commit late
// inside this window still appear.
//
// Keyset gap (15g-1 note): the cursor is the chunk id, and an overlapping
// attempt can commit a lower id late. A page without `after` always renders
// from the start, and any page with `after` offers a "From the start" link;
// terminal jobs never refresh, so a reload of the start page is complete.
func (h *Handler) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.operationsPrelude(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if !onlyParams(q, "id", "after") {
		http.NotFound(w, r)
		return
	}
	id, single := singleParam(q, "id")
	_, hasID := q["id"]
	if hasID && (!single || !validOperationsID(id)) {
		http.NotFound(w, r)
		return
	}
	rawAfter, single := singleParam(q, "after")
	if !single {
		badRequest(w)
		return
	}
	var after int64
	_, hasAfter := q["after"]
	if hasAfter {
		n, valid := parseChunkCursor(rawAfter)
		if !valid {
			badRequest(w)
			return
		}
		after = n
	}
	var picker []deploymentOption
	if !hasID {
		// 22-3h: id is optional; default to the newest deployment. A cursor
		// belongs to one deployment, so `after` without `id` is meaningless.
		if hasAfter {
			badRequest(w)
			return
		}
		if h.config.OperationsReader == nil {
			http.NotFound(w, r)
			return
		}
		list, err := h.config.OperationsReader.ListDeployments(r.Context(), p, postgres.DeploymentFilter{}, postgres.DeploymentPage{Limit: logPickerLimit})
		if err != nil {
			operationsFailure(w, r, err)
			return
		}
		if len(list.Items) == 0 {
			h.renderLogsEmpty(w, r, p)
			return
		}
		id = list.Items[0].DeploymentID
		picker = deploymentOptions(list.Items, id)
	}
	page, err := h.config.DeploymentLogReader.ListDeploymentLogs(r.Context(), p, id, after, 0)
	if err != nil {
		if errors.Is(err, postgres.ErrOperationsInvalidInput) {
			err = postgres.ErrOperationsNotFound // never distinguish malformed from absent
		}
		operationsFailure(w, r, err)
		return
	}
	if hasID {
		picker = h.logPicker(r, p, id)
	}
	view := deploymentLogsView{Nav: h.navFor(r.Context(), "logs", p), ExecNotice: !h.config.ActivationEnabled, Title: "Logs for deployment " + id, ID: id, DetailHref: detailHref(id), Picker: picker}
	if after > 0 {
		view.StartHref = logsHref(id, 0)
	}
	for _, l := range page.Items {
		number := strconv.Itoa(l.AttemptNumber)
		if n := len(view.Groups); n == 0 || view.Groups[n-1].Number != number {
			view.Groups = append(view.Groups, logGroup{Number: number})
		}
		g := &view.Groups[len(view.Groups)-1]
		g.Lines = append(g.Lines, logLineRow{
			Stamp: l.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), Clock: l.CreatedAt.UTC().Format("15:04:05.000"),
			StreamClass: logStreamClass(l.Stream), StreamLabel: logStreamClass(l.Stream), Body: l.Body,
			Marker: isLogMarker(l.Stream, l.Body),
		})
	}
	if page.Next != nil {
		view.NewerHref = logsHref(id, *page.Next)
	} else if h.config.OperationsReader != nil {
		// Best effort: any failure here only means no auto-refresh.
		if d, derr := h.config.OperationsReader.GetDeploymentDetail(r.Context(), p, id); derr == nil {
			live := !terminalJobStates[d.Job.State]
			if hasID {
				view.Refresh = live
			} else if live {
				// No-id mode resolves to the newest deployment on every load, so
				// an auto-refresh could jump to a newer one; offer an explicit
				// link that follows this deployment instead.
				view.FollowHref = logsHref(id, 0)
			}
		}
	}
	renderOperations(w, logsTemplate, view)
}

// parseChunkCursor strictly parses a non-negative decimal int64: digits only,
// no sign, no leading zeros except "0", bounded length.
func parseChunkCursor(s string) (int64, bool) {
	if s == "" || len(s) > 19 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}
