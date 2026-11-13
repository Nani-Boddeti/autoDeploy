package main

import (
	"errors"
	"strings"
	"testing"
)

func TestFailAtReportsSiteNotErrorText(t *testing.T) {
	err := failAt(errors.New("postgres://user:secret@host/db"))
	site := failureSite(err)
	if !strings.HasPrefix(site, " at failat_test.go:") {
		t.Fatalf("site = %q", site)
	}
	if strings.Contains(site, "secret") {
		t.Fatal("error text leaked into the site")
	}
	if failAt(nil) != nil || failureSite(errors.New("x")) != "" {
		t.Fatal("nil or untagged errors must stay unchanged")
	}
}
