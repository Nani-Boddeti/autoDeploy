//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
)

// Step G2a: ReconcileActivation, the agent path of the declared resolution,
// and the explicit unresolved-activation check in Reconcile. Every block is
// produced by the real Activate and the real tx2 / sweeper / RevokeServer;
// the only forced SQL is stated at its use.

// agentCall is one ReconcileActivation request; the zero edits below build
// each refusal from the genuine call.
type agentCall struct {
	serverID         string
	cred             [32]byte
	envID            string
	attemptID, fence int64
	outcome          string
	digest           [32]byte
}

func (a activatedFixture) agentCall(outcome string) agentCall {
	return agentCall{a.serverID, a.cred, a.envID, a.attemptID, 1, outcome, a.routeDigest}
}

func (a activatedFixture) reconcileActivation(ctx context.Context, c agentCall) error {
	return a.repo.ReconcileActivation(ctx, c.serverID, c.cred, c.envID, c.attemptID, c.fence, c.outcome, c.digest)
}

// assertAgentRefused calls ReconcileActivation and requires wantErr and no
// write (the full snapshot, audit rows included).
func (a activatedFixture) assertAgentRefused(t *testing.T, c agentCall, wantErr error) {
	t.Helper()
	before := a.resolveState(t)
	if err := a.reconcileActivation(context.Background(), c); !errors.Is(err, wantErr) {
		t.Fatalf("ReconcileActivation(%q) err=%v, want %v", c.outcome, err, wantErr)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused ReconcileActivation(%q) wrote state:\nbefore %+v\nafter  %+v", c.outcome, before, after)
	}
}

// assertAgentResolved asserts the full post-state of an agent resolution: the
// same rows as the operator's, and NO audit row.
func (a activatedFixture) assertAgentResolved(t *testing.T, outcome string, p blockedPrecondition) {
	t.Helper()
	a.assertResolvedAudit(t, outcome, p, false)
}

var allOutcomes = []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated, ActivationOutcomeNoActivation}

// agentBlock is a plain (non-activation) block with a server credential, so
// the agent can address it.
func agentPlainBlock(t *testing.T, seed byte) activatedFixture {
	t.Helper()
	a := plainBlock(t, seed)
	a.cred = a.withCredential(t, seed+50)
	return a
}

func TestReconcileActivationPostStateMatrix(t *testing.T) {
	seed := byte(60)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		for _, cause := range blockCauses[:3] { // revoke: the agent is refused, see TestReconcileActivationRevokedServer
			for _, withPredecessor := range []bool{true, false} {
				seed += 2
				t.Run(fmt.Sprintf("%s/%s/pred%t", outcome, cause.name, withPredecessor), func(t *testing.T) {
					a := newActivatedFixture(t, seed, withPredecessor)
					cause.block(t, a)
					p := a.blockedPrecondition(t)
					if err := a.reconcileActivation(context.Background(), a.agentCall(outcome)); err != nil {
						t.Fatalf("ReconcileActivation: %v", err)
					}
					a.assertAgentResolved(t, outcome, p)
				})
			}
		}
	}
}

