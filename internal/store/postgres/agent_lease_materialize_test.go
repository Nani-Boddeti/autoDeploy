//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
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
