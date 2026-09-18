//go:build integration

// Package agenthttp integration tests wire the real
// internal/store/postgres repositories (ServerRepository,
// AgentLeaseRepository, DeploymentRepository) behind Handler and drive the
// full agent protocol over real HTTP against a live PostgreSQL 18 database,
// proving Step 5's transport correctly reaches the durable Step 1-4
// repositories rather than only satisfying its own unit-tested fakes.
package agenthttp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// agentHTTPFixture bootstraps one owner, one servers row with a fresh
// enrollment token, and one queued agent_jobs row (with its backing
// deployment and enqueue event) directly over SQL -- mirroring
// internal/store/postgres's own leaseFixture, but built entirely from this
// package's test file since that fixture's helpers are unexported to the
// postgres test package.
type agentHTTPFixture struct {
	pool     *pgxpool.Pool
	ownerID  string
	serverID string
	jobID    int64
	envID    string
}

// testPool applies every migration (idempotently -- migrations.Apply records
// and skips what is already applied) before returning a pool, exactly like
// internal/store/postgres's own authIntegrationPool. This package must pass
// when run alone against a fresh database, not only when the postgres
// package's tests happen to run first and apply schema as a side effect.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer conn.Close(ctx)
	if err = migrations.Apply(ctx, conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// randomSuffix returns a fresh short hex identifier so repeated invocations
// of the same test against one persistent database (normal run immediately
// followed by a -race run) never collide on a primary key.
func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newAgentHTTPFixture(t *testing.T) agentHTTPFixture {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	// IDs are unique per t.Name() plus a fresh random suffix, and every
	// table this fixture touches is either append-only (servers, its
	// credentials/tokens/audit) or scoped by a foreign key to a fresh row
	// this fixture just inserted (agent_jobs, deployments,
	// environment_slots): nothing here is ever reset or truncated, since
	// store/postgres's own integration suite truncates shared tables
	// (including a CASCADE off "owners") as part of its own fixtures and may
	// run concurrently as a separate test binary against the same database
	// (see tasks/lessons.md on shared-database integration suites); this
	// fixture must never join in that blanket reset, only add rows under
	// identifiers nothing else can collide with. The random suffix (rather
	// than t.Name() alone) is required because "servers" forbids deletion:
	// two invocations of this same test against one persistent database
	// (e.g. a normal run immediately followed by a -race run, both against
	// the same fresh container) would otherwise collide on the servers
	// primary key.
	suffix := randomSuffix(t)
	owner := "agent-http-owner-" + t.Name() + "-" + suffix
	serverID := "agent-http-server-" + t.Name() + "-" + suffix
	envID := "agent-http-env-" + t.Name() + "-" + suffix

	if _, err := pool.Exec(ctx, `INSERT INTO owners(id) VALUES ($1) ON CONFLICT DO NOTHING`, owner); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO servers(id, owner_id, name, protocol_version, status) VALUES ($1,$2,$1,1,'active')`, serverID, owner); err != nil {
		t.Fatalf("insert server: %v", err)
	}

	deploymentID := "agent-http-deployment-" + t.Name() + "-" + suffix
	var createdAt time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(deploymentID), ProjectID: "project", EnvironmentID: envID, ServerID: serverID, CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := postgres.NewDeploymentRepository(pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}

	digest := make([]byte, 32)
	var jobID int64
	if err := pool.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state) VALUES (1,'deployment_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','0123456789abcdef0123456789abcdef01234567',0,clock_timestamp(),$5,$6,'queued') RETURNING id`,
		owner, envID, serverID, deploymentID, "agent-http-key-"+t.Name()+"-"+suffix, digest).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES ($1,1,'enqueued')`, jobID); err != nil {
		t.Fatal(err)
	}

	// Migration 000014's claim-time freeze (Slice 9a Step 2) fails a
	// candidate whose environment has no environment_runtime_configs row,
	// so this fresh envID needs one before Claim can ever succeed against
	// it. Fixed, arbitrary-but-valid defaults; nothing here asserts on
	// their values.
	if _, err := pool.Exec(ctx, `INSERT INTO environment_runtime_configs(environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision) VALUES ($1,8080,'http','stop_then_start',30,500,268435456,'unless-stopped',1)`, envID); err != nil {
		t.Fatal(err)
	}

	return agentHTTPFixture{pool: pool, ownerID: owner, serverID: serverID, jobID: jobID, envID: envID}
}

// enrollmentDigest inserts a fresh enrollment token for f.serverID and
// returns the raw token, so the HTTP enroll test can present it exactly as
// an operator handing the printed token to an agent would.
func (f agentHTTPFixture) issueEnrollment(t *testing.T) server.EnrollmentToken {
	t.Helper()
	tok, err := server.NewEnrollmentToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d, err := server.EnrollmentTokenDigest(tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO server_enrollment_tokens(server_id,digest,issued_at,expires_at) VALUES ($1,$2,clock_timestamp(),clock_timestamp()+interval '15 minutes') ON CONFLICT (server_id) DO UPDATE SET digest=EXCLUDED.digest,issued_at=EXCLUDED.issued_at,expires_at=EXCLUDED.expires_at,consumed_at=NULL`, f.serverID, d[:]); err != nil {
		t.Fatal(err)
	}
	return tok
}

func newTestHandler(pool *pgxpool.Pool) Handler {
	servers := postgres.NewServerRepository(pool)
	leases := postgres.NewAgentLeaseRepository(pool, postgres.NewDeploymentRepository(pool))
	return Handler{
		Enroller: servers,
		Authenticator: authenticatorFunc(func(ctx context.Context, id string, d [32]byte) error {
			_, err := servers.Authenticate(ctx, id, d)
			return err
		}),
		Heartbeater: servers,
		Claimer:     leases,
		Renewer:     leases,
		Completer:   leases,
		Canceller:   leases,
		Reconciler:  leases,
	}
}

