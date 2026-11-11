//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/runtime/dockerid"
)

func TestActivatePredecessorEmptyOnFirstActivation(t *testing.T) {
	f := newLeaseFixture(t, 101)
	cred := f.withCredential(t, 151)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	result, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(1))
	if err != nil {
		t.Fatal(err)
	}
	if result.PredecessorReleaseID != "" || result.PredecessorRuntimeInstanceID != "" {
		t.Fatalf("first activation predecessor = %q/%q, want empty", result.PredecessorReleaseID, result.PredecessorRuntimeInstanceID)
	}
}

// A real two-release flow: the first activation is authorized and finalized
// by ReportActivationResult, then a second release's Activate must name the
// first release and its attempt-derived runtime instance; replays (even after
// the route head has since moved) answer identically from the stored
// expected_active_release_id.
func TestActivatePredecessorAfterRealFinalizeAndReplay(t *testing.T) {
	a := newActivatedFixture(t, 102, false)
	ctx := context.Background()
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	a.assertFinalized(t, 1)

	f2 := a.leaseFixture
	f2.jobID = f2.insertJob(t, f2.serverID, f2.envID, "second-"+releaseSuffix(t), 0)
	attempt2 := f2.leaseJob(t, 2, 90)
	f2.advanceDeploymentToHealthChecking(t)
	f2.insertCandidateRelease(t, attempt2, "candidate2-"+releaseSuffix(t))
	img := digest(2)

	first, err := a.repo.Activate(ctx, f2.serverID, a.cred, attempt2, 2, img)
	if err != nil {
		t.Fatalf("second Activate: %v", err)
	}
	wantRuntime := dockerid.AttemptRuntimeInstanceID(a.attemptID)
	if first.PredecessorReleaseID != a.candidateID || first.PredecessorRuntimeInstanceID != wantRuntime {
		t.Fatalf("predecessor = %q/%q, want %q/%q", first.PredecessorReleaseID, first.PredecessorRuntimeInstanceID, a.candidateID, wantRuntime)
	}
	if first.PredecessorRuntimeInstanceID == first.BackendRuntimeInstanceID {
		t.Fatal("predecessor runtime instance equals the new backend")
	}

	// Move the head away from the predecessor (forced SQL); the replay must
	// still answer from the operation's stored expected_active_release_id.
	if _, err := f2.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=NULL, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f2.envID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		replay, err := a.repo.Activate(ctx, f2.serverID, a.cred, attempt2, 2, img)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if replay.PredecessorReleaseID != first.PredecessorReleaseID || replay.PredecessorRuntimeInstanceID != first.PredecessorRuntimeInstanceID {
			t.Fatalf("replay %d predecessor = %q/%q, want %q/%q", i, replay.PredecessorReleaseID, replay.PredecessorRuntimeInstanceID, first.PredecessorReleaseID, first.PredecessorRuntimeInstanceID)
		}
	}
}

