//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advanceDeploymentToHealthChecking walks f's backing deployment through the
// full queued -> ... -> health_checking chain using the real
// DeploymentRepository, mirroring assignDeployment (agent_lease_step4_test.go)
// but carried all the way to the state Activate's own transitionDeployment
// call requires (health_checking -> activating is the only legal edge into
// activating). leaseJob bypasses Claim for test setup, so it never advances
// the deployment at all; every Activate test needs this first.
func (f leaseFixture) advanceDeploymentToHealthChecking(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	repo := NewDeploymentRepository(f.pool)
	for _, target := range []deployment.Status{
		deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding,
		deployment.StatusStarting, deployment.StatusHealthChecking,
	} {
		dep, err := repo.GetByID(ctx, deployment.ID(deploymentID))
		if err != nil {
			t.Fatal(err)
		}
		var now time.Time
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		next, err := dep.Transition(target, now)
		if err != nil {
			t.Fatalf("transition to %s: %v", target, err)
		}
		if err := repo.Save(ctx, next); err != nil {
			t.Fatalf("save %s: %v", target, err)
		}
	}
	return deploymentID
}

// insertRouteHead creates a route_heads row for f.envID with the given
// hostname and no active release (first deployment shape).
func (f leaseFixture) insertRouteHead(t *testing.T, hostname string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO route_heads(environment_id, hostname, route_revision) VALUES ($1,$2,0)`, f.envID, hostname); err != nil {
		t.Fatal(err)
	}
}

// insertActiveReleaseAndPointRouteHead creates a second deployment's release
// row directly in 'active' state (releases has no INSERT-time state-machine
// trigger -- only UPDATE is guarded) and repoints f's already-created route
// head at it, so a later Activate call sees a genuine predecessor.
func (f leaseFixture) insertActiveReleaseAndPointRouteHead(t *testing.T, releaseID string) {
	t.Helper()
	ctx := context.Background()
	deploymentID := "predecessor-" + releaseID
	var now time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(deploymentID), ProjectID: "project", EnvironmentID: f.envID, ServerID: f.serverID, CommitSHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(f.pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		VALUES ($1,$2,$3,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'abcdefabcdefabcdefabcdefabcdefabcdefabcd','stop_then_start','active')`,
		releaseID, deploymentID, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, releaseID); err != nil {
		t.Fatal(err)
	}
}

// insertCandidateRelease creates the one release row this fixture's own
// deployment owns (releases.deployment_id UNIQUE), in 'candidate' state, and
// the frozen agent_job_attempt_execution row Activate reads BackendPort/
// BackendScheme from. Mirrors ensureRelease's real shape closely enough for
// Activate's own purposes without pulling in Claim's freeze machinery.
func (f leaseFixture) insertCandidateRelease(t *testing.T, attemptID int64, releaseID string) {
	t.Helper()
	ctx := context.Background()
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO releases(id, deployment_id, environment_id, config_digest, commit_sha, strategy, state)
		VALUES ($1,$2,$3,decode(repeat('ab',32),'hex'),'0123456789abcdef0123456789abcdef01234567','stop_then_start','candidate')`,
		releaseID, deploymentID, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempt_execution(attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		VALUES ($1,8080,'http','stop_then_start',30,1,500,268435456,'unless-stopped',0,'/','app.internal')`, attemptID); err != nil {
		t.Fatal(err)
	}
}

