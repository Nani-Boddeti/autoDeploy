//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
)

// Slice 23-F (ADR 0004 amendment 2026-11-21): an attempt closed by expiry or
// revocation vacates the slot WITHOUT reconciliation_required only when the
// deployment is still before any container call (queued/assigned/fetching/
// building) and no release operation exists; every other status blocks.

func TestDeploymentStatusLeavesNoHostStateIsAnAllowlist(t *testing.T) {
	safe := map[deployment.Status]bool{
		deployment.StatusQueued: true, deployment.StatusAssigned: true,
		deployment.StatusFetching: true, deployment.StatusBuilding: true,
	}
	all := []deployment.Status{
		deployment.StatusQueued, deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding,
		deployment.StatusStarting, deployment.StatusHealthChecking, deployment.StatusActivating, deployment.StatusHealthy,
		deployment.StatusFailed, deployment.StatusCancelled, deployment.StatusSuperseded, deployment.StatusRolledBack,
		deployment.Status(""), deployment.Status("some_future_status"),
	}
	for _, s := range all {
		if got := deploymentStatusLeavesNoHostState(s); got != safe[s] {
			t.Errorf("deploymentStatusLeavesNoHostState(%q) = %t, want %t", s, got, safe[s])
		}
	}
}

// advanceDeploymentTo walks f's deployment forward along the execution path
// with the real aggregate transitions until it reaches target. Tests that need
// an ordinary expiry close to BLOCK the slot (a candidate container may exist)
// use it to put the deployment in starting or later, since an expiry in an
// earlier phase now vacates the slot without the flag (slice 23-F).
func (f leaseFixture) advanceDeploymentTo(t *testing.T, target deployment.Status) {
	t.Helper()
	ctx := context.Background()
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	repo := NewDeploymentRepository(f.pool)
	path := []deployment.Status{deployment.StatusQueued, deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding,
		deployment.StatusStarting, deployment.StatusHealthChecking}
	for {
		dep, err := repo.GetByID(ctx, deployment.ID(deploymentID))
		if err != nil {
			t.Fatal(err)
		}
		cur := dep.Status()
		if cur == target {
			return
		}
		next := -1
		for i, st := range path {
			if st == cur {
				next = i + 1
			}
		}
		if next < 1 || next >= len(path) {
			t.Fatalf("cannot advance deployment from %s to %s", cur, target)
		}
		var now time.Time
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		moved, err := dep.Transition(path[next], now)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(ctx, moved); err != nil {
			t.Fatal(err)
		}
	}
}

// claimAt claims f's job through the real Claim path (deployment assigned) and
// reports progress up to and including target.
func (f leaseFixture) claimAt(t *testing.T, seed byte, target deployment.Status) (repo *AgentLeaseRepository, digestOut [32]byte, attemptID, fence int64) {
	t.Helper()
	d := f.withCredential(t, seed)
	repo, attemptID, fence = authorizeFirstClaim(t, f, d)
	for _, phase := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking} {
		if target == deployment.StatusAssigned {
			break
		}
		if err := repo.ReportProgress(context.Background(), f.serverID, d, attemptID, fence, phase); err != nil {
			t.Fatalf("report %s: %v", phase, err)
		}
		if phase == target {
			break
		}
	}
	if got := f.deploymentStatus(t); got != target {
		t.Fatalf("deployment status=%s, want %s", got, target)
	}
	return repo, d, attemptID, fence
}

func (f leaseFixture) lapseAttempt(t *testing.T, attemptID int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, attemptID); err != nil {
		t.Fatalf("lapse: %v", err)
	}
}

