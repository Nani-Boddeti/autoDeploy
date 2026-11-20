package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDumpPagesForReview writes every parity page as static HTML plus the
// stylesheet so the pages can be screenshotted from a plain file server. It is
// skipped unless AUTODEPLOY_UI_DUMP_DIR is set and touches nothing else.
func TestDumpPagesForReview(t *testing.T) {
	dir := os.Getenv("AUTODEPLOY_UI_DUMP_DIR")
	if dir == "" {
		t.Skip("AUTODEPLOY_UI_DUMP_DIR not set")
	}
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "app.css"), appCSS, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, e := range parityEntries() {
		slug := strings.Trim(strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return '-'
		}, strings.ToLower(e.name)), "-")
		if err := os.WriteFile(filepath.Join(dir, slug+".html"), []byte(e.render(t)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Empty states are not parity pages; dump them so the checklist can be reviewed.
	for name, render := range emptyDumpPages() {
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(render(t)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
