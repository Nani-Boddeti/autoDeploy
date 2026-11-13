package web

import (
	"bytes"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

// The setup flow can only start from a POST to /github/setup; the landing page
// must offer it, or no installation can ever be created (found on the acer).
func TestLandingOffersGitHubSetupForm(t *testing.T) {
	var b bytes.Buffer
	if err := landingTemplate.Execute(&b, landingView{CSRF: "tok"}); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if !strings.Contains(html, `<form method="post" action="/github/setup"><input type="hidden" name="csrf" value="tok">`) {
		t.Fatalf("landing page lacks the GitHub setup form: %s", html)
	}
}

// POST /github/bindings had a handler but no form anywhere, so no repository could be bound
// (found by static audit before the first deploy).
func TestLandingOffersBindingFormWithPickLists(t *testing.T) {
	var b bytes.Buffer
	view := landingView{
		CSRF: "tok", ShowBind: true,
		Repositories: []postgres.BindableRepository{{RepositoryID: 201, FullName: "owner/demo", BoundProject: "demo"}},
		Servers:      []landingServer{{ID: "server_1", Name: "acer"}},
	}
	if err := landingTemplate.Execute(&b, view); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{
		`<form method="post" action="/github/bindings">`,
		`<option value="201">owner/demo (bound to demo)</option>`,
		`<option value="server_1">acer (server_1)</option>`,
		`name="project_id"`, `name="ref"`, `value="refs/heads/main"`, `name="environment_id"`, `name="repository_id"`, `name="server_id"`,
		`<input type="hidden" name="csrf" value="tok">`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("binding form lacks %q: %s", want, html)
		}
	}
	// Six fields exactly: githubBind rejects any other count.
	form := html[strings.Index(html, `action="/github/bindings"`):]
	form = form[:strings.Index(form, "</form>")]
	if got := strings.Count(form, `name="`); got != 6 {
		t.Fatalf("binding form has %d named fields, want 6: %s", got, form)
	}
}

func TestLandingHidesBindingFormWithoutGitHub(t *testing.T) {
	var b bytes.Buffer
	if err := landingTemplate.Execute(&b, landingView{CSRF: "tok"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "/github/bindings") {
		t.Fatal("binding form shown although GitHub is not configured")
	}
}
