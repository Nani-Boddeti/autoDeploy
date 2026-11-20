package web

import (
	"context"

	"autodeploy/internal/auth"
)

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

// The first-class pages choose their environment from the overview read, so
// they appear only when it exists as well as their own dependency.
func hasConfigPage(h *Handler) bool { return h.config.RuntimeConfigAdmin != nil && hasOverview(h) }

func hasRoutesPage(h *Handler) bool { return h.config.RouteHeadAdmin != nil && hasOverview(h) }

func hasLogsPage(h *Handler) bool { return h.config.DeploymentLogReader != nil && hasOperations(h) }

var navItems = []navDef{
	{Key: "home", Label: "Overview", Href: "/", Group: "Operate", enabled: always},
	{Key: "environments", Label: "Environments", Href: "/environments", Group: "Operate", enabled: hasOverview},
	{Key: "deployments", Label: "Deployments", Href: "/deployments", Group: "Operate", enabled: hasOperations},
	{Key: "logs", Label: "Logs", Href: "/deployments/logs", Group: "Operate", enabled: hasLogsPage},
	{Key: "config", Label: "Runtime config", Href: "/environments/config", Group: "Configure", enabled: hasConfigPage},
	{Key: "routes", Label: "Routes", Href: "/environments/route", Group: "Configure", enabled: hasRoutesPage},
	{Key: "projects", Label: "Projects", Href: "/projects", Group: "Configure", enabled: hasOperations},
	{Key: "github", Label: "GitHub", Href: "/github", Group: "Configure", enabled: always},
	{Key: "servers", Label: "Servers", Href: "/servers", Group: "Infrastructure", enabled: hasServers},
	{Key: "reconciliation", Label: "Reconciliation", Href: "/reconciliation", Group: "Infrastructure", enabled: hasServers},
}

type navItem struct {
	Key, Label, Href, Group string
	Current                 bool
	// Badge is a count shown beside the label; 0 shows nothing.
	Badge int
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
	// ReconcilingCount is the number of blocked environments; it drives the
	// Reconciliation badge and is 0 when unknown.
	ReconcilingCount int
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
// without the sign-out form. The Reconciliation badge costs one query, made only
// when ServerAdmin is configured; handlers that already hold the list call
// navForCount instead.
func (h *Handler) navFor(ctx context.Context, active string, p auth.Principal) navView {
	return h.navForCount(active, p, h.reconcilingCount(ctx, p))
}

// reconcilingCount counts the blocked environments for the nav badge. It never
// fails the page: any error (or a missing principal or ServerAdmin) reads as 0.
func (h *Handler) reconcilingCount(ctx context.Context, p auth.Principal) int {
	if h.config.ServerAdmin == nil || p.OwnerID == "" {
		return 0
	}
	environments, err := h.config.ServerAdmin.ListReconcilingEnvironments(ctx, p)
	if err != nil {
		h.logger().Debug("nav badge: listing reconciling environments failed", "error", err)
		return 0
	}
	return len(environments)
}

// navForCount is navFor with the blocked-environment count supplied by a handler
// that already listed them.
func (h *Handler) navForCount(active string, p auth.Principal, reconciling int) navView {
	view := navView{ReconcilingCount: reconciling}
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
		item := navItem{Key: def.Key, Label: def.Label, Href: def.Href, Group: def.Group, Current: def.Key == active}
		if def.Key == "reconciliation" {
			item.Badge = reconciling
		}
		view.Groups[i].Items = append(view.Groups[i].Items, item)
	}
	if p.SessionID != "" {
		if token, err := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin); err == nil {
			view.CSRF = token
		}
	}
	return view
}
