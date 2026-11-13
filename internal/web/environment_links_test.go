package web

import (
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

const linkEnv = "env a&b"

// html/template renders the query "+" as &#43; (same URL in the browser).
const linkEnvEnc = "env&#43;a%26b"

func wantEnvLinks(t *testing.T, page, body string) {
	t.Helper()
	for _, want := range []string{
		`href="/environments/config?environment_id=` + linkEnvEnc + `"`,
		`href="/environments/route?environment_id=` + linkEnvEnc + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: missing %s", page, want)
		}
	}
}

func TestEnvironmentLinksOnOperationsPages(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.projects = []postgres.ProjectSummary{{ProjectID: "proj", EnvironmentIDs: []string{linkEnv}}}
	d := sampleDetail("leased", false)
	d.Deployment.EnvironmentID = linkEnv
	stub.detail = d
	stub.list = postgres.DeploymentList{Items: []postgres.DeploymentSummary{d.Deployment}}
	for _, target := range []string{"/projects", "/deployments/detail?id=dep-1"} {
		rr := opsGet(h, p, http.MethodGet, target)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", target, rr.Code)
		}
		wantEnvLinks(t, target, rr.Body.String())
	}
}

func TestEnvironmentLinksOnServersAndConfigPages(t *testing.T) {
	h, admin, p := serversHandler(t)
	admin.environments = []postgres.ReconcilingEnvironment{{EnvironmentID: linkEnv, AttemptID: 1, JobID: 2, ServerID: "s"}}
	rr := opsGet(h, p, http.MethodGet, "/servers")
	if rr.Code != http.StatusOK {
		t.Fatalf("/servers = %d", rr.Code)
	}
	wantEnvLinks(t, "/servers", rr.Body.String())

	ch, _, cp := runtimeConfigHandler(t)
	rr = opsGet(ch, cp, http.MethodGet, "/environments/config?environment_id=env-a")
	if rr.Code != http.StatusOK {
		t.Fatalf("config = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `href="/environments/route?environment_id=env-a"`) {
		t.Error("config page missing route link")
	}
	rr = opsGet(ch, cp, http.MethodGet, "/environments/config?environment_id=env%20a%26b")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `href="/environments/route?environment_id=env%20a%26b"`) {
		t.Error("config page route link not percent-encoded")
	}
}

func TestExecNoticeFollowsActivationFlag(t *testing.T) {
	const notice = "Traffic activation is disabled on this control plane."
	for _, enabled := range []bool{false, true} {
		h, stub, p := operationsHandler(t)
		h.config.ActivationEnabled = enabled
		stub.detail = sampleDetail("leased", false)
		for _, target := range []string{"/deployments", "/deployments/detail?id=dep-1"} {
			got := strings.Contains(opsGet(h, p, http.MethodGet, target).Body.String(), notice)
			if got == enabled {
				t.Errorf("%s activationEnabled=%v: notice shown=%v", target, enabled, got)
			}
		}
		lh, _, _, lp := logsHandler(t)
		lh.config.ActivationEnabled = enabled
		if got := strings.Contains(opsGet(lh, lp, http.MethodGet, "/deployments/logs?id=dep-1").Body.String(), notice); got == enabled {
			t.Errorf("logs activationEnabled=%v: notice shown=%v", enabled, got)
		}
	}
}