type authenticatorFunc func(context.Context, string, [32]byte) error

func (f authenticatorFunc) Authenticate(ctx context.Context, id string, d [32]byte) error {
	return f(ctx, id, d)
}

func postJSON(t *testing.T, srv *httptest.Server, path string, tok server.CredentialToken, serverID string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set(authHeaderName, bearerPrefix+string(tok))
	}
	if serverID != "" {
		req.Header.Set(serverIDHeader, serverID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response for %s: %v", path, err)
	}
	return resp, parsed
}

func hexOf(d [32]byte) string { return hex.EncodeToString(d[:]) }

// TestAgentHTTPFullLifecycle drives enroll -> heartbeat -> claim (with a
// real queued job) -> renew -> complete, then proves the exact-replay and
// mismatched-digest behavior tasks/todo.md Step 5 requires end to end over
// real HTTP against the real repositories.
func TestAgentHTTPFullLifecycle(t *testing.T) {
	f := newAgentHTTPFixture(t)
	h := newTestHandler(f.pool)
	// TestServer marks requests as arriving over TLS-equivalent trusted
	// provenance is not automatic on httptest.NewServer (plain HTTP), so
	// wrap the handler to stamp r.TLS the way a real TLS listener would.
	srv := httptest.NewServer(withTLS(h))
	defer srv.Close()

	credentialToken, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentialDigest, err := server.CredentialTokenDigest(credentialToken)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentToken := f.issueEnrollment(t)

	resp, body := postJSON(t, srv, pathEnroll, "", "", enrollRequest{
		ServerID:         f.serverID,
		EnrollmentToken:  string(enrollmentToken),
		CredentialDigest: hexOf(credentialDigest),
	})
	if resp.StatusCode != http.StatusOK || body["credential_version"] != float64(1) {
		t.Fatalf("enroll: status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = postJSON(t, srv, pathHeartbeat, credentialToken, f.serverID, heartbeatRequest{
		ProtocolVersion: 1, AgentVersion: "v1", OS: "linux", Arch: "amd64", CPUCount: 4,
		MemoryBytes: 1 << 30, DiskFreeBytes: 1 << 30, ExecutorAvailable: false,
	})
	if resp.StatusCode != http.StatusOK || body["server_status"] != "active" {
		t.Fatalf("heartbeat: status=%d body=%v", resp.StatusCode, body)
	}
	var lastHeartbeat *time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT last_heartbeat_at FROM servers WHERE id=$1`, f.serverID).Scan(&lastHeartbeat); err != nil {
		t.Fatal(err)
	}
	if lastHeartbeat == nil {
		t.Fatal("last_heartbeat_at not recorded")
	}

	resp, body = postJSON(t, srv, pathClaim, credentialToken, f.serverID, claimRequest{Limit: 1, ExecutorAvailable: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: status=%d body=%v", resp.StatusCode, body)
	}
	leases, _ := body["leases"].([]any)
	if len(leases) != 1 {
		t.Fatalf("claim: leases=%v", body["leases"])
	}
	lease := leases[0].(map[string]any)
	attemptID := int64(lease["attempt_id"].(float64))
	fence := int64(lease["fence"].(float64))
	if lease["job_id"].(float64) != float64(f.jobID) {
		t.Fatalf("claimed wrong job: %v", lease)
	}
	if _, err := time.Parse(time.RFC3339Nano, lease["lease_expires_at"].(string)); err != nil {
		t.Fatalf("lease_expires_at not RFC3339Nano: %v", err)
	}

	resp, body = postJSON(t, srv, pathRenew, credentialToken, f.serverID, renewRequest{AttemptID: attemptID, Fence: fence, Sequence: 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew: status=%d body=%v", resp.StatusCode, body)
	}

	var resultDigest [32]byte
	copy(resultDigest[:], []byte("0123456789abcdef0123456789abcdef"))
	completeReq := completeRequest{AttemptID: attemptID, Fence: fence, Outcome: "succeeded", OperationID: "op-1", ResultDigest: hexOf(resultDigest)}
	resp, _ = postJSON(t, srv, pathComplete, credentialToken, f.serverID, completeReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete: status=%d", resp.StatusCode)
	}

	// Lost completion response: an identical replay must return the same
	// 200, not mutate anything further, and not error.
	resp, _ = postJSON(t, srv, pathComplete, credentialToken, f.serverID, completeReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay complete: status=%d", resp.StatusCode)
	}

	// A mismatched digest for the same closed attempt must be rejected as
	// an integrity conflict, not silently accepted or treated as stale.
	var otherDigest [32]byte
	copy(otherDigest[:], []byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"))
	mismatched := completeReq
	mismatched.ResultDigest = hexOf(otherDigest)
	resp, body = postJSON(t, srv, pathComplete, credentialToken, f.serverID, mismatched)
	if resp.StatusCode != http.StatusConflict || body["error"] != "integrity_conflict" {
		t.Fatalf("mismatched digest: status=%d body=%v", resp.StatusCode, body)
	}
}

// withTLS wraps h so every request looks like it arrived over a trusted TLS
// listener, matching how a real deployment terminates TLS before this
// handler ever sees a request. httptest.NewServer only serves plain HTTP.
func withTLS(h Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.TLS = &tls.ConnectionState{}
		h.ServeHTTP(w, r)
	}
}
