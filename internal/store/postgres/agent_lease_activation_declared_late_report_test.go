//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Step G2b: a report that contradicts a DECLARED outcome (the attempt has a
// 'reconciled' event) is traced with one activation_late_report event and
// still answered ErrAgentLeaseIntegrityConflict. Every declaration below is
// made by the real ResolveActivation / ReconcileActivation.

type declaredFixture struct {
	activatedFixture
	outcome string
}

// declare blocks the activation (real tx2 failure) and declares outcome via
// who ("operator" or "agent").
func declare(t *testing.T, seed byte, who, outcome string) declaredFixture {
	t.Helper()
	a := newActivatedFixture(t, seed, true)
	a.blockViaTx2Failure(t)
	var err error
	if who == "operator" {
		err = a.resolve(outcome)
	} else {
		err = a.reconcileActivation(context.Background(), a.agentCall(outcome))
	}
	if err != nil {
		t.Fatalf("%s declares %s: %v", who, outcome, err)
	}
	return declaredFixture{a, outcome}
}

func (a activatedFixture) wrongDigestSuccess(ctx context.Context) error {
	wrong := a.routeDigest
	wrong[2] ^= 0xff
	return a.repo.ReportActivationResult(ctx, a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
}

// stateWithoutEvents is the snapshot a traced report must leave unchanged:
// everything except the event list.
func (a activatedFixture) stateWithoutEvents(t *testing.T) resolveState {
	t.Helper()
	s := a.resolveState(t)
	s.events = nil
	return s
}

// assertTraced runs report: integrity_conflict, exactly one new
// activation_late_report event after the reconciled event, nothing else
// changed; a replay adds nothing.
func assertTraced(t *testing.T, a activatedFixture, report func() error) {
	t.Helper()
	beforeEvents := a.jobEvents(t)
	before := a.stateWithoutEvents(t)
	if err := report(); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	want := append(append([]string{}, beforeEvents...), "activation_late_report")
	if got := a.jobEvents(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	if after := a.stateWithoutEvents(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a traced report changed state:\nbefore %+v\nafter  %+v", before, after)
	}
	if err := report(); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("replay err=%v, want ErrAgentLeaseIntegrityConflict", err)
	}
	if got := a.jobEvents(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("replay changed events: %v, want %v", got, want)
	}
	if after := a.stateWithoutEvents(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a replayed report changed state")
	}
}

// assertNoEvent runs report and requires wantErr and an unchanged snapshot,
// events included.
func assertNoEvent(t *testing.T, a activatedFixture, report func() error, wantErr error) {
	t.Helper()
	before := a.resolveState(t)
	err := report()
	if wantErr == nil && err != nil || wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("report wrote state:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestDeclaredOutcomeContradictingReportIsTraced(t *testing.T) {
	ctx := context.Background()
	seed := byte(160)
	for _, tc := range []struct {
		who, outcome, kind string
	}{
		{"operator", "not_activated", "success"},
		{"agent", "not_activated", "success"},
		{"operator", "activated", "failure"},
		{"agent", "activated", "failure"},
		{"operator", "activated", "wrongdigest"},
		{"agent", "activated", "wrongdigest"},
	} {
		seed++
		t.Run(tc.who+"_"+tc.outcome+"_"+tc.kind, func(t *testing.T) {
			a := declare(t, seed, tc.who, tc.outcome)
			report := map[string]func() error{
				"success":     func() error { return a.report(ctx, true, "") },
				"failure":     func() error { return a.report(ctx, false, "late story") },
				"wrongdigest": func() error { return a.wrongDigestSuccess(ctx) },
			}[tc.kind]
			assertTraced(t, a.activatedFixture, report)
		})
	}
}

// Agreeing reports keep today's answers and write nothing: a matching success
// against a declared activated is the idempotent nil; a failure against a
// declared not_activated is still integrity_conflict (the failed arm has no
// agreeing path), untraced because it does not contradict the declaration.
func TestDeclaredOutcomeAgreeingReportsAreUntouched(t *testing.T) {
	ctx := context.Background()
	t.Run("activated_success", func(t *testing.T) {
		a := declare(t, 170, "operator", "activated")
		assertNoEvent(t, a.activatedFixture, func() error { return a.report(ctx, true, "") }, nil)
	})
	t.Run("not_activated_failure", func(t *testing.T) {
		a := declare(t, 171, "agent", "not_activated")
		assertNoEvent(t, a.activatedFixture, func() error { return a.report(ctx, false, "late story") }, ErrAgentLeaseIntegrityConflict)
	})
}

// An outcome finalized by tx2 itself (no reconciled event) is byte-identical
// to before: a conflicting report is answered and nothing is written.
func TestTx2FinalizedOutcomeConflictIsNotTraced(t *testing.T) {
	ctx := context.Background()
	a := newActivatedFixture(t, 172, true)
	if err := a.report(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	assertNoEvent(t, a, func() error { return a.report(ctx, false, "late story") }, ErrAgentLeaseIntegrityConflict)
	assertNoEvent(t, a, func() error { return a.wrongDigestSuccess(ctx) }, ErrAgentLeaseIntegrityConflict)
}

// A failed append or commit is retryable (a plain error, not the conflict),
// writes nothing, and the retry records exactly one event.
func TestDeclaredOutcomeTraceFailureIsRetryableAndLeavesNothing(t *testing.T) {
	ctx := context.Background()
	for i, stage := range []string{"declared_late_report_event", "commit"} {
		t.Run(stage, func(t *testing.T) {
			a := declare(t, byte(175+i), "operator", "not_activated")
			if stage == "commit" {
				a.installTraceCommitFailureOnce(t)
			} else {
				a.repo.failAfter = func(s string) error {
					if s == stage {
						return errors.New("injected at " + stage)
					}
					return nil
				}
			}
			before := a.resolveState(t)
			err := a.report(ctx, true, "")
			if err == nil || errors.Is(err, ErrAgentLeaseIntegrityConflict) || errors.Is(err, ErrAgentLeaseConstraintViolation) {
				t.Fatalf("err=%v, want a plain retryable error", err)
			}
			if after := a.resolveState(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a failed trace left a write:\nbefore %+v\nafter  %+v", before, after)
			}
			a.repo.failAfter = nil
			assertTraced(t, a.activatedFixture, func() error { return a.report(ctx, true, "") })
		})
	}
}

// installTraceCommitFailureOnce creates a REAL deferred constraint trigger that
// refuses the COMMIT of the first activation_late_report event for this job.
func (a activatedFixture) installTraceCommitFailureOnce(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	name := "test_trace_commit_once_" + a.operationID
	drop := func() {
		_, _ = a.pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+name+` ON agent_job_events`)
		_, _ = a.pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+name+`()`)
		_, _ = a.pool.Exec(ctx, `DROP SEQUENCE IF EXISTS `+name+`_seq`)
	}
	drop()
	if _, err := a.pool.Exec(ctx, `CREATE SEQUENCE `+name+`_seq`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(drop)
	if _, err := a.pool.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.event_type = 'activation_late_report' AND nextval('`+name+`_seq') = 1 THEN RAISE EXCEPTION 'test trace commit refused once'; END IF;
		RETURN NULL; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER `+name+` AFTER INSERT ON agent_job_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+name+`()`); err != nil {
		t.Fatal(err)
	}
}
