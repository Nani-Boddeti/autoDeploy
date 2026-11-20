package web

import "autodeploy/internal/auth"

// navDef is one sidebar entry. enabled reports whether the dependency behind the
// page is configured (a nil dependency makes the route 404, so the link is omitted).
// Later slices append their items here; Group order is the order of first appearance.
type navDef struct {
	Key, Label, Href, Group string
	enabled                 func(*Handler) bool
}

func always(*Handler) bool { return true }

func hasOperations(h *Handler) bool { return h.config.OperationsReader != nil }

func hasOverview(h *Handler) bool {
	_, ok := h.overviewReader()
	return ok
}

func hasServers(h *Handler) bool { return h.config.ServerAdmin != nil }

var navItems = []navDef{
	{Key: "home", Label: "Overview", Href: "/", Group: "Operate", enabled: always},
	{Key: "environments", Label: "Environments", Href: "/environments", Group: "Operate", enabled: hasOverview},
	{Key: "deployments", Label: "Deployments", Href: "/deployments", Group: "Operate", enabled: hasOperations},
	{Key: "projects", Label: "Projects", Href: "/projects", Group: "Configure", enabled: hasOperations},
	{Key: "github", Label: "GitHub", Href: "/#github", Group: "Configure", enabled: always},
	{Key: "servers", Label: "Servers", Href: "/servers", Group: "Infrastructure", enabled: hasServers},
}

type navItem struct {
	Key, Label, Href, Group string
	Current                 bool
}

type navGroup struct {
	Label string
	Items []navItem
}

// navView is the argument of the "nav" template. CSRF is empty when no session
// token could be minted; the sidebar then omits the sign-out form.
type navView struct {
	Groups []navGroup
	CSRF   string
}

// Named views for the flow-only pages, so each carries its Nav.
type enrollmentView struct {
	Nav                        navView
	ServerID, Token, ExpiresAt string
}

type githubCompleteView struct {
	Nav  navView
	CSRF string
}

type githubContinueView struct {
	Nav navView
}

// navFor builds the sidebar for the page identified by active. A zero principal
// (the GitHub callback arrives cross-site without a session cookie) yields a nav
// without the sign-out form.
func (h *Handler) navFor(active string, p auth.Principal) navView {
	var view navView
	index := map[string]int{}
	for _, def := range navItems {
		if !def.enabled(h) {
			continue
		}
		i, ok := index[def.Group]
		if !ok {
			i = len(view.Groups)
			index[def.Group] = i
			view.Groups = append(view.Groups, navGroup{Label: def.Group})
		}
		view.Groups[i].Items = append(view.Groups[i].Items, navItem{Key: def.Key, Label: def.Label, Href: def.Href, Group: def.Group, Current: def.Key == active})
	}
	if p.SessionID != "" {
		if token, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin); err == nil {
			view.CSRF = token
		}
	}
	return view
}
