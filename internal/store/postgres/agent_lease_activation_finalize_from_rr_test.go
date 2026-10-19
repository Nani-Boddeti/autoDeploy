//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/deployment"
)

// finalizeFrom runs finalizeActivationSucceeded the way a real caller does:
// one transaction taking the documented locks (credential SHARE -> slot ->
// job -> attempt -> operation; deployment, route head and releases are taken
// by the helper itself in order) and verifying nothing else, then the helper
// with the given fromState. It commits on success and rolls back on error, and
// returns the helper's error and the `now` it was given.
func (a activatedFixture) finalizeFrom(t *testing.T, fromState string) (time.Time, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT 1 FROM server_credentials WHERE server_id=$1 AND status='active' FOR SHARE`, a.serverID); err != nil {
		t.Fatalf("lock credential: %v", err)
	}
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, a.envID},
		{`SELECT 1 FROM agent_jobs WHERE id=$1 FOR UPDATE`, a.jobID},
		{`SELECT 1 FROM agent_job_attempts WHERE id=$1 FOR UPDATE`, a.attemptID},
		{`SELECT 1 FROM release_operations WHERE id=$1 FOR UPDATE`, mustParseInt(t, a.operationID)},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
			t.Fatalf("lock (%s): %v", q.sql, err)
		}
	}
	var deploymentID, candidate string
	var expectedRev int64
	var expectedActive *string
	if err := tx.QueryRow(ctx, `SELECT deployment_id FROM agent_jobs WHERE id=$1`, a.jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT release_id, expected_route_revision, expected_active_release_id FROM release_operations WHERE id=$1`, mustParseInt(t, a.operationID)).Scan(&candidate, &expectedRev, &expectedActive); err != nil {
		t.Fatal(err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	err = a.repo.finalizeActivationSucceeded(ctx, tx, activationFinalizeInput{
		jobID:                   a.jobID,
		attemptID:               a.attemptID,
		operationID:             mustParseInt(t, a.operationID),
		environmentID:           a.envID,
		deploymentID:            deploymentID,
		candidateReleaseID:      candidate,
		expectedRouteRevision:   expectedRev,
		expectedActiveReleaseID: expectedActive,
		resultDigest:            a.routeDigest,
		summary:                 "operator declared activated",
		now:                     now,
		fromState:               fromState,
	})
	if err != nil {
		return now, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return now, nil
}

// assertFinalizedFromReconciliation is assertFinalized minus its job-event
// suffix check (the block event legitimately precedes 'activated').
func (a activatedFixture) assertFinalizedFromReconciliation(t *testing.T, wantRevision int64) {
	t.Helper()
	if state, _ := a.releaseState(t, a.candidateID); state != "active" {
		t.Fatalf("candidate state=%s, want active", state)
	}
	if state, _ := a.releaseState(t, a.predecessorID); state != "retained" {
		t.Fatalf("predecessor state=%s, want retained", state)
	}
	revision, active, reconciling := a.routeHead(t)
	if revision != wantRevision || active == nil || *active != a.candidateID || reconciling {
		t.Fatalf("route head revision=%d active=%v reconciling=%v, want %d %s false", revision, active, reconciling, wantRevision, a.candidateID)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusHealthy {
		t.Fatalf("deployment=%s, want healthy", got)
	}
	if state, closedAt, _, resultOp, _ := a.attemptRow(t, a.attemptID); state != "succeeded" || closedAt == nil || resultOp == nil || *resultOp != a.operationID {
		t.Fatalf("attempt state=%s closedAt=%v resultOp=%v", state, closedAt, resultOp)
	}
	if fence, current, _, _ := a.slotRow(t); fence != 1 || current != nil {
		t.Fatalf("slot fence=%d current=%v, want 1 nil", fence, current)
	}
	if jobState, _, _, _ := a.jobRow(t); jobState != "succeeded" {
		t.Fatalf("job state=%s, want succeeded", jobState)
	}
	if events := strings.Join(a.jobEvents(t), ","); events != "activation_authorized,activation_reconciliation_required,activated,succeeded" {
		t.Fatalf("job events=%s, want authorized, block, activated, succeeded", events)
	}
}

func (a activatedFixture) blockViaTx2Failure(t *testing.T) {
	t.Helper()
	if err := a.report(context.Background(), false, "reload timed out"); err != nil {
		t.Fatalf("ambiguous report: %v", err)
	}
}

func (a activatedFixture) blockViaSweeperDiversion(t *testing.T) {
	t.Helper()
	if closed := a.lapseAndExpireOne(t); closed {
		t.Fatal("expireOne closed the attempt; the diversion must keep it leased")
	}
}

func TestFinalizeActivationSucceededFromReconciliationRequired(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  byte
		block func(activatedFixture, *testing.T)
	}{
		{"tx2 failure report", 230, activatedFixture.blockViaTx2Failure},
		{"sweeper diversion", 231, activatedFixture.blockViaSweeperDiversion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newActivatedFixture(t, tc.seed, true)
			tc.block(a, t)
			state, blockedAt, _ := a.operation(t)
			if state != "reconciliation_required" || blockedAt == nil {
				t.Fatalf("setup: operation state=%s closedAt=%v", state, blockedAt)
			}
			wantRevision, _, flagged := a.routeHead(t)
			if !flagged {
				t.Fatal("setup: route head must be flagged")
			}
			if _, _, _, slotFlag := a.slotRow(t); !slotFlag {
				t.Fatal("setup: slot must be flagged")
			}

			if _, err := a.finalizeFrom(t, "reconciliation_required"); err != nil {
				t.Fatalf("finalize from reconciliation_required: %v", err)
			}

			state, closedAt, summary := a.operation(t)
			if state != "succeeded" || closedAt == nil || summary != "operator declared activated" {
				t.Fatalf("operation state=%s closedAt=%v summary=%q", state, closedAt, summary)
			}
			// Byte-for-byte: the instant the ambiguity was recorded is evidence.
			if !closedAt.Equal(*blockedAt) {
				t.Fatalf("operation closed_at moved from %v to %v", blockedAt, closedAt)
			}
			a.assertFinalizedFromReconciliation(t, wantRevision+1)
			if _, _, _, slotFlag := a.slotRow(t); slotFlag {
				t.Fatal("slot flag not cleared")
			}
			var reconAttempt *int64
			if err := a.pool.QueryRow(context.Background(), `SELECT reconciliation_attempt_id FROM environment_slots WHERE environment_id=$1`, a.envID).Scan(&reconAttempt); err != nil || reconAttempt != nil {
				t.Fatalf("slot reconciliation_attempt_id=%v err=%v, want NULL", reconAttempt, err)
			}
		})
	}
}

// The tx2 path keeps stamping closed_at with the finalize `now`: the
// COALESCE must not turn a NULL closed_at into NULL.
func TestFinalizeActivationSucceededFromSwitchAuthorizedSetsClosedAt(t *testing.T) {
	a := newActivatedFixture(t, 232, true)
	now, err := a.finalizeFrom(t, "switch_authorized")
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	_, closedAt, _ := a.operation(t)
	if closedAt == nil || !closedAt.Equal(now) {
		t.Fatalf("operation closed_at=%v, want finalize now %v", closedAt, now)
	}
	a.assertFinalized(t, 2)
}

// A flag owned by a different attempt must neither be cleared nor left in
// place while the slot is freed: the helper fails and the caller's rollback
// leaves everything as it was.
func TestFinalizeActivationSucceededRefusesSlotFlaggedByAnotherAttempt(t *testing.T) {
	a := newActivatedFixture(t, 233, true)
	a.blockViaTx2Failure(t)
	other := a.insertOtherAttempt(t)
	if _, err := a.pool.Exec(context.Background(), `UPDATE environment_slots SET reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, other); err != nil {
		t.Fatal(err)
	}
	_, err := a.finalizeFrom(t, "reconciliation_required")
	if !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
	}
	if state, _, _ := a.operation(t); state != "reconciliation_required" {
		t.Fatalf("operation=%s, want unchanged", state)
	}
	if _, current, recon, flagged := a.slotRow(t); current == nil || *current != a.attemptID || !flagged || recon == nil || *recon != other {
		t.Fatalf("slot current=%v recon=%v flagged=%v, want unchanged and still owned by %d", current, recon, flagged, other)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
	if state, _, _, _, _ := a.attemptRow(t, a.attemptID); state != "leased" {
		t.Fatalf("attempt=%s, want leased", state)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("route head flag must be unchanged")
	}
}

// Each zero-row check is a plain non-transient error; the end-to-end tx2
// behaviour of such an error is pinned by TestReportActivationResultCorrupt*.
func TestFinalizeActivationSucceededZeroRowChecks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seed     byte
		breakSQL string
	}{
		{"slot not held", 234, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=clock_timestamp() WHERE environment_id=$1`},
		{"job not leased", 235, `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp()+interval '1 minute', updated_at=clock_timestamp() WHERE id=(SELECT job_id FROM agent_job_attempts WHERE id=$1)`},
		{"attempt not leased", 236, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newActivatedFixture(t, tc.seed, false)
			arg := any(a.attemptID)
			if tc.name == "slot not held" {
				arg = a.envID
			}
			if _, err := a.pool.Exec(context.Background(), tc.breakSQL, arg); err != nil {
				t.Fatalf("break: %v", err)
			}
			_, err := a.finalizeFrom(t, "switch_authorized")
			if !errors.Is(err, ErrAgentLeaseCorruptState) {
				t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
			}
		})
	}
}

