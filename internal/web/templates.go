package web

import (
	"embed"
	"html/template"
	"net/http"
	"strconv"
)

// Page templates and the shared layout live under templates/ and are embedded
// so the binary stays self-contained. html/template auto-escapes every field.
// The layout (templates/layout.html) defines "head", "nav" and "foot"; each
// page file calls them and contains no inline style, script or style attribute
// because the CSP only permits same-origin stylesheets.
//
//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/app.css
var appCSS []byte

func parsePage(name string) *template.Template {
	return template.Must(template.New(name).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")).Lookup(name + ".html")
}

var (
	loginTemplate             = parsePage("login")
	landingTemplate           = parsePage("landing")
	githubCompleteTemplate    = parsePage("github-complete")
	serversTemplate           = parsePage("servers")
	environmentConfigTemplate = parsePage("environment-config")
	routeHeadTemplate         = parsePage("environment-route")
	enrollmentTokenTemplate   = parsePage("server-enrollment-token")
)

// serveStylesheet serves the embedded stylesheet. It needs no session (the
// login page uses it), accepts GET/HEAD only and rejects any query string.
func serveStylesheet(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(appCSS)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(appCSS)
	}
}