// A repeat is anchored to the GIVEN attempt: idempotent when it agrees,
// integrity conflict when it does not, and it never writes.
func TestReconcileActivationIdempotentRepeat(t *testing.T) {
	ctx := context.Background()
	for i, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		t.Run(outcome, func(t *testing.T) {
			a := newActivatedFixture(t, byte(90+2*i), true)
			a.blockViaTx2Failure(t)
			if err := a.reconcileActivation(ctx, a.agentCall(outcome)); err != nil {
				t.Fatal(err)
			}
			before := a.resolveState(t)
			if err := a.reconcileActivation(ctx, a.agentCall(outcome)); err != nil {
				t.Fatalf("repeat err=%v, want nil", err)
			}
			for _, other := range allOutcomes {
				if other == outcome {
					continue
				}
				if err := a.reconcileActivation(ctx, a.agentCall(other)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
					t.Fatalf("repeat as %s err=%v, want integrity conflict", other, err)
				}
			}
			if outcome == ActivationOutcomeActivated {
				c := a.agentCall(outcome)
				c.digest[7] ^= 0xff
				if err := a.reconcileActivation(ctx, c); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
					t.Fatalf("repeat with a wrong digest err=%v, want integrity conflict", err)
				}
			}
			// The identity of the caller is still checked on a repeat.
			c := a.agentCall(outcome)
			c.fence = 2
			if err := a.reconcileActivation(ctx, c); !errors.Is(err, ErrServerAuthorizationDenied) {
				t.Fatalf("repeat at the wrong fence err=%v, want denied", err)
			}
			if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
				t.Fatal("a repeat wrote state")
			}
		})
	}
}

// The repeat is anchored to the attempt the caller names, not to the
// environment's latest attempt (that is the operator's anchor).
func TestReconcileActivationReplayIsAnchoredToGivenAttempt(t *testing.T) {
	ctx := context.Background()
	t.Run("a newer attempt exists", func(t *testing.T) {
		a := newActivatedFixture(t, 100, true)
		a.blockViaSweeperDiversion(t)
		if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNotActivated)); err != nil {
			t.Fatal(err)
		}
		a.insertJob(t, a.serverID, a.envID, "attempt-b", 0)
		if leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5); err != nil || len(leases) != 1 {
			t.Fatalf("claim B: leases=%d err=%v", len(leases), err)
		}
		before := a.resolveState(t)
		if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNotActivated)); err != nil {
			t.Fatalf("repeat of A after B was claimed err=%v, want nil", err)
		}
		if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeActivated)); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("contradicting repeat of A err=%v, want integrity conflict", err)
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatal("a repeat wrote state")
		}
	})
	t.Run("closed attempt never reconciled", func(t *testing.T) {
		a := agentPlainBlock(t, 102)
		// Forced in SQL: the flag is cleared without the 'reconciled' event a
		// real reconciliation appends, so the closed attempt has no event.
		a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID)
		for _, outcome := range allOutcomes {
			a.assertAgentRefused(t, a.agentCall(outcome), ErrServerAuthorizationDenied)
		}
	})
	t.Run("leased attempt of a healthy slot", func(t *testing.T) {
		a := newActivatedFixture(t, 104, true) // leased, nothing pending
		for _, outcome := range allOutcomes {
			a.assertAgentRefused(t, a.agentCall(outcome), ErrServerAuthorizationDenied)
		}
	})
	t.Run("leased attempt with a reconciled event", func(t *testing.T) {
		a := newActivatedFixture(t, 106, true)
		// Forced in SQL: a 'reconciled' event for a still-leased attempt (no
		// real path writes one); a repeat is honoured only for a CLOSED attempt.
		a.mustExec(t, `INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) SELECT $1, COALESCE(MAX(sequence),0)+1, 'reconciled', $2 FROM agent_job_events WHERE job_id=$1`, a.jobID, a.attemptID)
		for _, outcome := range allOutcomes {
			a.assertAgentRefused(t, a.agentCall(outcome), ErrServerAuthorizationDenied)
		}
	})
}

// A revoked server cannot authenticate: the agent is refused with nothing
// written, and the operator can still resolve the very same environment.
func TestReconcileActivationRevokedServer(t *testing.T) {
	for i, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		t.Run(outcome, func(t *testing.T) {
			a := newActivatedFixture(t, byte(110+2*i), true)
			blockCauses[3].block(t, a)
			p := a.blockedPrecondition(t)
			for _, o := range allOutcomes {
				a.assertAgentRefused(t, a.agentCall(o), ErrServerAuthorizationDenied)
			}
			if err := a.resolve(outcome); err != nil {
				t.Fatalf("operator ResolveActivation after the agent was refused: %v", err)
			}
			a.assertResolved(t, outcome, p)
		})
	}
}

