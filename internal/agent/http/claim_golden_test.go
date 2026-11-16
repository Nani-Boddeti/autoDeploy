package agenthttp

import (
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"autodeploy/internal/queue"
	"autodeploy/internal/server"
)

// claimGoldenLease is a lease built only from fields that predate step 22-1c.
func claimGoldenLease() queue.ClaimedLease {
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	return queue.ClaimedLease{
		JobID: 1, AttemptID: 2, AttemptNumber: 1, Fence: 3, ProjectID: "p", EnvironmentID: "env", DeploymentID: "dep",
		RepositoryID: 9, Ref: "refs/heads/main", CommitSHA: "abc", LeaseExpiresAt: at, AuthorityExpiresAt: at,
		ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "blue_green", HealthPolicyRevision: 1,
		CPUMillis: 250, MemoryBytes: 134217728, RestartPolicy: "unless-stopped", DrainSeconds: 5,
		HealthGatePath: "/health", HealthGateHost: "app.example.com", ReleaseID: "rel",
	}
}

// claimGoldenBody is the exact response bytes the claim handler produced for
// claimGoldenLease before step 22-1c. A claim that does not opt in must keep
// producing precisely this.
const claimGoldenBody = `{"leases":[{"job_id":1,"attempt_id":2,"attempt_number":1,"fence":3,"project_id":"p","environment_id":"env","deployment_id":"dep","repository_id":9,"ref":"refs/heads/main","commit_sha":"abc","lease_expires_at":"2026-11-16T10:00:00Z","authority_expires_at":"2026-11-16T10:00:00Z","container_port":8080,"container_scheme":"http","release_strategy":"blue_green","health_policy_revision":1,"cpu_millis":250,"memory_bytes":134217728,"restart_policy":"unless-stopped","drain_seconds":5,"health_gate_path":"/health","health_gate_host":"app.example.com","release_id":"rel","secret_bindings":[],"literal_bindings":[]}]}`

func TestClaimResponseBytesWithoutCapabilityAreUnchanged(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"limit":1,"executor_available":true}`,
		"empty":  `{"limit":1,"executor_available":true,"capabilities":[]}`,
		"null":   `{"limit":1,"executor_available":true,"capabilities":null}`,
	} {
		fc := &fakeClaimer{leases: []queue.ClaimedLease{claimGoldenLease()}}
		h := Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}
		tok, _ := server.NewCredentialToken(rand.Reader)
		w := do(h, newRequestTLS(newRequest(t, pathClaim, body, bearerPrefix+string(tok), "server_1")))
		if w.Code != http.StatusOK || w.Body.String() != claimGoldenBody || fc.calls != 1 {
			t.Errorf("%s: status=%d calls=%d body=%s", name, w.Code, fc.calls, w.Body.String())
		}
	}
}
