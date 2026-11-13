package web

import (
	"bytes"
	"strings"
	"testing"
)

// The setup flow can only start from a POST to /github/setup; the landing page
// must offer it, or no installation can ever be created (found on the acer).
func TestLandingOffersGitHubSetupForm(t *testing.T) {
	var b bytes.Buffer
	if err := landingTemplate.Execute(&b, struct{ CSRF string }{"tok"}); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if !strings.Contains(html, `<form method="post" action="/github/setup"><input type="hidden" name="csrf" value="tok">`) {
		t.Fatalf("landing page lacks the GitHub setup form: %s", html)
	}
}