// Owner decision Q2: an agent activated declaration must present the digest
// the operation authorized. not_activated does not depend on it.
func TestReconcileActivationDigestRule(t *testing.T) {
	t.Run("mismatch refused", func(t *testing.T) {
		a := newActivatedFixture(t, 120, true)
		a.blockViaTx2Failure(t)
		var stages []string
		a.repo.failAfter = func(s string) error { stages = append(stages, s); return nil }
		defer func() { a.repo.failAfter = nil }()
		for _, bad := range [][32]byte{{}, func() [32]byte { d := a.routeDigest; d[31] ^= 1; return d }()} {
			c := a.agentCall(ActivationOutcomeActivated)
			c.digest = bad
			a.assertAgentRefused(t, c, ErrAgentLeaseIntegrityConflict)
		}
		if len(stages) != 0 {
			t.Fatalf("writes ran before the digest refusal: %v", stages)
		}
	})
	t.Run("not_activated ignores it", func(t *testing.T) {
		a := newActivatedFixture(t, 122, true)
		a.blockViaTx2Failure(t)
		p := a.blockedPrecondition(t)
		c := a.agentCall(ActivationOutcomeNotActivated)
		c.digest = [32]byte{}
		if err := a.reconcileActivation(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		a.assertAgentResolved(t, ActivationOutcomeNotActivated, p)
	})
}

func TestReconcileActivationOutcomeValidation(t *testing.T) {
	a := newActivatedFixture(t, 124, true)
	a.blockViaTx2Failure(t)
	for _, bad := range []string{"", "bogus", "Activated", "activated "} {
		a.assertAgentRefused(t, a.agentCall(bad), ErrAgentLeaseInvalidInput)
	}
	c := a.agentCall(ActivationOutcomeActivated)
	c.fence = 0
	a.assertAgentRefused(t, c, ErrAgentLeaseInvalidInput)
}

// Each refusal below leaves the full snapshot untouched, audit rows included.
func TestReconcileActivationRefusals(t *testing.T) {
	blocked := func(t *testing.T, seed byte) activatedFixture {
		a := newActivatedFixture(t, seed, true)
		a.blockViaTx2Failure(t)
		return a
	}
	t.Run("wrong_server", func(t *testing.T) {
		a := blocked(t, 130)
		a.insertServer(t, "other-server")
		other := digest(131)
		a.mustExec(t, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, "other-server", other[:])
		for _, o := range allOutcomes {
			c := a.agentCall(o)
			c.serverID, c.cred = "other-server", other
			a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
		}
	})
	t.Run("foreign_server_does_not_queue_on_job", func(t *testing.T) {
		a := blocked(t, 133)
		a.insertServer(t, "other-server")
		other := digest(135)
		a.mustExec(t, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, "other-server", other[:])
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// A third transaction holds the attempt's job row. The identity check
		// runs on the non-locking peek, so the foreign call is denied without
		// ever waiting on it.
		holder, err := a.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
		if _, err := holder.Exec(ctx, `SELECT 1 FROM agent_jobs WHERE id=$1 FOR UPDATE`, a.jobID); err != nil {
			t.Fatal(err)
		}
		for _, o := range allOutcomes {
			c := a.agentCall(o)
			c.serverID, c.cred = "other-server", other
			callCtx, callCancel := context.WithTimeout(ctx, 3*time.Second)
			err := a.reconcileActivation(callCtx, c)
			callCancel()
			if !errors.Is(err, ErrServerAuthorizationDenied) {
				t.Fatalf("foreign server %s err=%v, want denied without waiting on the job row", o, err)
			}
		}
	})
	t.Run("wrong_credential", func(t *testing.T) {
		a := blocked(t, 132)
		for _, o := range allOutcomes {
			c := a.agentCall(o)
			c.cred = digest(1)
			a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
		}
	})
	t.Run("wrong_fence", func(t *testing.T) {
		a := blocked(t, 134)
		for _, o := range allOutcomes {
			c := a.agentCall(o)
			c.fence = 2
			a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
		}
	})
	t.Run("wrong_env_flagged", func(t *testing.T) {
		a := blocked(t, 136)
		// Forced in SQL: another environment's slot names this attempt as its
		// pending one (reachable only through corruption), so the slot gate
		// passes and the attempt's own environment is the guard.
		a.mustExec(t, `INSERT INTO environment_slots(environment_id) VALUES ('other-env')`)
		a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$1, updated_at=clock_timestamp() WHERE environment_id='other-env'`, a.attemptID)
		for _, o := range allOutcomes {
			c := a.agentCall(o)
			c.envID = "other-env"
			a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
		}
	})
	t.Run("unknown_env_and_attempt", func(t *testing.T) {
		a := blocked(t, 138)
		for _, o := range allOutcomes {
			c := a.agentCall(o)
			c.envID = "no-such-env"
			a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
			c = a.agentCall(o)
			c.attemptID = a.attemptID + 1000
			a.assertAgentRefused(t, c, ErrServerAuthorizationDenied)
		}
	})
	t.Run("foreign_flag", func(t *testing.T) {
		a := blocked(t, 140)
		other := a.insertOtherAttempt(t)
		// Forced in SQL: the slot flag names an attempt other than the one the
		// unresolved operation belongs to.
		a.mustExec(t, `UPDATE environment_slots SET reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, other)
		for _, o := range allOutcomes {
			// The caller names the operation's attempt: the slot does not point at it.
			a.assertAgentRefused(t, a.agentCall(o), ErrServerAuthorizationDenied)
			// The caller names the flagged attempt: the operation is not its own.
			c := a.agentCall(o)
			c.attemptID = other
			a.assertAgentRefused(t, c, ErrAgentLeaseCorruptState)
		}
	})
	t.Run("switch_authorized", func(t *testing.T) {
		a := newActivatedFixture(t, 142, true)
		// Forced in SQL: the real code never flags a slot over a
		// switch_authorized operation (the host switch may be in flight).
		a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, a.attemptID)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseActivationUnresolved)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNotActivated), ErrAgentLeaseActivationUnresolved)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNoActivation), ErrAgentLeaseOutcomeMismatch)
	})
	t.Run("kind_plain_block", func(t *testing.T) {
		a := agentPlainBlock(t, 144)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseOutcomeMismatch)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNotActivated), ErrAgentLeaseOutcomeMismatch)
	})
	t.Run("kind_activation_block", func(t *testing.T) {
		a := blocked(t, 146)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNoActivation), ErrAgentLeaseOutcomeMismatch)
	})
	t.Run("operation_fence", func(t *testing.T) {
		a := blocked(t, 148)
		// Forced in SQL (trigger disabled for the one statement): the
		// operation's fence differs from the attempt's.
		withTriggerDisabled(t, a, "release_operations", "release_operations_validate_update", func() {
			a.mustExec(t, `UPDATE release_operations SET fence=fence+5 WHERE environment_id=$1`, a.envID)
		})
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseCorruptState)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNotActivated), ErrAgentLeaseCorruptState)
	})
	t.Run("leased_plain_flag", func(t *testing.T) {
		f := newLeaseFixture(t, 150)
		f.insertRouteHead(t, "app.example.com")
		attemptID := f.leaseJob(t, 1, 90)
		a := activatedFixture{leaseFixture: f, attemptID: attemptID, repo: NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))}
		a.cred = f.withCredential(t, 200)
		// Forced in SQL: flag the slot over a still-leased occupant with no
		// operation; Reconcile's rule (only a closed attempt) denies it.
		a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, attemptID)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNoActivation), ErrServerAuthorizationDenied)
		a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseOutcomeMismatch)
	})
}

