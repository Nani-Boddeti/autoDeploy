//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/deployment"
)

// authorizeFirstClaim claims once and returns the authorization inputs a
// materialization request would present.
func authorizeFirstClaim(t *testing.T, f leaseFixture, digest [32]byte) (*AgentLeaseRepository, int64, int64) {
	t.Helper()
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, digest, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("expected one lease, got %d", len(leases))
	}
	return repo, leases[0].AttemptID, leases[0].Fence
}

func TestAuthorizeMaterializationReturnsFrozenSetForLiveLease(t *testing.T) {
	f := newLeaseFixture(t, 190)
	d := f.withCredential(t, 90)
	headID := "secret-head-" + t.Name()
	versionID := "secret-version-" + t.Name()
	f.insertSecretHeadWithActiveVersion(t, headID, f.envID, "APP_TOKEN", "runtime", versionID, 1)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)

	authorization, err := repo.AuthorizeMaterialization(context.Background(), f.serverID, d, attemptID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if len(authorization.Secrets) != 1 {
		t.Fatalf("secrets=%+v, want exactly the frozen set", authorization.Secrets)
	}
	ref := authorization.Secrets[0]
	if ref.SecretVersionID != versionID || ref.VariableName != "APP_TOKEN" || ref.DeliveryScope != "runtime" || ref.SecretID != headID {
		t.Fatalf("reference=%+v", ref)
	}
	if ref.Purpose != "runtime" {
		t.Fatalf("purpose=%s", ref.Purpose)
	}
	if authorization.EnvironmentID != f.envID || authorization.OwnerID != f.ownerID {
		t.Fatalf("scope=%+v", authorization)
	}
}

// TestAuthorizeMaterializationAllowsSupersededFrozenVersion is the ADR 0003
// rule that executing work keeps its frozen versions: a rotation after the
// freeze supersedes the version, and the running attempt must still be able
// to materialize it or an in-flight deployment would break whenever an
// administrator rotated a secret.
func TestAuthorizeMaterializationAllowsSupersededFrozenVersion(t *testing.T) {
	f := newLeaseFixture(t, 191)
	d := f.withCredential(t, 92)
	headID := "secret-head-" + t.Name()
	firstVersion := "secret-version-" + t.Name() + "-v1"
	f.insertSecretHeadWithActiveVersion(t, headID, f.envID, "APP_TOKEN", "runtime", firstVersion, 1)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)

	f.rotateSecretVersion(t, headID, f.envID, "secret-version-"+t.Name()+"-v2", 2)

	authorization, err := repo.AuthorizeMaterialization(context.Background(), f.serverID, d, attemptID, fence)
	if err != nil {
		t.Fatalf("superseded frozen version must still materialize: %v", err)
	}
	if len(authorization.Secrets) != 1 || authorization.Secrets[0].SecretVersionID != firstVersion {
		t.Fatalf("authorized %+v, want the frozen pre-rotation version", authorization.Secrets)
	}
}

// TestAuthorizeMaterializationRefusesRevokedVersion is the other half: a
// revoked version is compromise handling, which ADR 0003 says does withdraw
// frozen versions.
func TestAuthorizeMaterializationRefusesRevokedVersion(t *testing.T) {
	f := newLeaseFixture(t, 192)
	d := f.withCredential(t, 94)
	headID := "secret-head-" + t.Name()
	versionID := "secret-version-" + t.Name()
	f.insertSecretHeadWithActiveVersion(t, headID, f.envID, "APP_TOKEN", "runtime", versionID, 1)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)

	ctx := context.Background()
	// A version that is still its head's current version cannot be revoked
	// (000004 forbids it), and the legal paths into 'revoked' are from
	// 'active' or 'superseded'. Compromise handling therefore replaces first,
	// which supersedes the frozen version, then revokes it -- mirror that.
	f.rotateSecretVersion(t, headID, f.envID, "secret-version-"+t.Name()+"-v2", 2)
	if _, err := f.pool.Exec(ctx, `UPDATE secret_versions SET state='revoked' WHERE id=$1`, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want ErrServerAuthorizationDenied for a revoked frozen version", err)
	}
}

