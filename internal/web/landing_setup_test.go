package web

import (
	"bytes"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

// The setup flow can only start from a POST to /github/setup; the GitHub page
// (moved from the Overview in 22-3g) must offer it, or no installation can ever
// be created (found on the acer).
func TestGitHubPageOffersSetupForm(t *testing.T) {
	var b bytes.Buffer
	if err := githubTemplate.Execute(&b, githubView{CSRF: "tok"}); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if !strings.Contains(html, `<form method="post" action="/github/setup"><input type="hidden" name="csrf" value="tok">`) {
		t.Fatalf("GitHub page lacks the GitHub setup form: %s", html)
	}
}

// POST /github/bindings had a handler but no form anywhere, so no repository could be bound
// (found by static audit before the first deploy).
func TestGitHubPageOffersBindingFormWithPickLists(t *testing.T) {
	var b bytes.Buffer
	view := githubView{
		CSRF: "tok", ShowBind: true,
		Repositories: []postgres.BindableRepository{{RepositoryID: 201, FullName: "owner/demo", BoundProject: "demo"}},
		Servers:      []githubServer{{ID: "server_1", Name: "acer"}},
	}
	if err := githubTemplate.Execute(&b, view); err != nil {
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

func TestGitHubPageHidesBindingFormWithoutGitHub(t *testing.T) {
	var b bytes.Buffer
	if err := githubTemplate.Execute(&b, githubView{CSRF: "tok"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "/github/bindings") {
		t.Fatal("binding form shown although GitHub is not configured")
	}
}