func (f leaseFixture) releaseState(t *testing.T, releaseID string) (state string, imageDigest []byte) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT state, image_digest FROM releases WHERE id=$1`, releaseID).Scan(&state, &imageDigest); err != nil {
		t.Fatal(err)
	}
	return
}

func (f leaseFixture) activationOperationRow(t *testing.T, attemptID int64) (state string, envID *string, expectedRouteRevision *int64, expectedActiveReleaseID *string) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(), `SELECT state, environment_id, expected_route_revision, expected_active_release_id FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, attemptID).
		Scan(&state, &envID, &expectedRouteRevision, &expectedActiveReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestAgentLeaseActivateFirstDeploymentSwitchAuthorized(t *testing.T) {
	f := newLeaseFixture(t, 160)
	cred := f.withCredential(t, 210)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	releaseID := "release-" + t.Name()
	f.insertCandidateRelease(t, attemptID, releaseID)

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	imgDigest := digest(1)
	result, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, imgDigest)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if result.OperationID == "" || result.Hostname != "app.example.com" || result.RouteRevision != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.EntrypointName == "" || result.CertResolverName == "" || result.BackendRuntimeInstanceID == "" || result.RouteDigest == "" {
		t.Fatalf("missing response field: %+v", result)
	}

	state, image := f.releaseState(t, releaseID)
	if state != "ready" || len(image) != 32 || [32]byte(image) != imgDigest {
		t.Fatalf("release state=%s image=%x", state, image)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment status=%s, want activating", got)
	}
	opState, envID, expectedRouteRevision, expectedActive := f.activationOperationRow(t, attemptID)
	if opState != "switch_authorized" || envID == nil || *envID != f.envID || expectedRouteRevision == nil || *expectedRouteRevision != 0 || expectedActive != nil {
		t.Fatalf("operation row: state=%s env=%v rev=%v active=%v", opState, envID, expectedRouteRevision, expectedActive)
	}
	var eventType string
	if err := f.pool.QueryRow(context.Background(), `SELECT event_type FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='activation_authorized'`, f.jobID, attemptID).Scan(&eventType); err != nil {
		t.Fatalf("activation_authorized event missing: %v", err)
	}
}

func TestAgentLeaseActivateWithPredecessorCapturesExpectedActiveRelease(t *testing.T) {
	f := newLeaseFixture(t, 161)
	cred := f.withCredential(t, 211)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	predecessorID := "predecessor-release-" + t.Name()
	f.insertActiveReleaseAndPointRouteHead(t, predecessorID)
	releaseID := "release-" + t.Name()
	f.insertCandidateRelease(t, attemptID, releaseID)

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	result, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(2))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// route_revision was bumped to 1 by insertActiveReleaseAndPointRouteHead's
	// own UPDATE, so Activate's own new revision is 2.
	if result.RouteRevision != 2 {
		t.Fatalf("RouteRevision=%d, want 2", result.RouteRevision)
	}
	opState, _, expectedRouteRevision, expectedActive := f.activationOperationRow(t, attemptID)
	if opState != "switch_authorized" || expectedRouteRevision == nil || *expectedRouteRevision != 1 || expectedActive == nil || *expectedActive != predecessorID {
		t.Fatalf("operation row: state=%s rev=%v active=%v", opState, expectedRouteRevision, expectedActive)
	}
}