// Each pre-verify guard on its own, agent entry: the block is real, then one
// precondition is broken in SQL. The refusal must come before any write.
func TestReconcileActivationPreVerifyGuards(t *testing.T) {
	for i, tc := range []struct {
		name    string
		breakIt func(*testing.T, activatedFixture)
	}{
		{"attempt_closed", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, a.attemptID)
		}},
		{"job_not_leased", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `UPDATE agent_jobs SET state='failed', updated_at=clock_timestamp() WHERE id=$1`, a.jobID)
		}},
		{"slot_not_held", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID)
		}},
		{"deployment_not_activating", func(t *testing.T, a activatedFixture) { a.setDeploymentStatus(t, deployment.StatusFailed) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newActivatedFixture(t, byte(160+2*i), true)
			a.blockViaTx2Failure(t)
			tc.breakIt(t, a)
			var stages []string
			a.repo.failAfter = func(s string) error { stages = append(stages, s); return nil }
			defer func() { a.repo.failAfter = nil }()
			a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseCorruptState)
			a.assertAgentRefused(t, a.agentCall(ActivationOutcomeNotActivated), ErrAgentLeaseCorruptState)
			if len(stages) != 0 {
				t.Fatalf("writes ran before the refusal: stages %v", stages)
			}
		})
	}
}