func (f leaseFixture) jobEventTypes(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT event_type FROM agent_job_events WHERE job_id=$1 ORDER BY sequence`, f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// sweep runs the named closer (expireOne via ExpireBatch, or revoke).
func (f leaseFixture) sweep(t *testing.T, caller string, repo *AgentLeaseRepository, attemptID int64) {
	t.Helper()
	ctx := context.Background()
	switch caller {
	case "expireOne":
		f.lapseAttempt(t, attemptID)
		if n, err := repo.ExpireBatch(ctx, 10); err != nil || n != 1 {
			t.Fatalf("ExpireBatch n=%d err=%v", n, err)
		}
	case "revoke":
		if err := NewServerRepository(f.pool).RevokeServer(ctx, f.principal, f.serverID, "compromised host"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown caller %q", caller)
	}
}

func TestExpiryBeforeAnyContainerCallVacatesSlotWithoutBlock(t *testing.T) {
	for i, target := range []deployment.Status{deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding} {
		for j, caller := range sweepCallers {
			t.Run(string(target)+"/"+caller, func(t *testing.T) {
				f := newLeaseFixture(t, byte(60+i*4+j*2))
				repo, d, attemptID, fence := f.claimAt(t, byte(130+i*4+j*2), target)
				f.sweep(t, caller, repo, attemptID)

				if state, closedAt, _, _, _ := f.attemptRow(t, attemptID); state != "expired" || closedAt == nil {
					t.Fatalf("attempt=%s closedAt=%v, want expired", state, closedAt)
				}
				slotFence, current, reconAttempt, flagged := f.slotRow(t)
				if slotFence != fence+1 || current != nil || flagged || reconAttempt != nil {
					t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want fence %d, vacated, NOT flagged", slotFence, current, flagged, reconAttempt, fence+1)
				}
				if jobState, _, _, count := f.jobRow(t); jobState != "retry_wait" || count != 1 {
					t.Fatalf("job=%s attempt_count=%d, want retry_wait/1", jobState, count)
				}
				ev := f.jobEventTypes(t)
				if n := len(ev); n < 2 || ev[n-2] != "expired" || ev[n-1] != "retry_scheduled" {
					t.Fatalf("events=%v, want ...expired, retry_scheduled", ev)
				}
				if got := f.deploymentStatus(t); got != target {
					t.Fatalf("deployment=%s, want unchanged %s on a retried attempt", got, target)
				}
				if caller == "expireOne" {
					// The slot is claimable immediately: a second job on the
					// same environment is leased at once, under a newer fence.
					f.insertJob(t, f.serverID, f.envID, "next-"+string(target), 0)
					leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
					if err != nil || len(leases) != 1 || leases[0].Fence <= slotFence {
						t.Fatalf("claim after vacate: leases=%+v err=%v, want one lease with fence > %d", leases, err, slotFence)
					}
				}
			})
		}
	}
}

func TestExpiryOnceAContainerMayExistBlocksAsBefore(t *testing.T) {
	for i, target := range []deployment.Status{deployment.StatusStarting, deployment.StatusHealthChecking} {
		for j, caller := range sweepCallers {
			t.Run(string(target)+"/"+caller, func(t *testing.T) {
				f := newLeaseFixture(t, byte(80+i*4+j*2))
				repo, _, attemptID, fence := f.claimAt(t, byte(150+i*4+j*2), target)
				f.sweep(t, caller, repo, attemptID)
				slotFence, current, reconAttempt, flagged := f.slotRow(t)
				if slotFence != fence+1 || current != nil || !flagged || reconAttempt == nil || *reconAttempt != attemptID {
					t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want vacated and flagged for attempt %d", slotFence, current, flagged, reconAttempt, attemptID)
				}
				if jobState, _, _, _ := f.jobRow(t); jobState != "retry_wait" {
					t.Fatalf("job=%s, want retry_wait", jobState)
				}
			})
		}
	}
}

// A cancel requested during a build (the K1 scenario: agent killed while
// building, operator cancels) closes as cancelled without the block.
func TestCancelledDuringBuildClosesWithoutBlock(t *testing.T) {
	f := newLeaseFixture(t, 100)
	repo, _, attemptID, fence := f.claimAt(t, 170, deployment.StatusBuilding)
	if err := repo.RequestCancellation(context.Background(), f.principal, f.jobID); err != nil {
		t.Fatal(err)
	}
	f.sweep(t, "expireOne", repo, attemptID)
	if state, _, _, _, _ := f.attemptRow(t, attemptID); state != "cancelled" {
		t.Fatalf("attempt=%s, want cancelled", state)
	}
	slotFence, current, reconAttempt, flagged := f.slotRow(t)
	if slotFence != fence+1 || current != nil || flagged || reconAttempt != nil {
		t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want vacated, NOT flagged", slotFence, current, flagged, reconAttempt)
	}
	if jobState, _, _, _ := f.jobRow(t); jobState != "cancelled" {
		t.Fatalf("job=%s, want cancelled", jobState)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusCancelled {
		t.Fatalf("deployment=%s, want cancelled", got)
	}
}

// Negative definition: a status outside the allowlist blocks, including a
// terminal one that should never coexist with a leased attempt.
func TestExpiryWithUnexpectedDeploymentStatusBlocks(t *testing.T) {
	f := newLeaseFixture(t, 104)
	repo, _, attemptID, fence := f.claimAt(t, 174, deployment.StatusBuilding)
	ctx := context.Background()
	depRepo := NewDeploymentRepository(f.pool)
	var deploymentID string
	if err := f.pool.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	dep, err := depRepo.GetByID(ctx, deployment.ID(deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	next, err := dep.Transition(deployment.StatusFailed, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := depRepo.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
	f.sweep(t, "expireOne", repo, attemptID)
	slotFence, current, reconAttempt, flagged := f.slotRow(t)
	if slotFence != fence+1 || current != nil || !flagged || reconAttempt == nil || *reconAttempt != attemptID {
		t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want flagged under a terminal status", slotFence, current, flagged, reconAttempt)
	}
}

// A release operation that is already RESOLVED still proves the host was
// mutated: with the deployment forced back into the allowlist the slot must
// still be flagged (the operation check, not the status, decides here).
func TestExpiryWithResolvedActivationOperationBlocksEvenIfStatusLooksSafe(t *testing.T) {
	for i, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixtureAtAttempt(t, byte(106+i), 1) // attempt 1: retry path, no deployment transition on close
			a.forceOperationState(t, state)
			if _, err := a.pool.Exec(context.Background(), `UPDATE deployments SET status='building' WHERE id=(SELECT deployment_id FROM agent_jobs WHERE id=$1)`, a.jobID); err != nil {
				t.Fatal(err)
			}
			a.arm(t, "expireOne")
			if _, err := a.sweepVia(t, "expireOne"); err != nil {
				t.Fatal(err)
			}
			fence, current, reconAttempt, flagged := a.slotRow(t)
			if fence != 2 || current != nil || !flagged || reconAttempt == nil || *reconAttempt != a.attemptID {
				t.Fatalf("slot fence=%d current=%v flagged=%v reconAttempt=%v, want flagged", fence, current, flagged, reconAttempt)
			}
		})
	}
}

// Precondition half in the store: once expiry vacated the slot, a late
// starting report from the old attempt is refused and changes nothing, so a
// partitioned agent cannot start a container the block no longer covers.
func TestProgressRefusedAfterExpiryVacatedSlot(t *testing.T) {
	f := newLeaseFixture(t, 110)
	repo, d, attemptID, fence := f.claimAt(t, 180, deployment.StatusBuilding)
	f.sweep(t, "expireOne", repo, attemptID)
	ctx := context.Background()
	for _, phase := range []deployment.Status{deployment.StatusStarting, deployment.StatusHealthChecking, deployment.StatusBuilding} {
		if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, phase); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("late %s report err=%v, want denial", phase, err)
		}
		if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence+1, phase); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("late %s report under the advanced fence err=%v, want denial", phase, err)
		}
	}
	if got := f.deploymentStatus(t); got != deployment.StatusBuilding {
		t.Fatalf("deployment=%s, want building unchanged", got)
	}
}

// Real lock contention: a starting report is parked on the slot row by an
// open transaction until the authority lapses, then released. The report must
// be refused (authority lapsed) and the following expiry must not block,
// because the deployment never reached starting. Contexts are bounded so a
// regression fails instead of hanging.
func TestProgressParkedOnSlotLockLosesToLapsedAuthority(t *testing.T) {
	f := newLeaseFixture(t, 114)
	repo, d, attemptID, _ := f.claimAt(t, 184, deployment.StatusBuilding)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()+interval '2 seconds', authority_expires_at=now()+interval '2 seconds' WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, f.envID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var progressErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		progressErr = repo.ReportProgress(ctx, f.serverID, d, attemptID, 1, deployment.StatusStarting)
	}()
	waitForLockWaiters(t, ctx, f.pool, "environment_slots%FOR SHARE", 1)
	time.Sleep(2200 * time.Millisecond) // authority lapses while the report is parked
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !errors.Is(progressErr, ErrServerAuthorizationDenied) {
		t.Fatalf("parked report err=%v, want denial once authority lapsed", progressErr)
	}
	if n, err := repo.ExpireBatch(ctx, 10); err != nil || n != 1 {
		t.Fatalf("ExpireBatch n=%d err=%v", n, err)
	}
	if _, _, _, flagged := f.slotRow(t); flagged {
		t.Fatal("slot flagged although the deployment never reached starting")
	}
	if got := f.deploymentStatus(t); got != deployment.StatusBuilding {
		t.Fatalf("deployment=%s, want building", got)
	}
}

// Races a starting report against expiry of the same attempt around the
// authority boundary and asserts the invariant that makes the narrowing safe:
// the slot is never left unblocked once the deployment reached starting.
func TestProgressRacesExpiryNeverLeavesStartedAttemptUnblocked(t *testing.T) {
	for i := 0; i < 6; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			f := newLeaseFixture(t, byte(116+i*2))
			repo, d, attemptID, fence := f.claimAt(t, byte(190+i), deployment.StatusBuilding)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()+interval '400 milliseconds', authority_expires_at=now()+interval '400 milliseconds' WHERE id=$1`, attemptID); err != nil {
				t.Fatal(err)
			}
			begin := time.Now()
			progressAt := 400*time.Millisecond - 60*time.Millisecond + time.Duration(i)*25*time.Millisecond
			var wg sync.WaitGroup
			var progressErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				time.Sleep(time.Until(begin.Add(progressAt)))
				progressErr = repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusStarting)
			}()
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					n, err := repo.ExpireBatch(ctx, 10)
					if err != nil {
						t.Errorf("ExpireBatch: %v", err)
						return
					}
					if n == 1 {
						return
					}
					time.Sleep(3 * time.Millisecond)
				}
			}()
			wg.Wait()

			status := f.deploymentStatus(t)
			slotFence, current, _, flagged := f.slotRow(t)
			if current != nil || slotFence != fence+1 {
				t.Fatalf("slot fence=%d current=%v, want the attempt closed and vacated", slotFence, current)
			}
			t.Logf("progress err=%v status=%s flagged=%t", progressErr, status, flagged)
			switch {
			case progressErr == nil && (status != deployment.StatusStarting || !flagged):
				t.Fatalf("progress committed first but status=%s flagged=%t, want starting and flagged", status, flagged)
			case progressErr != nil && (!errors.Is(progressErr, ErrServerAuthorizationDenied) || status == deployment.StatusStarting || flagged):
				t.Fatalf("progress refused (%v) but status=%s flagged=%t, want denial, status building, not flagged", progressErr, status, flagged)
			}
		})
	}
}