func TestAgentLeaseActivateReplayIsIdempotent(t *testing.T) {
	f := newLeaseFixture(t, 162)
	cred := f.withCredential(t, 212)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	releaseID := "release-" + t.Name()
	f.insertCandidateRelease(t, attemptID, releaseID)

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	imgDigest := digest(3)
	first, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, imgDigest)
	if err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	second, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, imgDigest)
	if err != nil {
		t.Fatalf("replay Activate: %v", err)
	}
	if first != second {
		t.Fatalf("replay result differs: first=%+v second=%+v", first, second)
	}
	// Exactly one operation row, exactly one activation_authorized event --
	// the replay must not have mutated or duplicated anything.
	var opCount, eventCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, attemptID).Scan(&opCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='activation_authorized'`, f.jobID, attemptID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if opCount != 1 || eventCount != 1 {
		t.Fatalf("opCount=%d eventCount=%d, want 1 and 1", opCount, eventCount)
	}
}

func TestAgentLeaseActivateConflictingDigestRefused(t *testing.T) {
	f := newLeaseFixture(t, 163)
	cred := f.withCredential(t, 213)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	releaseID := "release-" + t.Name()
	f.insertCandidateRelease(t, attemptID, releaseID)

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(4)); err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(5)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("err = %v, want ErrAgentLeaseIntegrityConflict", err)
	}
}

// TestAgentLeaseActivateRefusesWrongFence is also this recheck's guard test:
// it proves the fence check fires by presenting a fence that does not match
// either the attempt's own fence or the slot's current fence.
func TestAgentLeaseActivateRefusesWrongFence(t *testing.T) {
	f := newLeaseFixture(t, 164)
	cred := f.withCredential(t, 214)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "release-"+t.Name())

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 2, digest(6)); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err = %v, want ErrAgentLeaseStale", err)
	}
}

func TestAgentLeaseActivateRefusesWrongServer(t *testing.T) {
	f := newLeaseFixture(t, 165)
	f.withCredential(t, 215)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "release-"+t.Name())
	f.insertServer(t, "other-server")
	otherCred := digest(255)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, "other-server", otherCred[:]); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	// other-server authenticates fine on its own credential, but the attempt
	// this attemptID names belongs to f.serverID -- the ownership recheck
	// (not the credential check) must be what refuses this call.
	if _, err := repo.Activate(context.Background(), "other-server", otherCred, attemptID, 1, digest(7)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err = %v, want ErrServerAuthorizationDenied", err)
	}
}

// TestAgentLeaseActivateRefusesCancelledJob is this recheck's guard test:
// removing the cancel_requested_at check would let this test's Activate
// call succeed instead of being refused.
func TestAgentLeaseActivateRefusesCancelledJob(t *testing.T) {
	f := newLeaseFixture(t, 166)
	cred := f.withCredential(t, 216)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "release-"+t.Name())
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(8)); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err = %v, want ErrAgentLeaseStale", err)
	}
}

// TestAgentLeaseActivateRefusesExpiredAuthority is the lease-authority
// recheck's guard test: leasing with a near-zero grant and letting it lapse
// before calling Activate must refuse.
func TestAgentLeaseActivateRefusesExpiredAuthority(t *testing.T) {
	f := newLeaseFixture(t, 167)
	cred := f.withCredential(t, 217)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "release-"+t.Name())
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET authority_expires_at=clock_timestamp() - interval '1 second' WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(9)); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err = %v, want ErrAgentLeaseStale", err)
	}
}

// TestAgentLeaseActivateRefusesMissingRouteHead is the route-head-presence
// recheck's guard test: no insertRouteHead call at all.
func TestAgentLeaseActivateRefusesMissingRouteHead(t *testing.T) {
	f := newLeaseFixture(t, 168)
	cred := f.withCredential(t, 218)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertCandidateRelease(t, attemptID, "release-"+t.Name())

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(10)); !errors.Is(err, ErrAgentLeaseActivationNoRouteHead) {
		t.Fatalf("err = %v, want ErrAgentLeaseActivationNoRouteHead", err)
	}
}

// TestAgentLeaseActivateRefusesCandidateAlreadyActive is the
// candidate-readiness recheck's guard test for the "already active" branch
// (design §7): force the release straight to 'active' at insert time
// (legal -- releases has no INSERT-time state-machine trigger) and confirm
// Activate refuses rather than reprocessing it.
func TestAgentLeaseActivateRefusesCandidateAlreadyActive(t *testing.T) {
	f := newLeaseFixture(t, 169)
	cred := f.withCredential(t, 219)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	ctx := context.Background()
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	releaseID := "release-" + t.Name()
	if _, err := f.pool.Exec(ctx, `INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		VALUES ($1,$2,$3,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'0123456789abcdef0123456789abcdef01234567','stop_then_start','active')`,
		releaseID, deploymentID, f.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempt_execution(attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision, health_gate_path, health_gate_host)
		VALUES ($1,8080,'http','stop_then_start',30,1,500,268435456,'unless-stopped',0,'/','app.internal')`, attemptID); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(ctx, f.serverID, cred, attemptID, 1, digest(11)); !errors.Is(err, ErrAgentLeaseActivationCandidateNotReady) {
		t.Fatalf("err = %v, want ErrAgentLeaseActivationCandidateNotReady", err)
	}
}