func TestAuthorizeMaterializationRefusesStaleFence(t *testing.T) {
	f := newLeaseFixture(t, 193)
	d := f.withCredential(t, 96)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence+1); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for a fence ahead of the slot", err)
	}
	// Advance the slot as expiry would: the attempt's own fence is now stale.
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=fence+1 WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial once the slot fence moved on", err)
	}
}

func TestAuthorizeMaterializationRefusesExpiredAuthority(t *testing.T) {
	f := newLeaseFixture(t, 194)
	d := f.withCredential(t, 98)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET authority_expires_at=clock_timestamp() - interval '1 second' WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for expired authority", err)
	}
}

func TestAuthorizeMaterializationRefusesCancelledJob(t *testing.T) {
	f := newLeaseFixture(t, 195)
	d := f.withCredential(t, 100)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial once cancellation was requested", err)
	}
}

func TestAuthorizeMaterializationRefusesClosedAttempt(t *testing.T) {
	f := newLeaseFixture(t, 196)
	d := f.withCredential(t, 102)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if err := repo.Complete(ctx, f.serverID, d, attemptID, fence, "succeeded", "", "op-"+t.Name(), [32]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for an attempt that already completed", err)
	}
}

// TestAuthorizeMaterializationRefusesAnotherServer proves an authenticated
// server cannot read a different server's frozen secret set: holding a valid
// credential is not authority over someone else's attempt.
func TestAuthorizeMaterializationRefusesAnotherServer(t *testing.T) {
	f := newLeaseFixture(t, 197)
	d := f.withCredential(t, 104)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	f.insertServer(t, "server-intruder")
	intruderDigest := [32]byte{9, 9, 9}
	if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id, version, digest, status) VALUES ('server-intruder', 1, $1, 'active')`, intruderDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, "server-intruder", intruderDigest, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for another server's attempt", err)
	}
}

func TestAuthorizeMaterializationRefusesRevokedCredential(t *testing.T) {
	f := newLeaseFixture(t, 198)
	d := f.withCredential(t, 106)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE server_credentials SET status='revoked', revoked_at=clock_timestamp() WHERE server_id=$1 AND status='active'`, f.serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); err == nil {
		t.Fatal("expected denial once the presented credential was revoked")
	}
}

