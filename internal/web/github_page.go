package web

import (
	"net/http"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

var githubTemplate = parsePage("github")

type githubServer struct{ ID, Name string }

// githubBindingRow is one bound project on /github.
type githubBindingRow struct {
	ProjectID, Repository, DeployRef, EnvironmentID, ServerID string
	State, StateTone                                          string
}

// githubView is the GitHub page's data. The forms moved verbatim from the
// Overview (22-3g): the install and refresh forms always render, the binding
// form only when GitHub setup is configured (ShowBind).
type githubView struct {
	Nav          navView
	CSRF         string
	ShowBind     bool
	Repositories []postgres.BindableRepository
	Servers      []githubServer

	// HasBindings is true when an OperationsReader can list projects.
	HasBindings         bool
	BindingsUnavailable bool
	Bindings            []githubBindingRow
}

// githubPage renders the install, refresh and bind forms plus the bindings
// table. Only the session is required: every read below degrades on failure so
// the forms (the page's purpose) always render.
func (h *Handler) githubPage(w http.ResponseWriter, r *http.Request) {
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
	view := githubView{Nav: h.navFor(r.Context(), "github", principal), CSRF: csrf, ShowBind: h.config.GitHubSetup != nil}
	if view.ShowBind {
		facts := h.loadSetupFacts(r.Context(), principal)
		view.Repositories = facts.repos
		for _, srv := range facts.servers {
			view.Servers = append(view.Servers, githubServer{ID: srv.ID, Name: srv.Name})
		}
	}
	if h.config.OperationsReader != nil {
		view.HasBindings = true
		projects, listErr := h.config.OperationsReader.ListProjects(r.Context(), principal)
		if listErr != nil {
			view.BindingsUnavailable = true
		}
		for _, pr := range projects {
			if !pr.HasBinding {
				continue
			}
			row := githubBindingRow{
				ProjectID: pr.ProjectID, Repository: orDash(pr.RepositoryFullName), DeployRef: orDash(pr.RepositoryDefaultRef),
				EnvironmentID: orDash(pr.BindingEnvironmentID), ServerID: orDash(pr.BindingServerID),
				State: "inactive", StateTone: "neutral",
			}
			if pr.BindingActive {
				row.State, row.StateTone = "active", "ok"
			}
			view.Bindings = append(view.Bindings, row)
		}
	}
	renderOperations(w, githubTemplate, view)
}
