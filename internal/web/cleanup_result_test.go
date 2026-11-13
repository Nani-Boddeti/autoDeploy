package web

import (
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

func TestDetailShowsRetentionCleanup(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.detail = sampleDetail("succeeded", true)
	if b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(b, "Retention cleanup") {
		t.Fatal("row shown without a cleanup")
	}
	cases := []struct {
		name  string
		clean postgres.ReleaseCleanup
		want  []string
	}{
		{"open", postgres.ReleaseCleanup{}, []string{`pill busy">in progress`}},
		{"removed", postgres.ReleaseCleanup{Result: "removed"}, []string{`pill ok">removed`, "container removed"}},
		{"removed image kept", postgres.ReleaseCleanup{Result: "removed", ImageKept: true}, []string{`pill ok">removed`, "image kept"}},
		{"failed", postgres.ReleaseCleanup{Result: "failed", ErrorClass: "engine", Summary: "<b>why</b>", Attempts: 2}, []string{`pill bad">failed`, "engine", "(attempt 2)"}},
	}
	for _, c := range cases {
		clean := c.clean
		stub.detail.ReleaseCleanup = &clean
		b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
		if !strings.Contains(b, "Retention cleanup") {
			t.Errorf("%s: row missing", c.name)
		}
		for _, w := range c.want {
			if !strings.Contains(b, w) {
				t.Errorf("%s: missing %q", c.name, w)
			}
		}
		if strings.Contains(b, "<b>why</b>") {
			t.Errorf("%s: summary not escaped", c.name)
		}
	}
}

func TestStateToneRemoving(t *testing.T) {
	if stateTone("removing") != "busy" || stateTone("removed") != "neutral" {
		t.Fatal("removing/removed tones")
	}
}