// Deterministic complement of the parked-lock test: the starting report
// commits FIRST (authority still valid), then authority lapses and expiry runs.
// Expiry must see starting and FLAG the slot. No timing dependence.
func TestExpiryAfterCommittedStartingReportFlagsSlot(t *testing.T) {
	f := newLeaseFixture(t, 140)
	repo, d, attemptID, fence := f.claimAt(t, 200, deployment.StatusBuilding)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := repo.ReportProgress(ctx, f.serverID, d, attemptID, fence, deployment.StatusStarting); err != nil {
		t.Fatalf("starting report: %v", err)
	}
	f.lapseAttempt(t, attemptID)
	if n, err := repo.ExpireBatch(ctx, 10); err != nil || n != 1 {
		t.Fatalf("ExpireBatch n=%d err=%v", n, err)
	}
	slotFence, current, reconAttempt, flagged := f.slotRow(t)
	if slotFence != fence+1 || current != nil || !flagged || reconAttempt == nil || *reconAttempt != attemptID {
		t.Fatalf("slot fence=%d current=%v flagged=%t reconAttempt=%v, want vacated and flagged", slotFence, current, flagged, reconAttempt)
	}
	if got := f.deploymentStatus(t); got != deployment.StatusStarting {
		t.Fatalf("deployment=%s, want starting", got)
	}
}