// TestAgentLeaseActivateRefusesCandidateFailed covers the other arm of the
// candidate-readiness recheck: a release that failed its build is not
// 'candidate' or 'ready' either.
func TestAgentLeaseActivateRefusesCandidateFailed(t *testing.T) {
	f := newLeaseFixture(t, 173)
	cred := f.withCredential(t, 223)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	releaseID := "release-" + t.Name()
	f.insertCandidateRelease(t, attemptID, releaseID)
	if _, err := f.pool.Exec(context.Background(), `UPDATE releases SET state='failed', updated_at=clock_timestamp() WHERE id=$1`, releaseID); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(15)); !errors.Is(err, ErrAgentLeaseActivationCandidateNotReady) {
		t.Fatalf("err = %v, want ErrAgentLeaseActivationCandidateNotReady", err)
	}
}

// TestAgentLeaseActivateConcurrentCallsProduceExactlyOneOperation races
// several goroutines calling Activate for the same attempt/fence
// concurrently (simulating a retried agent call under network uncertainty).
// Exactly one ever reaches a fresh authorization; the rest must observe the
// replay path (identical digest here, so every caller succeeds, but the
// release_operations row must never be duplicated).
func TestAgentLeaseActivateConcurrentCallsProduceExactlyOneOperation(t *testing.T) {
	f := newLeaseFixture(t, 170)
	cred := f.withCredential(t, 220)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "release-"+t.Name())

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	imgDigest := digest(12)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, imgDigest)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	var opCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, attemptID).Scan(&opCount); err != nil {
		t.Fatal(err)
	}
	if opCount != 1 {
		t.Fatalf("opCount=%d, want exactly 1", opCount)
	}
}

// TestAgentLeaseActivateOrderingVsRequestCancellationBothOrderings proves
// §5's outcome-given-ordering claim: whichever call runs to completion
// first (both calls made sequentially, on one goroutine, in each of the two
// possible orders) determines the outcome, and the other observes a
// consistent refusal -- never both succeeding. It does NOT exercise any
// lock: two sequential calls on one goroutine see each other's committed
// state through ordinary read-committed MVCC regardless of whether either
// method takes a single FOR UPDATE anywhere, so this test would pass
// identically with every lock in both methods deleted. Its purpose is
// narrower and still worth keeping: proving the two committed *outcomes*
// (ErrAgentJobActivating vs ErrAgentLeaseStale, and which one) are correct
// for each ordering. See
// TestAgentLeaseActivateGenuinelyRacesRequestCancellation below for the
// actual concurrency/locking proof.
func TestAgentLeaseActivateOrderingVsRequestCancellationBothOrderings(t *testing.T) {
	for _, activateFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "activate_first", false: "cancel_first"}[activateFirst], func(t *testing.T) {
			seed := byte(171)
			if !activateFirst {
				seed = 172
			}
			f := newLeaseFixture(t, seed)
			cred := f.withCredential(t, seed+50)
			attemptID := f.leaseJob(t, 1, 90)
			f.advanceDeploymentToHealthChecking(t)
			f.insertRouteHead(t, "app.example.com")
			f.insertCandidateRelease(t, attemptID, "release-cancel-race")
			repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))

			if activateFirst {
				if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(13)); err != nil {
					t.Fatalf("Activate: %v", err)
				}
				if err := repo.RequestCancellation(context.Background(), f.principal, f.jobID); !errors.Is(err, ErrAgentJobActivating) {
					t.Fatalf("RequestCancellation err = %v, want ErrAgentJobActivating", err)
				}
			} else {
				if err := repo.RequestCancellation(context.Background(), f.principal, f.jobID); err != nil {
					t.Fatalf("RequestCancellation: %v", err)
				}
				if _, err := repo.Activate(context.Background(), f.serverID, cred, attemptID, 1, digest(14)); !errors.Is(err, ErrAgentLeaseStale) {
					t.Fatalf("Activate err = %v, want ErrAgentLeaseStale", err)
				}
			}
		})
	}
}