// Forced SQL: the route head is pointed at an 'active' release that has no
// succeeded activate operation (releases has no insert-time state trigger, and
// route_heads only needs the (release, environment) FK to hold). Activate must
// refuse as corrupt state and authorize nothing.
func TestActivatePredecessorWithoutSucceededOperationIsCorruptState(t *testing.T) {
	f := newLeaseFixture(t, 103)
	cred := f.withCredential(t, 153)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	ctx := context.Background()

	orphan := "orphan-" + releaseSuffix(t)
	depID := "orphan-dep-" + releaseSuffix(t)
	var createdAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(depID), ProjectID: "project", EnvironmentID: f.envID, ServerID: f.serverID, CommitSHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(f.pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		VALUES ($1,$2,$3,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'abcdefabcdefabcdefabcdefabcdefabcdefabcd','stop_then_start','active')`, orphan, depID, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, orphan); err != nil {
		t.Fatal(err)
	}
	f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(ctx, f.serverID, cred, attemptID, 1, digest(3)); !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
	}
	var ops int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM release_operations WHERE attempt_id=$1`, attemptID).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 0 {
		t.Fatalf("operations=%d, want 0 (nothing authorized)", ops)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusHealthChecking {
		t.Fatalf("deployment=%s, want health_checking (rolled back)", got)
	}
}

func (f leaseFixture) insertPlainDeployment(t *testing.T, id string) {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(id), ProjectID: "project", EnvironmentID: f.envID, ServerID: f.serverID, CommitSHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(f.pool).Create(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

// Forced SQL: the predecessor release gets a SECOND succeeded activate
// operation (its own extra deployment, job and attempt, inserted after the
// first, so a higher id). The newest operation's attempt must be named.
func TestActivatePredecessorUsesNewestSucceededOperation(t *testing.T) {
	f := newLeaseFixture(t, 104)
	cred := f.withCredential(t, 154)
	ctx := context.Background()
	f.insertRouteHead(t, "app.example.com")
	predecessorID := "predecessor-" + releaseSuffix(t)
	firstAttempt := f.insertActiveReleaseAndPointRouteHead(t, predecessorID)
	f.insertPlainDeployment(t, "extra-dep-"+releaseSuffix(t))
	secondAttempt := f.insertSucceededActivation(t, predecessorID, "extra-dep-"+releaseSuffix(t), "-2")
	if secondAttempt <= firstAttempt {
		t.Fatalf("second attempt %d not above first %d", secondAttempt, firstAttempt)
	}
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	result, err := repo.Activate(ctx, f.serverID, cred, attemptID, 1, digest(4))
	if err != nil {
		t.Fatal(err)
	}
	if want := dockerid.AttemptRuntimeInstanceID(secondAttempt); result.PredecessorRuntimeInstanceID != want {
		t.Fatalf("runtime instance %q, want newest operation's %q", result.PredecessorRuntimeInstanceID, want)
	}
}

// A route head can only reference an 'active' release (trigger), and Activate
// refuses an 'active' candidate before deriving anything
// (ErrAgentLeaseActivationCandidateNotReady), so predecessor == candidate is
// unreachable end to end. Forced SQL proves both halves: the trigger refuses
// pointing the head at a non-active candidate, and derivePredecessor itself
// (the defense in depth) answers corrupt state with no query needed.
func TestActivatePredecessorEqualToCandidateIsCorruptState(t *testing.T) {
	f := newLeaseFixture(t, 105)
	ctx := context.Background()
	f.insertRouteHead(t, "app.example.com")
	attemptID := f.leaseJob(t, 1, 90)
	candidate := "candidate-" + releaseSuffix(t)
	f.insertCandidateRelease(t, attemptID, candidate)
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, candidate); err == nil {
		t.Fatal("trigger must refuse a route head pointing at a non-active release")
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, _, err = derivePredecessor(ctx, tx, &candidate, candidate, dockerid.AttemptRuntimeInstanceID(attemptID)); !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
	}
	var ops int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM release_operations WHERE attempt_id=$1`, attemptID).Scan(&ops); err != nil || ops != 0 {
		t.Fatalf("operations=%d err=%v, want 0", ops, err)
	}
}

// Activate cannot reach the equal-runtime-instance case end to end: an
// operation carrying the candidate's own attempt id would be found first by
// the replay lookup (attempt, fence, kind) and answered as stale. So the
// assertion is exercised on derivePredecessor directly, in a rolled-back
// transaction, after forced SQL gives the predecessor release a succeeded
// activate operation on the candidate's own attempt.
func TestDerivePredecessorEqualRuntimeInstanceIsCorruptState(t *testing.T) {
	f := newLeaseFixture(t, 106)
	ctx := context.Background()
	f.insertRouteHead(t, "app.example.com")
	predecessorID := "predecessor-" + releaseSuffix(t)
	f.insertActiveReleaseAndPointRouteHead(t, predecessorID)
	attemptID := f.leaseJob(t, 1, 90)
	f.insertCandidateRelease(t, attemptID, "candidate-"+releaseSuffix(t))
	if _, err := f.pool.Exec(ctx, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, created_at, closed_at, environment_id, expected_route_revision, route_digest)
		VALUES ($1,$2,$3,1,'activate',decode(repeat('11',32),'hex'),'succeeded',clock_timestamp()-interval '1 second',clock_timestamp(),$4,0,decode(repeat('33',32),'hex'))`,
		predecessorID, f.jobID, attemptID, f.envID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, _, err = derivePredecessor(ctx, tx, &predecessorID, "candidate-other", dockerid.AttemptRuntimeInstanceID(attemptID))
	if !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
	}
}
