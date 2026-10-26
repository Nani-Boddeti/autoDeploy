//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Step H3 phase 1: properties the superseded Reconcile / MarkReconciled tests
// asserted that no ReconcileActivation / ResolveActivation test asserted on an
// equivalent state. Each test drives the no_activation outcome (the old plain
// behaviour) or the shapes the old methods were tested against.

func (a activatedFixture) reconciledEventCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='reconciled'`, a.jobID, a.attemptID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Agent path, plain block: the foreign server is denied with nothing written
// (checked at the attempt's own fence, not the slot's advanced one), the owning
// server clears the gate with exactly one 'reconciled' event and no fence
// change, a replay writes nothing, and only then can the queued job be claimed.
func TestReconcileActivationNoActivationUnblocksClaimAndIsServerScoped(t *testing.T) {
	ctx := context.Background()
	a := agentPlainBlock(t, 240)
	second := a.insertJob(t, a.serverID, a.envID, "second-"+releaseSuffix(t), 0)
	if leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5); err != nil || len(leases) != 0 {
		t.Fatalf("claim while blocked: leases=%+v err=%v, want none", leases, err)
	}
	fenceBefore, _, _, _ := a.slotRow(t)
	if fenceBefore <= 1 {
		t.Fatalf("setup: the close must advance the slot fence past the attempt's (got %d)", fenceBefore)
	}

	a.insertServer(t, "other-server")
	other := digest(241)
	a.mustExec(t, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, "other-server", other[:])
	c := a.agentCall(ActivationOutcomeNoActivation)
	c.serverID, c.cred = "other-server", other
	a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
	if _, _, _, flagged := a.slotRow(t); !flagged {
		t.Fatal("a foreign server must not clear the flag")
	}

	// Genuine call: the attempt's own fence (1), not the slot's advanced fence.
	eventsBefore := a.jobEvents(t)
	if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNoActivation)); err != nil {
		t.Fatal(err)
	}
	if fence, current, rec, flagged := a.slotRow(t); fence != fenceBefore || current != nil || rec != nil || flagged {
		t.Fatalf("slot fence=%d (was %d) current=%v rec=%v flagged=%v", fence, fenceBefore, current, rec, flagged)
	}
	if got, want := a.jobEvents(t), append(append([]string{}, eventsBefore...), "reconciled"); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	if n := a.reconciledEventCount(t); n != 1 {
		t.Fatalf("reconciled events=%d, want 1", n)
	}
	before := a.resolveState(t)
	if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNoActivation)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("replay wrote state")
	}
	if n := a.reconciledEventCount(t); n != 1 {
		t.Fatalf("reconciled events after replay=%d, want 1", n)
	}
	leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5)
	if err != nil || len(leases) != 1 || leases[0].JobID != second {
		t.Fatalf("claim after reconcile: leases=%+v err=%v, want the queued job", leases, err)
	}
}

// Operator path, plain block: a foreign owner is denied with nothing written;
// the owner clears the flag leaving the fence alone, with exactly one audit
// row (action environment_mark_reconciled) and exactly one 'reconciled' event.
func TestResolveActivationNoActivationIsOwnerScopedAndAuditedOnce(t *testing.T) {
	ctx := context.Background()
	a := plainBlock(t, 244)
	stranger := secondOwnerPrincipal(t, a.pool, NewAuthRepository(a.pool), 245)
	before := a.resolveState(t)
	if err := a.repo.ResolveActivation(ctx, stranger, a.envID, ActivationOutcomeNoActivation, "cross-owner break-glass attempt"); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner err=%v", err)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("cross-owner resolution wrote state")
	}
	if _, _, _, flagged := a.slotRow(t); !flagged {
		t.Fatal("cross-owner resolution must not clear the flag")
	}

	fenceBefore, _, _, _ := a.slotRow(t)
	eventsBefore := a.jobEvents(t)
	if err := a.repo.ResolveActivation(ctx, a.principal, a.envID, ActivationOutcomeNoActivation, "operator break-glass: agent unreachable"); err != nil {
		t.Fatal(err)
	}
	if fence, current, rec, flagged := a.slotRow(t); fence != fenceBefore || current != nil || rec != nil || flagged {
		t.Fatalf("slot fence=%d (was %d) current=%v rec=%v flagged=%v", fence, fenceBefore, current, rec, flagged)
	}
	audit := a.auditRows(t)
	if len(audit) != 1 || !strings.HasSuffix(audit[0], "|environment_mark_reconciled|success|outcome=no_activation; operator break-glass: agent unreachable") {
		t.Fatalf("audit=%v, want exactly one environment_mark_reconciled row", audit)
	}
	if got, want := a.jobEvents(t), append(append([]string{}, eventsBefore...), "reconciled"); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	if n := a.reconciledEventCount(t); n != 1 {
		t.Fatalf("reconciled events=%d, want 1", n)
	}
}

// no_activation over an attempt whose activate operation is TERMINAL. The
// succeeded shape (attempt closed) clears the flag on both paths, as the old
// MarkReconciled did. The failed shape (operation forced failed under a still
// leased attempt) is DELIBERATELY refused now: clearing it would leave a leased
// attempt holding the slot (G1 fix round, tasks/todo.md "G1 committed").
func TestNoActivationOverTerminalActivationOperation(t *testing.T) {
	ctx := context.Background()
	flag := func(t *testing.T, a activatedFixture) {
		a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, a.envID, a.attemptID)
	}
	for _, path := range []string{"operator", "agent"} {
		t.Run(path+"/succeeded", func(t *testing.T) {
			seed := byte(246)
			if path == "agent" {
				seed = 248
			}
			a := newActivatedFixture(t, seed, false)
			if err := a.report(ctx, true, ""); err != nil {
				t.Fatal(err)
			}
			flag(t, a)
			var err error
			if path == "operator" {
				err = a.resolve(ActivationOutcomeNoActivation)
			} else {
				err = a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNoActivation))
			}
			if err != nil {
				t.Fatalf("no_activation over a succeeded activation: %v", err)
			}
			if _, _, rec, flagged := a.slotRow(t); flagged || rec != nil {
				t.Fatal("slot flag must be cleared")
			}
			if n := a.reconciledEventCount(t); n != 1 {
				t.Fatalf("reconciled events=%d, want 1", n)
			}
		})
		t.Run(path+"/failed_under_leased_attempt", func(t *testing.T) {
			seed := byte(250)
			if path == "agent" {
				seed = 252
			}
			a := newActivatedFixture(t, seed, false)
			a.forceOperationState(t, "failed")
			flag(t, a)
			if path == "operator" {
				a.assertRefused(t, ActivationOutcomeNoActivation, ErrAgentLeaseCorruptState)
			} else {
				a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNoActivation), ErrServerAuthorizationDenied)
			}
		})
	}
}

// A CLOSED attempt that still carries an unresolved activate operation (the
// shape a missed close leaves behind): a plain no_activation declaration never
// clears it, on either path, whatever the operation's state. The kind check
// answers first (outcome mismatch); activated / not_activated meet the
// operation's state (switch_authorized: not resolvable) or the shape check
// (reconciliation_required over a closed attempt: corrupt state).
func TestNoActivationRefusesClosedAttemptWithUnresolvedActivation(t *testing.T) {
	for i, state := range []string{"reconciliation_required", "switch_authorized"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixture(t, byte(232+2*i), true)
			if state == "reconciliation_required" {
				a.blockViaTx2Failure(t)
			} else {
				// Forced in SQL: the real code never flags a slot over a
				// switch_authorized operation.
				a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, a.attemptID)
			}
			// Forced in SQL: close the attempt without resolving the operation.
			a.mustExec(t, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, a.attemptID)

			a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNoActivation), ErrAgentLeaseOutcomeMismatch)
			a.assertRefused(t, ActivationOutcomeNoActivation, ErrAgentLeaseOutcomeMismatch)

			wantDeclared := ErrAgentLeaseCorruptState
			if state == "switch_authorized" {
				wantDeclared = ErrAgentLeaseActivationUnresolved
			}
			for _, o := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
				a.assertAgentRefused(t, a.agentCall(o), wantDeclared)
				a.assertRefused(t, o, wantDeclared)
			}
			if _, _, rec, flagged := a.slotRow(t); !flagged || rec == nil {
				t.Fatal("the flag must survive every refusal")
			}
		})
	}
}

// The legacy sweeper close (attempt closed, slot freed, job retry_wait) of a
// switch_authorized activation, as already-deployed databases could hold it:
// a plain no_activation declaration is refused on both paths, nothing written.
func TestNoActivationRefusesLegacyClosedSwitchAuthorized(t *testing.T) {
	a := newActivatedFixture(t, 236, false)
	a.forceLegacySweeperClose(t)
	if state, _, _ := a.operation(t); state != "switch_authorized" {
		t.Fatalf("operation=%s, want switch_authorized", state)
	}
	a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNoActivation), ErrAgentLeaseOutcomeMismatch)
	a.assertRefused(t, ActivationOutcomeNoActivation, ErrAgentLeaseOutcomeMismatch)
	for _, o := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		a.assertAgentRefused(t, a.agentCall(o), ErrAgentLeaseActivationUnresolved)
		a.assertRefused(t, o, ErrAgentLeaseActivationUnresolved)
	}
	if _, _, rec, flagged := a.slotRow(t); !flagged || rec == nil {
		t.Fatal("the flag must survive every refusal")
	}
}
