package web

import (
	"net/http"
	"strconv"

	"autodeploy/internal/auth"
)

var reconciliationTemplate = parsePage("reconciliation")

// reconciliationView is the Reconciliation page's data. With blocked
// environments it is the resolve table; with none it shows every environment's
// activation state (Environments), or, without the overview read, the servers
// summary, so the page is never text-only.
type reconciliationView struct {
	Nav  navView
	CSRF string
	// Blocked is the reconciling-environments table; ResolveReasonMax is the
	// resolve form's maxlength (UTF-16 units, so only a hint:
	// environmentsResolve enforces maxResolveReasonBytes in bytes).
	Blocked          []reconcilingRow
	ResolveReasonMax int

	HasEnvironments    bool
	EnvironmentsFailed bool
	Environments       []boardRow
	HasServers         bool
	ServersFailed      bool
	ServerRows         []overviewServer
	// Checklist is shown instead of a text-only empty paragraph.
	Checklist *checklistView
}

// reconciliationPage renders the environments blocked on reconciliation with
// their inline resolve forms. GET only; it mutates nothing.
func (h *Handler) reconciliationPage(w http.ResponseWriter, r *http.Request) {
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
	blocked, err := h.config.ServerAdmin.ListReconcilingEnvironments(r.Context(), p)
	if err != nil {
		badRequest(w)
		return
	}
	view := reconciliationView{Nav: h.navForCount("reconciliation", p, len(blocked)), CSRF: csrf, ResolveReasonMax: maxResolveReasonBytes}
	for _, e := range blocked {
		link := newEnvironmentLink(e.EnvironmentID)
		view.Blocked = append(view.Blocked, reconcilingRow{EnvironmentID: e.EnvironmentID, ConfigHref: link.ConfigHref, RouteHref: link.RouteHref, AttemptID: strconv.FormatInt(e.AttemptID, 10), JobID: strconv.FormatInt(e.JobID, 10), ServerID: e.ServerID, Since: formatTimestamp(e.Since), Activation: activationLabel(e.Activation)})
	}
	if len(blocked) == 0 {
		if reader, ok := h.overviewReader(); ok {
			view.HasEnvironments = true
			rows, readErr := reader.ListEnvironmentOverview(r.Context(), p)
			if readErr == nil {
				view.Environments, readErr = h.buildBoard(rows, csrf, "/reconciliation")
			}
			view.EnvironmentsFailed = readErr != nil
		} else {
			view.HasServers = true
			servers, listErr := h.config.ServerAdmin.ListServers(r.Context(), p)
			view.ServersFailed = listErr != nil
			view.ServerRows = h.overviewServers(servers)
		}
	}
	if len(blocked) == 0 && ((view.HasEnvironments && !view.EnvironmentsFailed && len(view.Environments) == 0) || (view.HasServers && !view.ServersFailed && len(view.ServerRows) == 0)) {
		view.Checklist = h.setupChecklist(r.Context(), p, false)
	}
	renderOperations(w, reconciliationTemplate, view)
}