// An rr operation whose head flag is already clear must still resolve (false
// to false with revision+1 is legal); refusing would wedge the environment.
func TestFinalizeActivationSucceededFromReconciliationRequiredUnflaggedHead(t *testing.T) {
	a := newActivatedFixture(t, 237, true)
	a.blockViaTx2Failure(t)
	revision, _, _ := a.routeHead(t)
	if _, err := a.pool.Exec(context.Background(), `UPDATE route_heads SET reconciliation_required=false, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.finalizeFrom(t, "reconciliation_required"); err != nil {
		t.Fatalf("finalize with unflagged head: %v", err)
	}
	a.assertFinalizedFromReconciliation(t, revision+1)
}

// A switch_authorized operation with a flagged head is corrupt: finalize
// refuses before writing anything.
func TestFinalizeActivationSucceededRefusesFlaggedHeadFromSwitchAuthorized(t *testing.T) {
	a := newActivatedFixture(t, 238, true)
	a.setRouteHeadFlag(t, true)
	_, err := a.finalizeFrom(t, "switch_authorized")
	if !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("err=%v, want ErrAgentLeaseCorruptState", err)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("head flag must be untouched")
	}
}

// assertCorruptShapeBlocked asserts what tx2 must leave for a deterministic
// corrupt-state finalize failure after the occupant check: the typed terminal
// error, the durable block (operation rr with evidence, head and slot
// flagged), and nothing resolved.
func (a activatedFixture) assertCorruptShapeBlocked(t *testing.T, err error, wantEvidence string) {
	t.Helper()
	if !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("report err=%v, want ErrAgentLeaseCorruptState", err)
	}
	state, closedAt, summary := a.operation(t)
	if state != "reconciliation_required" || closedAt == nil || !strings.Contains(summary, wantEvidence) {
		t.Fatalf("operation state=%s closedAt=%v summary=%q, want rr naming %q", state, closedAt, summary, wantEvidence)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("head must be flagged")
	}
	if _, current, _, flagged := a.slotRow(t); !flagged || current == nil || *current != a.attemptID {
		t.Fatalf("slot current=%v flagged=%v, want occupied and flagged", current, flagged)
	}
	if got := a.deploymentStatus(t); got != deployment.StatusActivating {
		t.Fatalf("deployment=%s, want activating", got)
	}
	if s, _, _, _, _ := a.attemptRow(t, a.attemptID); s != "leased" {
		t.Fatalf("attempt=%s, want leased", s)
	}
	if s, _, _, _ := a.jobRow(t); s != "leased" {
		t.Fatalf("job=%s, want leased", s)
	}
	if got := strings.Join(a.jobEvents(t), ","); got != "activation_authorized,activation_reconciliation_required" {
		t.Fatalf("job events=%s, want exactly one block event", got)
	}
}

// Shape (a): the slot is flagged by ANOTHER attempt while the operation is
// still switch_authorized. Pinned actual behaviour: the block routine
// overwrites reconciliation_attempt_id with the reporting attempt (it is not
// changed by this step).
func TestReportActivationResultCorruptSlotFlaggedByOtherAttemptBlocks(t *testing.T) {
	a := newActivatedFixture(t, 239, true)
	other := a.insertOtherAttempt(t)
	if _, err := a.pool.Exec(context.Background(), `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, other); err != nil {
		t.Fatal(err)
	}
	err := a.report(context.Background(), true, "")
	a.assertCorruptShapeBlocked(t, err, "flagged by another attempt")
	if _, _, recon, _ := a.slotRow(t); recon == nil || *recon != a.attemptID {
		t.Fatalf("slot reconciliation_attempt_id=%v, want overwritten to %d (the other attempt's %d is lost)", recon, a.attemptID, other)
	}
}

// Shape (b): the head is flagged while the operation is switch_authorized.
// The block routine tolerates the already-flagged head (it skips the head
// write, which the 000020 trigger would reject as a no-op).
func TestReportActivationResultCorruptFlaggedHeadBlocks(t *testing.T) {
	a := newActivatedFixture(t, 240, true)
	a.setRouteHeadFlag(t, true)
	err := a.report(context.Background(), true, "")
	a.assertCorruptShapeBlocked(t, err, "flagged while operation")
}