// TestAuthorizeMaterializationPerformsNoMutation proves this route is
// read-only: authorization must not consume, advance, or close anything, or a
// build fetching its secrets would disturb the lease it is executing under.
func TestAuthorizeMaterializationPerformsNoMutation(t *testing.T) {
	f := newLeaseFixture(t, 199)
	d := f.withCredential(t, 108)
	headID := "secret-head-" + t.Name()
	f.insertSecretHeadWithActiveVersion(t, headID, f.envID, "APP_TOKEN", "runtime", "secret-version-"+t.Name(), 1)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	type snapshot struct {
		attemptState                     string
		attemptFence, slotFence          int64
		renewalSequence, agentEventCount int
	}
	read := func() snapshot {
		var s snapshot
		if err := f.pool.QueryRow(ctx, `SELECT a.state, a.fence, s.fence, a.renewal_sequence,
			(SELECT count(*) FROM agent_job_events WHERE job_id=$2)
			FROM agent_job_attempts a JOIN environment_slots s ON s.environment_id=a.environment_id WHERE a.id=$1`,
			attemptID, f.jobID).Scan(&s.attemptState, &s.attemptFence, &s.slotFence, &s.renewalSequence, &s.agentEventCount); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := read()
	for i := 0; i < 3; i++ {
		if _, err := repo.AuthorizeMaterialization(ctx, f.serverID, d, attemptID, fence); err != nil {
			t.Fatal(err)
		}
	}
	if after := read(); after != before {
		t.Fatalf("authorization mutated lease state: before=%+v after=%+v", before, after)
	}
}

func TestReportProgressAdvancesDeploymentThroughPhases(t *testing.T) {
	f := newLeaseFixture(t, 200)
	d := f.withCredential(t, 110)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	status := func() string {
		var s string
		if err := f.pool.QueryRow(ctx, `SELECT status FROM deployments WHERE id=(SELECT deployment_id FROM agent_jobs WHERE id=$1)`, f.jobID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if status() != "assigned" {
		t.Fatalf("status after claim=%s, want assigned", status())
	}
	for _, phase := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking} {
		if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, phase); err != nil {
			t.Fatalf("report %s: %v", phase, err)
		}
		if status() != string(phase) {
			t.Fatalf("status=%s after reporting %s", status(), phase)
		}
	}

	// The deployment's own event log records each phase.
	var events int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM deployment_events WHERE deployment_id=(SELECT deployment_id FROM agent_jobs WHERE id=$1) AND to_status IN ('fetching','building','starting','health_checking')`, f.jobID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 4 {
		t.Fatalf("deployment phase events=%d, want 4", events)
	}
}

// TestReportProgressAbsorbsDuplicateAndStalePhases is why ExecutionOrder
// exists: canTransition rejects a same-status transition, so without an
// explicit order comparison a progress report retried after a network failure
// would fail a build that is otherwise fine.
func TestReportProgressAbsorbsDuplicateAndStalePhases(t *testing.T) {
	f := newLeaseFixture(t, 201)
	d := f.withCredential(t, 112)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusFetching); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusBuilding); err != nil {
		t.Fatal(err)
	}
	// Duplicate of the current phase, and a stale report about an earlier one.
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusBuilding); err != nil {
		t.Fatalf("duplicate phase must be absorbed: %v", err)
	}
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusFetching); err != nil {
		t.Fatalf("stale earlier phase must be absorbed: %v", err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM deployments WHERE id=(SELECT deployment_id FROM agent_jobs WHERE id=$1)`, f.jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "building" {
		t.Fatalf("status=%s, want building (a stale report must not move it back)", status)
	}
	// No extra event rows were appended by the absorbed reports.
	var events int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM deployment_events WHERE deployment_id=(SELECT deployment_id FROM agent_jobs WHERE id=$1) AND to_status IN ('fetching','building')`, f.jobID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("events=%d, want 2: an absorbed report must append nothing", events)
	}
}

// TestReportProgressRefusesSkippedPhase proves a phase that never happened
// cannot be claimed: inventing the intermediate transitions would forge
// history in an append-only log.
func TestReportProgressRefusesSkippedPhase(t *testing.T) {
	f := newLeaseFixture(t, 202)
	d := f.withCredential(t, 114)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusStarting); !errors.Is(err, ErrAgentLeaseInvalidInput) {
		t.Fatalf("err=%v, want ErrAgentLeaseInvalidInput for a skipped phase", err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM deployments WHERE id=(SELECT deployment_id FROM agent_jobs WHERE id=$1)`, f.jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "assigned" {
		t.Fatalf("status=%s, want assigned: a refused report must change nothing", status)
	}
}

// TestReportProgressAppendsNoAgentJobEvent is ADR 0004's bounded-events rule:
// a long build would otherwise write unbounded agent_job_events rows.
func TestReportProgressAppendsNoAgentJobEvent(t *testing.T) {
	f := newLeaseFixture(t, 203)
	d := f.withCredential(t, 116)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	count := func() int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_job_events WHERE job_id=$1`, f.jobID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	for _, phase := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusBuilding, deployment.StatusStarting} {
		if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, phase); err != nil {
			t.Fatal(err)
		}
	}
	if after := count(); after != before {
		t.Fatalf("agent_job_events grew from %d to %d", before, after)
	}
}

func TestReportProgressRefusesUnauthorizedCallers(t *testing.T) {
	f := newLeaseFixture(t, 204)
	d := f.withCredential(t, 118)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence+1, deployment.StatusFetching); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("stale fence err=%v", err)
	}
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusActivating); !errors.Is(err, ErrAgentLeaseInvalidInput) {
		t.Fatalf("activating must not be reportable by an agent: err=%v", err)
	}
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusHealthy); !errors.Is(err, ErrAgentLeaseInvalidInput) {
		t.Fatalf("healthy must not be reportable by an agent: err=%v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET authority_expires_at=clock_timestamp() - interval '1 second' WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusFetching); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("expired authority err=%v", err)
	}
}

// TestReportProgressRefusesTerminalDeployment proves a deployment that has
// left the execution path cannot have its history rewritten by work that is
// winding down.
func TestReportProgressRefusesTerminalDeployment(t *testing.T) {
	f := newLeaseFixture(t, 205)
	d := f.withCredential(t, 120)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if err := repo.RequestCancellation(ctx, f.principal, f.jobID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusFetching); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial once the deployment is terminal", err)
	}
}
