package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodeploy/internal/testutil/srcguard"
)

// Every source file except main.go (the composition root, which hands the
// process stderr to run) must be free of direct process output, so failures
// can only leave through run's fixed, secret-free messages.
func TestNoProcessLoggingInSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := srcguard.Violations(f, string(b))
		if err != nil {
			t.Fatal(err)
		}
		if f == "main.go" {
			if len(v) != 1 || !strings.Contains(v[0], "Stderr") {
				t.Errorf("main.go may only reference os.Stderr once (to pass it to run): %v", v)
			}
			continue
		}
		for _, x := range v {
			t.Errorf("direct process output in %s: %s", f, x)
		}
	}
}