// waitForLockWaiters polls pg_stat_activity until at least want backends are
// genuinely blocked (wait_event_type='Lock') running a query matching
// queryLike, or fails the test on timeout. This is how
// TestAgentLeaseActivateGenuinelyRacesRequestCancellation proves both
// Activate and RequestCancellation are actually contending on the same row
// at the same instant, rather than trusting a fixed sleep to have given both
// goroutines enough of a head start.
func waitForLockWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryLike string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query ILIKE $1`, "%"+queryLike+"%").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d backends blocked on a lock running a query matching %q; saw %d", want, queryLike, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAgentLeaseActivateGenuinelyRacesRequestCancellation is the real
// concurrency/locking proof TestAgentLeaseActivateOrderingVsRequestCancellationBothOrderings
// above cannot provide: both Activate and RequestCancellation are launched
// on separate goroutines, each opening its own transaction and pool
// connection, and both are made to genuinely block at the same instant by
// holding environment_slots' row lock open in a third transaction until
// pg_stat_activity confirms both backends are actually waiting on a lock
// (waitForLockWaiters above) -- not merely "started", but provably queued
// behind the same row. Releasing the holder is what lets real database lock
// arbitration, not goroutine scheduling or a fixed sleep, decide which side
// proceeds first. The property asserted is exactly the one the ADR
// amendment claims: exactly one side ever succeeds, the loser observes the
// specific refusal consistent with whichever way the race actually went,
// and the committed end state (deployment status, or job cancel_requested_at)
// matches the winner -- never both succeeding, never an inconsistent state.
func TestAgentLeaseActivateGenuinelyRacesRequestCancellation(t *testing.T) {
	f := newLeaseFixture(t, 174)
	cred := f.withCredential(t, 224)
	attemptID := f.leaseJob(t, 1, 90)
	f.advanceDeploymentToHealthChecking(t)
	f.insertRouteHead(t, "app.example.com")
	f.insertCandidateRelease(t, attemptID, "release-genuine-race")
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deferred (not just called after a successful holder.Commit below): if
	// waitForLockWaiters ever fails this test (e.g. because a broken
	// FOR UPDATE removal stops one side from ever blocking on this row --
	// see this function's own doc comment), t.Fatal unwinds this goroutine
	// via runtime.Goexit, which still runs deferred calls. Without this
	// defer, that path would leave the holder transaction open forever,
	// leaking the lock and the pool connection and hanging the still-blocked
	// RequestCancellation goroutine for the rest of the test binary's life.
	// Rollback after a successful Commit is a documented no-op error, safe
	// to ignore, matching this package's own //nolint:errcheck idiom.
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, f.envID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var activateErr, cancelErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, activateErr = repo.Activate(ctx, f.serverID, cred, attemptID, 1, digest(15))
	}()
	go func() {
		defer wg.Done()
		cancelErr = repo.RequestCancellation(ctx, f.principal, f.jobID)
	}()

	// Both Activate's `SELECT fence, current_attempt_id FROM
	// environment_slots ... FOR UPDATE` and RequestCancellation's
	// `SELECT 1 FROM environment_slots ... FOR UPDATE` block on the row
	// this transaction is holding; once pg_stat_activity shows both
	// backends genuinely waiting, releasing the holder puts them into real
	// contention for the same row, resolved by Postgres's own lock queue,
	// not by this test's goroutine scheduling.
	waitForLockWaiters(t, ctx, f.pool, "environment_slots", 2)

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	activateSucceeded := activateErr == nil
	cancelSucceeded := cancelErr == nil
	if activateSucceeded == cancelSucceeded {
		t.Fatalf("exactly one side must win: activateErr=%v cancelErr=%v", activateErr, cancelErr)
	}
	if activateSucceeded {
		if !errors.Is(cancelErr, ErrAgentJobActivating) {
			t.Fatalf("activation won; cancellation err = %v, want ErrAgentJobActivating", cancelErr)
		}
		if got := f.deploymentStatus(t); got != deployment.StatusActivating {
			t.Fatalf("deployment status=%s, want activating", got)
		}
	} else {
		if !errors.Is(activateErr, ErrAgentLeaseStale) {
			t.Fatalf("cancellation won; activate err = %v, want ErrAgentLeaseStale", activateErr)
		}
		jobState, _, cancelRequestedAt, _ := f.jobRow(t)
		if jobState != "leased" || cancelRequestedAt == nil {
			t.Fatalf("jobState=%s cancelRequestedAt=%v, want leased with cancel_requested_at set", jobState, cancelRequestedAt)
		}
	}
}
