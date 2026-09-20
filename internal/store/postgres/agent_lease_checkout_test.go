//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
)

// TestAuthorizeCheckoutReturnsFrozenJobAuthorityForLiveLease proves the
// happy path: a live lease's own server, presenting its own fence, gets back
// exactly the job's owner/project/environment/deployment/repository/commit --
// never anything a caller could widen by naming it.
func TestAuthorizeCheckoutReturnsFrozenJobAuthorityForLiveLease(t *testing.T) {
	f := newLeaseFixture(t, 210)
	d := f.withCredential(t, 130)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)

	authorization, err := repo.AuthorizeCheckout(context.Background(), f.serverID, d, attemptID, fence)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.AttemptID != attemptID || authorization.Fence != fence || authorization.JobID != f.jobID {
		t.Fatalf("authorization=%+v", authorization)
	}
	if authorization.OwnerID != f.ownerID || authorization.ProjectID != "project" || authorization.EnvironmentID != f.envID {
		t.Fatalf("scope=%+v", authorization)
	}
	if authorization.RepositoryID != 201 || authorization.CommitSHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("commit authority=%+v, want the frozen job's own repository/commit", authorization)
	}
}

// TestAuthorizeCheckoutPerformsNoMutation mirrors
// TestAuthorizeMaterializationPerformsNoMutation: re-deriving checkout
// authority must not consume, advance, or close anything.
func TestAuthorizeCheckoutPerformsNoMutation(t *testing.T) {
	f := newLeaseFixture(t, 211)
	d := f.withCredential(t, 132)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	type snapshot struct {
		attemptState            string
		attemptFence, slotFence int64
		renewalSequence         int
	}
	read := func() snapshot {
		var s snapshot
		if err := f.pool.QueryRow(ctx, `SELECT a.state, a.fence, s.fence, a.renewal_sequence
			FROM agent_job_attempts a JOIN environment_slots s ON s.environment_id=a.environment_id WHERE a.id=$1`,
			attemptID).Scan(&s.attemptState, &s.attemptFence, &s.slotFence, &s.renewalSequence); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := read()
	for i := 0; i < 3; i++ {
		if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); err != nil {
			t.Fatal(err)
		}
	}
	if after := read(); after != before {
		t.Fatalf("authorization mutated lease state: before=%+v after=%+v", before, after)
	}
}

func TestAuthorizeCheckoutRefusesStaleFence(t *testing.T) {
	f := newLeaseFixture(t, 212)
	d := f.withCredential(t, 134)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence+1); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for a fence ahead of the slot", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=fence+1 WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial once the slot fence moved on", err)
	}
}

// TestAuthorizeCheckoutRefusesNonOccupantAttempt proves a fence match alone
// is not enough: the slot must still consider this attempt its current
// occupant, exactly like AuthorizeMaterialization.
func TestAuthorizeCheckoutRefusesNonOccupantAttempt(t *testing.T) {
	f := newLeaseFixture(t, 213)
	d := f.withCredential(t, 136)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial once this attempt is no longer the slot's occupant", err)
	}
}

func TestAuthorizeCheckoutRefusesReconciliationPendingEnvironment(t *testing.T) {
	f := newLeaseFixture(t, 214)
	d := f.withCredential(t, 138)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, f.envID, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial while the environment awaits reconciliation", err)
	}
}

func TestAuthorizeCheckoutRefusesCancelledJob(t *testing.T) {
	f := newLeaseFixture(t, 215)
	d := f.withCredential(t, 140)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial once cancellation was requested", err)
	}
}

func TestAuthorizeCheckoutRefusesClosedAttempt(t *testing.T) {
	f := newLeaseFixture(t, 216)
	d := f.withCredential(t, 142)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if err := repo.Complete(ctx, f.serverID, d, attemptID, fence, "succeeded", "", "op-"+t.Name(), [32]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for an attempt that already completed", err)
	}
}

func TestAuthorizeCheckoutRefusesExpiredAuthority(t *testing.T) {
	f := newLeaseFixture(t, 217)
	d := f.withCredential(t, 144)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET authority_expires_at=clock_timestamp() - interval '1 second' WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for expired authority", err)
	}
}

// TestAuthorizeCheckoutRefusesAnotherServer proves an authenticated server
// cannot reserve or mint a checkout credential for a different server's
// attempt: holding a valid credential is not authority over someone else's
// attempt.
func TestAuthorizeCheckoutRefusesAnotherServer(t *testing.T) {
	f := newLeaseFixture(t, 218)
	d := f.withCredential(t, 146)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	f.insertServer(t, "server-intruder")
	intruderDigest := [32]byte{9, 9, 9}
	if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id, version, digest, status) VALUES ('server-intruder', 1, $1, 'active')`, intruderDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, "server-intruder", intruderDigest, attemptID, fence); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v, want denial for another server's attempt", err)
	}
}

func TestAuthorizeCheckoutRefusesRevokedCredential(t *testing.T) {
	f := newLeaseFixture(t, 219)
	d := f.withCredential(t, 148)
	repo, attemptID, fence := authorizeFirstClaim(t, f, d)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE server_credentials SET status='revoked', revoked_at=clock_timestamp() WHERE server_id=$1 AND status='active'`, f.serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthorizeCheckout(ctx, f.serverID, d, attemptID, fence); err == nil {
		t.Fatal("expected denial once the presented credential was revoked")
	}
}
