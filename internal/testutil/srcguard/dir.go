package srcguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func violationsInDir(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s: %v", dir, err)
	}
	var all []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := Violations(f, string(b))
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		all = append(all, v...)
	}
	return all
}
