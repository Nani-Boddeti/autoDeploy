package web

import (
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

func TestDetailShowsPredecessorDrain(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.detail = sampleDetail("succeeded", true)
	if b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(b, "Predecessor drain") {
		t.Fatal("row shown without a recorded drain")
	}
	for outcome, pill := range map[string]string{
		"stopped": `pill ok">stopped`, "stop_failed": `pill bad">stop_failed`, "route_moved": `pill bad">route_moved`,
	} {
		stub.detail.PredecessorDrain = &postgres.DrainResultView{Outcome: outcome, Summary: "<b>why</b>"}
		b := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
		if !strings.Contains(b, "Predecessor drain") || !strings.Contains(b, pill) {
			t.Errorf("%s: row/pill missing", outcome)
		}
		if strings.Contains(b, "<b>why</b>") {
			t.Errorf("%s: summary not escaped", outcome)
		}
	}
}