func TestReconcileActivationRouteMovedWritesNothing(t *testing.T) {
	a := newActivatedFixture(t, 170, false)
	a.blockViaTx2Failure(t)
	// Forced in SQL: AssignRouteHostname can move the head while blocked.
	a.mustExec(t, `UPDATE route_heads SET hostname='moved.example.com', route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID)
	a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseActivationRouteMoved)
}

func TestReconcileActivationRollsBackAtEveryStage(t *testing.T) {
	injected := errors.New("injected")
	for i, stage := range []string{"deployment", "operation_close", "attempt_close", "slot_free", "job_close", "job_event"} {
		t.Run(stage, func(t *testing.T) {
			a := newActivatedFixture(t, byte(172+2*i), true)
			a.blockViaTx2Failure(t)
			before := a.resolveState(t)
			a.repo.failAfter = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			if err := a.reconcileActivation(context.Background(), a.agentCall(ActivationOutcomeNotActivated)); !errors.Is(err, injected) {
				t.Fatalf("err=%v, want injected failure at %s", err, stage)
			}
			a.repo.failAfter = nil
			if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("failure at %s left state behind (a new block must not be written either)", stage)
			}
			if err := a.reconcileActivation(context.Background(), a.agentCall(ActivationOutcomeNotActivated)); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
		})
	}
}

// no_activation carries Reconcile's semantics: the closed attempt's flag is
// cleared, 'reconciled' appended, no audit row; the repeat is idempotent.
func TestReconcileActivationNoActivation(t *testing.T) {
	ctx := context.Background()
	a := agentPlainBlock(t, 190)
	if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNoActivation)); err != nil {
		t.Fatal(err)
	}
	if fence, current, rec, flagged := a.slotRow(t); current != nil || rec != nil || flagged {
		t.Fatalf("slot fence=%d current=%v rec=%v flagged=%v", fence, current, rec, flagged)
	}
	if got := strings.Join(a.jobEvents(t), ","); !strings.HasSuffix(got, ",reconciled") {
		t.Fatalf("events=%s", got)
	}
	if audit := a.auditRows(t); len(audit) != 0 {
		t.Fatalf("audit rows=%v, want none", audit)
	}
	before := a.resolveState(t)
	if err := a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNoActivation)); err != nil {
		t.Fatalf("repeat err=%v", err)
	}
	a.assertAgentRefused(t, a.agentCall(ActivationOutcomeActivated), ErrAgentLeaseIntegrityConflict)
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("repeat wrote state")
	}
}

// Reconcile (no outcome) must refuse a closed attempt that still carries an
// unresolved activation: corrupt state, nothing written, never a flag clear.
func TestReconcileRefusesClosedAttemptWithUnresolvedActivation(t *testing.T) {
	ctx := context.Background()
	for i, state := range []string{"reconciliation_required", "switch_authorized"} {
		t.Run(state, func(t *testing.T) {
			a := newActivatedFixture(t, byte(200+2*i), true)
			if state == "reconciliation_required" {
				a.blockViaTx2Failure(t)
			} else {
				// Forced in SQL: the real code never flags a slot over a
				// switch_authorized operation.
				a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, a.attemptID)
			}
			reconcile := func() error {
				return a.repo.Reconcile(ctx, a.serverID, a.cred, a.envID, a.attemptID, 1, digest(3))
			}
			// A leased attempt is still denied, as before.
			before := a.resolveState(t)
			if err := reconcile(); !errors.Is(err, ErrServerAuthorizationDenied) {
				t.Fatalf("leased attempt err=%v, want denied", err)
			}
			if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
				t.Fatal("denied Reconcile of a leased attempt wrote state")
			}
			// Forced in SQL: close the attempt without resolving the operation
			// (the shape a missed close leaves behind).
			a.mustExec(t, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, a.attemptID)
			before = a.resolveState(t)
			if err := reconcile(); !errors.Is(err, ErrAgentLeaseCorruptState) {
				t.Fatalf("closed attempt with an unresolved activation err=%v, want corrupt state", err)
			}
			if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
				t.Fatal("refused Reconcile wrote state")
			}
		})
	}
}

// End to end: block via the real sweeper, the agent resolves, a NEW attempt
// claims and activates.
func TestReconcileActivationUnwedgesEnvironment(t *testing.T) {
	seed := byte(210)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		t.Run(outcome, func(t *testing.T) {
			seed += 2
			ctx := context.Background()
			a := newActivatedFixture(t, seed, true)
			a.blockViaSweeperDiversion(t)
			next := a.insertJob(t, a.serverID, a.envID, "next-"+releaseSuffix(t), 0)
			if leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5); err != nil || len(leases) != 0 {
				t.Fatalf("claim while blocked: leases=%d err=%v, want none", len(leases), err)
			}
			if err := a.reconcileActivation(ctx, a.agentCall(outcome)); err != nil {
				t.Fatal(err)
			}
			leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5)
			if err != nil || len(leases) != 1 || leases[0].JobID != next {
				t.Fatalf("claim after resolution: leases=%+v err=%v", leases, err)
			}
			a.assertNewAttemptActivates(t, outcome, next, leases[0])
		})
	}
}

// --- concurrency (run with -race) ---

const slotWaiters = "FROM environment_slots%FOR UPDATE"

// The agent and the operator race on the same slot (two backends queued
// behind a third transaction, confirmed). Exactly one resolves; the other is
// the idempotent answer or a typed refusal, never corruption.
func TestReconcileActivationRacesOperatorResolution(t *testing.T) {
	seed := byte(220)
	for _, agentFirst := range []bool{true, false} {
		for _, pair := range [][2]string{
			{ActivationOutcomeActivated, ActivationOutcomeActivated},
			{ActivationOutcomeNotActivated, ActivationOutcomeNotActivated},
			{ActivationOutcomeActivated, ActivationOutcomeNotActivated},
			{ActivationOutcomeNotActivated, ActivationOutcomeActivated},
		} {
			seed += 2
			agentOutcome, operatorOutcome := pair[0], pair[1]
			t.Run(fmt.Sprintf("agentFirst%t/%s_vs_%s", agentFirst, agentOutcome, operatorOutcome), func(t *testing.T) {
				a := newActivatedFixture(t, seed, true)
				a.blockViaTx2Failure(t)
				p := a.blockedPrecondition(t)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				release := a.holdSlot(t, ctx)
				var agentErr, operatorErr error
				var wg sync.WaitGroup
				startAgent := func() {
					wg.Add(1)
					go func() { defer wg.Done(); agentErr = a.reconcileActivation(ctx, a.agentCall(agentOutcome)) }()
				}
				startOperator := func() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						operatorErr = a.repo.ResolveActivation(ctx, a.principal, a.envID, operatorOutcome, resolveReason)
					}()
				}
				if agentFirst {
					startAgent()
					waitForLockWaiters(t, ctx, a.pool, slotWaiters, 1)
					startOperator()
				} else {
					startOperator()
					waitForLockWaiters(t, ctx, a.pool, slotWaiters, 1)
					startAgent()
				}
				waitForLockWaiters(t, ctx, a.pool, slotWaiters, 2)
				release()
				wg.Wait()
				firstOutcome, firstErr, secondErr := agentOutcome, agentErr, operatorErr
				if !agentFirst {
					firstOutcome, firstErr, secondErr = operatorOutcome, operatorErr, agentErr
				}
				if firstErr != nil {
					t.Fatalf("first resolution err=%v", firstErr)
				}
				if agentOutcome == operatorOutcome {
					if secondErr != nil {
						t.Fatalf("second resolution err=%v, want the idempotent answer", secondErr)
					}
				} else if !errors.Is(secondErr, ErrAgentLeaseIntegrityConflict) {
					t.Fatalf("second resolution err=%v, want integrity conflict", secondErr)
				}
				a.assertResolvedAudit(t, firstOutcome, p, !agentFirst)
			})
		}
	}
}

// The agent and a late tx2 report queue on the same slot; either order leaves
// a consistent terminal state (the report is evidence before, refused after).
func TestReconcileActivationRacesLateReport(t *testing.T) {
	seed := byte(40)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		for _, reportFirst := range []bool{true, false} {
			seed++
			t.Run(fmt.Sprintf("%s/reportFirst%t", outcome, reportFirst), func(t *testing.T) {
				a := newActivatedFixture(t, seed, true)
				a.blockViaTx2Failure(t)
				p := a.blockedPrecondition(t)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				release := a.holdSlot(t, ctx)
				var agentErr, reportErr error
				var wg sync.WaitGroup
				startAgent := func() {
					wg.Add(1)
					go func() { defer wg.Done(); agentErr = a.reconcileActivation(ctx, a.agentCall(outcome)) }()
				}
				startReport := func() {
					wg.Add(1)
					go func() { defer wg.Done(); reportErr = a.lateReport(ctx, "success wrong digest") }()
				}
				if reportFirst {
					startReport()
					waitForLockWaiters(t, ctx, a.pool, slotWaiters, 1)
					startAgent()
				} else {
					startAgent()
					waitForLockWaiters(t, ctx, a.pool, slotWaiters, 1)
					startReport()
				}
				waitForLockWaiters(t, ctx, a.pool, slotWaiters, 2)
				release()
				wg.Wait()
				if agentErr != nil {
					t.Fatalf("agent resolution err=%v", agentErr)
				}
				_, _, summary := a.operation(t)
				if reportFirst {
					if !errors.Is(reportErr, ErrAgentLeaseActivationLateReportRecorded) {
						t.Fatalf("report err=%v, want recorded as evidence", reportErr)
					}
					if !strings.Contains(summary, "late report #") || !strings.HasSuffix(summary, " | declared "+outcome) {
						t.Fatalf("summary=%q", summary)
					}
					p.opSummary = strings.TrimSuffix(summary, " | declared "+outcome)
					p.lateEvents = "activation_late_report,"
				} else if !errors.Is(reportErr, ErrAgentLeaseIntegrityConflict) {
					// The operation is terminal and the fence matches, so the
					// terminal arm (succeeded / failed) answers; Stale needs a
					// fence mismatch or a missing operation.
					t.Fatalf("report after resolution err=%v, want integrity conflict", reportErr)
				} else {
					// Declared outcome contradicted: traced once (step G2b).
					p.afterEvents = ",activation_late_report"
				}
				a.assertAgentResolved(t, outcome, p)
			})
		}
	}
}

// The agent vs the sweeper's per-candidate transaction (expireOne).
func TestReconcileActivationRacesExpireOne(t *testing.T) {
	t.Run("sweeper_first", func(t *testing.T) {
		// expireOne is parked at its operation_lock hook over a
		// switch_authorized operation holding slot/job/attempt/operation; the
		// agent queues on the slot and, after the sweeper commits its block,
		// sees reconciliation_required and resolves.
		a := newActivatedFixture(t, 160, true)
		a.lapse(t, "2 seconds")
		reached, proceed := make(chan struct{}), make(chan struct{})
		var unpark sync.Once
		t.Cleanup(func() { unpark.Do(func() { close(proceed) }) })
		sweeper := NewAgentLeaseRepository(a.pool, NewDeploymentRepository(a.pool))
		sweeper.failAfter = func(stage string) error {
			if stage == "operation_lock" {
				close(reached)
				<-proceed
			}
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var sweepErr, agentErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _, sweepErr = sweeper.expireOne(ctx, a.attemptID) }()
		<-reached
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentErr = a.reconcileActivation(ctx, a.agentCall(ActivationOutcomeNotActivated))
		}()
		waitForLockWaiters(t, ctx, a.pool, slotWaiters, 1)
		unpark.Do(func() { close(proceed) })
		wg.Wait()
		if sweepErr != nil || agentErr != nil {
			t.Fatalf("sweep err=%v agent err=%v", sweepErr, agentErr)
		}
		if state, _, summary := a.operation(t); state != "failed" || !strings.HasPrefix(summary, activationBlockedByAuthorityEndSummary) || !strings.HasSuffix(summary, " | declared not_activated") {
			t.Fatalf("operation=%s summary=%q", state, summary)
		}
		if got := strings.Join(a.jobEvents(t), ","); got != "activation_authorized,activation_reconciliation_required,activation_not_activated,failed,reconciled" {
			t.Fatalf("events=%s", got)
		}
	})
	t.Run("agent_first", func(t *testing.T) {
		// The agent holds every lock (parked after its writes); the sweeper's
		// SKIP LOCKED slot lock finds the slot taken and skips. After the
		// commit the attempt is closed and a second sweep is a no-op too.
		a := newActivatedFixture(t, 162, true)
		a.blockViaTx2Failure(t)
		a.lapse(t, "2 seconds")
		reached, proceed := make(chan struct{}), make(chan struct{})
		var unpark, park sync.Once
		t.Cleanup(func() { unpark.Do(func() { close(proceed) }) })
		resolver := NewAgentLeaseRepository(a.pool, NewDeploymentRepository(a.pool))
		resolver.failAfter = func(stage string) error {
			if stage == "job_event" {
				park.Do(func() { close(reached) })
				<-proceed
			}
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var agentErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentErr = resolver.ReconcileActivation(ctx, a.serverID, a.cred, a.envID, a.attemptID, 1, ActivationOutcomeActivated, a.routeDigest)
		}()
		<-reached
		closed, err := a.repo.expireOne(ctx, a.attemptID)
		if err != nil || closed {
			t.Fatalf("expireOne during resolution closed=%v err=%v, want a skipped no-op", closed, err)
		}
		unpark.Do(func() { close(proceed) })
		wg.Wait()
		if agentErr != nil {
			t.Fatal(agentErr)
		}
		if closed, err = a.repo.expireOne(ctx, a.attemptID); err != nil || closed {
			t.Fatalf("expireOne after resolution closed=%v err=%v", closed, err)
		}
		if got := strings.Join(a.jobEvents(t), ","); got != "activation_authorized,activation_reconciliation_required,activated,succeeded,reconciled" {
			t.Fatalf("events=%s", got)
		}
	})
}
