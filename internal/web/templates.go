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

//go:embed static/logs.js
var logsJS []byte

func parsePage(name string) *template.Template {
	return template.Must(template.New(name).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")).Lookup(name + ".html")
}

var (
	loginTemplate             = parsePage("login")
	landingTemplate           = parsePage("landing")
	githubCompleteTemplate    = parsePage("github-complete")
	githubContinueTemplate    = parsePage("github-continue")
	serversTemplate           = parsePage("servers")
	environmentConfigTemplate = parsePage("environment-config")
	routeHeadTemplate         = parsePage("environment-route")
	enrollmentTokenTemplate   = parsePage("server-enrollment-token")
)

// staticAsset is one embedded, same-origin static file.
type staticAsset struct {
	contentType string
	body        []byte
}

// staticAssets is the exact-path allowlist of served static files. There is no
// directory serving: any other /static/ path is a 404.
var staticAssets = map[string]staticAsset{
	"/static/app.css": {contentType: "text/css; charset=utf-8", body: appCSS},
	"/static/logs.js": {contentType: "text/javascript; charset=utf-8", body: logsJS},
}

// serveStatic serves one allowlisted embedded asset. It needs no session (the
// login page uses it), accepts GET/HEAD only and rejects any query string.
func serveStatic(w http.ResponseWriter, r *http.Request, asset staticAsset) {
	if r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(asset.body)
	}
}
