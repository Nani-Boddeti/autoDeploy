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

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
)

// Step G1: ResolveActivation, the operator's declared resolution of a blocked
// environment. Every block below is produced by the real Activate and the
// real tx2 / sweeper / RevokeServer; the only forced SQL is stated at its use.

const resolveReason = "operator verified the host"

type blockCause struct {
	name  string
	block func(*testing.T, activatedFixture)
}

var blockCauses = []blockCause{
	{"tx2", func(t *testing.T, a activatedFixture) { a.blockViaTx2Failure(t) }},
	{"ambig", func(t *testing.T, a activatedFixture) {
		wrong := a.routeDigest
		wrong[5] ^= 0xff
		err := a.repo.ReportActivationResult(context.Background(), a.serverID, a.cred, a.attemptID, 1, a.operationID, true, wrong, "")
		if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("ambiguous success report err=%v, want integrity conflict", err)
		}
	}},
	{"sweep", func(t *testing.T, a activatedFixture) { a.blockViaSweeperDiversion(t) }},
	{"revoke", func(t *testing.T, a activatedFixture) {
		if err := NewServerRepository(a.pool).RevokeServer(context.Background(), a.principal, a.serverID, "compromised host"); err != nil {
			t.Fatalf("RevokeServer: %v", err)
		}
	}},
}

// resolveState is everything a refused or repeated resolution must leave
// byte-identical.
type resolveState struct {
	attempt, slot, job, deployment, routeHead, operation string
	events                                               []string
	releases                                             string
	audit                                                []string
	routeEvents                                          int
	reconciled                                           int
}

func (a activatedFixture) auditRows(t *testing.T) []string {
	t.Helper()
	rows, err := a.pool.Query(context.Background(), `SELECT actor_id||'|'||action||'|'||result||'|'||reason FROM server_audit_events WHERE action='environment_mark_reconciled' ORDER BY id`)
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

func (a activatedFixture) resolveState(t *testing.T) resolveState {
	t.Helper()
	// Aggregated so a shape without a route head or operation (a plain block)
	// is a value, not an error.
	rows := func(query string, args ...any) string {
		return a.rowJSON(t, `SELECT coalesce(jsonb_agg(to_jsonb(q)), '[]'::jsonb) AS rs FROM (`+query+`) q`, args...)
	}
	s := resolveState{
		attempt:    rows(`SELECT * FROM agent_job_attempts WHERE id=$1`, a.attemptID),
		slot:       rows(`SELECT * FROM environment_slots WHERE environment_id=$1`, a.envID),
		job:        rows(`SELECT * FROM agent_jobs WHERE id=$1`, a.jobID),
		deployment: rows(`SELECT d.* FROM deployments d JOIN agent_jobs j ON j.deployment_id=d.id WHERE j.id=$1`, a.jobID),
		routeHead:  rows(`SELECT * FROM route_heads WHERE environment_id=$1`, a.envID),
		operation:  rows(`SELECT * FROM release_operations WHERE environment_id=$1`, a.envID),
		events:     a.jobEvents(t),
		releases:   rows(`SELECT * FROM releases WHERE environment_id=$1 ORDER BY id`, a.envID),
		audit:      a.auditRows(t),
	}
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM route_events WHERE environment_id=$1`, a.envID).Scan(&s.routeEvents); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_job_events WHERE event_type='reconciled'`).Scan(&s.reconciled); err != nil {
		t.Fatal(err)
	}
	return s
}

func (a activatedFixture) resolve(outcome string) error {
	return a.repo.ResolveActivation(context.Background(), a.principal, a.envID, outcome, resolveReason)
}

// assertRefused calls ResolveActivation and requires wantErr and no write.
func (a activatedFixture) assertRefused(t *testing.T, outcome string, wantErr error) {
	t.Helper()
	before := a.resolveState(t)
	if err := a.resolve(outcome); !errors.Is(err, wantErr) {
		t.Fatalf("ResolveActivation(%q) err=%v, want %v", outcome, err, wantErr)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused ResolveActivation(%q) wrote state:\nbefore %+v\nafter  %+v", outcome, before, after)
	}
}

// blockedPrecondition is what the matrix compares against.
type blockedPrecondition struct {
	revision       int64
	opClosedAt     *time.Time
	opSummary      string
	headStable     string // route head minus flag and updated_at
	headUpdatedAt  time.Time
	releases       string
	activatedEvent int
	lateEvents     string // events recorded between the block and the resolution
}

func (a activatedFixture) blockedPrecondition(t *testing.T) blockedPrecondition {
	t.Helper()
	p := blockedPrecondition{activatedEvent: a.activatedRouteEvents(t)}
	var state string
	p.revision, _, _ = a.routeHead(t)
	state, p.opClosedAt, p.opSummary = a.operation(t)
	if state != "reconciliation_required" || p.opClosedAt == nil {
		t.Fatalf("setup: operation state=%s closedAt=%v", state, p.opClosedAt)
	}
	if _, _, flagged := a.routeHead(t); !flagged {
		t.Fatal("setup: route head must be flagged")
	}
	if _, _, _, flagged := a.slotRow(t); !flagged {
		t.Fatal("setup: slot must be flagged")
	}
	if st, _, _, _, _ := a.attemptRow(t, a.attemptID); st != "leased" {
		t.Fatalf("setup: attempt=%s, want leased", st)
	}
	p.headStable = a.rowJSON(t, `SELECT to_jsonb(h) - 'reconciliation_required' - 'updated_at' AS j FROM route_heads h WHERE environment_id=$1`, a.envID)
	if err := a.pool.QueryRow(context.Background(), `SELECT updated_at FROM route_heads WHERE environment_id=$1`, a.envID).Scan(&p.headUpdatedAt); err != nil {
		t.Fatal(err)
	}
	p.releases = a.rowJSON(t, `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id), '[]'::jsonb) AS rs FROM releases r WHERE environment_id=$1`, a.envID)
	return p
}

// assertResolved asserts the FULL post-state of a declared resolution: every
// row the success / failure path touches, plus the audit row and the exact
// job event list.
func (a activatedFixture) assertResolved(t *testing.T, outcome string, p blockedPrecondition) {
	t.Helper()
	a.assertResolvedAudit(t, outcome, p, true)
}

// assertResolvedAudit is assertResolved with the audit expectation explicit:
// the operator path writes exactly one row, the agent path none.
func (a activatedFixture) assertResolvedAudit(t *testing.T, outcome string, p blockedPrecondition, wantAudit bool) {
	t.Helper()
	state, closedAt, summary := a.operation(t)
	if closedAt == nil || !closedAt.Equal(*p.opClosedAt) {
		t.Fatalf("operation closed_at %v -> %v, want unchanged", p.opClosedAt, closedAt)
	}
	if want := p.opSummary + " | declared " + outcome; summary != want {
		t.Fatalf("operation summary=%q, want %q", summary, want)
	}
	revision, active, flagged := a.routeHead(t)
	if flagged {
		t.Fatal("route head flag not cleared")
	}
	fence, current, recAttempt, slotFlag := a.slotRow(t)
	if fence != 1 || current != nil || recAttempt != nil || slotFlag {
		t.Fatalf("slot fence=%d current=%v recAttempt=%v flag=%v, want 1 free clear", fence, current, recAttempt, slotFlag)
	}
	attemptState, attemptClosedAt, failureClass, resultOp, resultDigest := a.attemptRow(t, a.attemptID)
	if attemptClosedAt == nil || failureClass != nil {
		t.Fatalf("attempt closedAt=%v failureClass=%v", attemptClosedAt, failureClass)
	}
	jobState, _, _, _ := a.jobRow(t)
	events := strings.Join(a.jobEvents(t), ",")
	audit := a.auditRows(t)
	if wantAudit && (len(audit) != 1 || !strings.HasSuffix(audit[0], "|environment_mark_reconciled|success|outcome="+outcome+"; "+resolveReason)) {
		t.Fatalf("audit rows=%v, want exactly one with outcome=%s", audit, outcome)
	}
	if !wantAudit && len(audit) != 0 {
		t.Fatalf("audit rows=%v, want none", audit)
	}
	switch outcome {
	case ActivationOutcomeActivated:
		if state != "succeeded" || revision != p.revision+1 || active == nil || *active != a.candidateID {
			t.Fatalf("operation=%s revision=%d (was %d) active=%v", state, revision, p.revision, active)
		}
		if st, _ := a.releaseState(t, a.candidateID); st != "active" {
			t.Fatalf("candidate=%s, want active", st)
		}
		if a.predecessorID != "" {
			if st, _ := a.releaseState(t, a.predecessorID); st != "retained" {
				t.Fatalf("predecessor=%s, want retained", st)
			}
		}
		if got := a.deploymentStatus(t); got != deployment.StatusHealthy {
			t.Fatalf("deployment=%s, want healthy", got)
		}
		if attemptState != "succeeded" || resultOp == nil || *resultOp != a.operationID || len(resultDigest) != 32 || [32]byte(resultDigest) != a.routeDigest {
			t.Fatalf("attempt=%s resultOp=%v digest ok=%v", attemptState, resultOp, len(resultDigest) == 32)
		}
		if jobState != "succeeded" {
			t.Fatalf("job=%s, want succeeded", jobState)
		}
		if events != "activation_authorized,activation_reconciliation_required,"+p.lateEvents+"activated,succeeded,reconciled" {
			t.Fatalf("events=%s", events)
		}
		if n := a.activatedRouteEvents(t); n != p.activatedEvent+1 {
			t.Fatalf("activated route events=%d, want %d", n, p.activatedEvent+1)
		}
	case ActivationOutcomeNotActivated:
		if state != "failed" || revision != p.revision {
			t.Fatalf("operation=%s revision=%d (was %d)", state, revision, p.revision)
		}
		if got := a.rowJSON(t, `SELECT to_jsonb(h) - 'reconciliation_required' - 'updated_at' AS j FROM route_heads h WHERE environment_id=$1`, a.envID); got != p.headStable {
			t.Fatalf("route head changed beyond flag/updated_at:\nbefore %s\nafter  %s", p.headStable, got)
		}
		var headUpdatedAt time.Time
		if err := a.pool.QueryRow(context.Background(), `SELECT updated_at FROM route_heads WHERE environment_id=$1`, a.envID).Scan(&headUpdatedAt); err != nil || !headUpdatedAt.After(p.headUpdatedAt) {
			t.Fatalf("route head updated_at %v -> %v err=%v, want later", p.headUpdatedAt, headUpdatedAt, err)
		}
		if got := a.rowJSON(t, `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id), '[]'::jsonb) AS rs FROM releases r WHERE environment_id=$1`, a.envID); got != p.releases {
			t.Fatalf("releases changed:\nbefore %s\nafter  %s", p.releases, got)
		}
		if got := a.deploymentStatus(t); got != deployment.StatusFailed {
			t.Fatalf("deployment=%s, want failed", got)
		}
		if attemptState != "expired" || resultOp != nil || resultDigest != nil {
			t.Fatalf("attempt=%s resultOp=%v digest=%v, want expired with no result", attemptState, resultOp, resultDigest)
		}
		if jobState != "failed" {
			t.Fatalf("job=%s, want failed", jobState)
		}
		if events != "activation_authorized,activation_reconciliation_required,"+p.lateEvents+"activation_not_activated,failed,reconciled" {
			t.Fatalf("events=%s", events)
		}
		if n := a.activatedRouteEvents(t); n != p.activatedEvent {
			t.Fatalf("activated route events=%d, want %d", n, p.activatedEvent)
		}
	}
}

func TestResolveActivationPostStateMatrix(t *testing.T) {
	seed := byte(20)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		for _, cause := range blockCauses {
			for _, withPredecessor := range []bool{true, false} {
				seed += 2
				name := fmt.Sprintf("%s/%s/pred%t", outcome, cause.name, withPredecessor)
				t.Run(name, func(t *testing.T) {
					a := newActivatedFixture(t, seed, withPredecessor)
					cause.block(t, a)
					p := a.blockedPrecondition(t)
					if err := a.resolve(outcome); err != nil {
						t.Fatalf("ResolveActivation: %v", err)
					}
					a.assertResolved(t, outcome, p)
				})
			}
		}
	}
}

func TestResolveActivationOutcomeValidation(t *testing.T) {
	a := newActivatedFixture(t, 120, true)
	a.blockViaTx2Failure(t)
	for _, outcome := range []string{"", "Activated", "activate", "succeeded", "not activated"} {
		before := a.resolveState(t)
		if err := a.resolve(outcome); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("outcome %q err=%v, want ErrAgentLeaseInvalidInput", outcome, err)
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("outcome %q wrote state", outcome)
		}
	}
	// validReason applies to the COMBINED string: a reason that fits alone but
	// not after the prefix is refused, nothing written.
	long := strings.Repeat("r", 512-len("outcome=activated; ")+1)
	before := a.resolveState(t)
	if err := a.repo.ResolveActivation(context.Background(), a.principal, a.envID, ActivationOutcomeActivated, long); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("over-long combined reason err=%v", err)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("over-long reason wrote state")
	}
	exact := strings.Repeat("r", 512-len("outcome=not_activated; "))
	if err := a.repo.ResolveActivation(context.Background(), a.principal, a.envID, ActivationOutcomeNotActivated, exact); err != nil {
		t.Fatalf("exactly 512-byte combined reason err=%v", err)
	}
}

// A plain (non-activation) block: an ordinary expiry close with no activation.
func plainBlock(t *testing.T, seed byte) activatedFixture {
	t.Helper()
	f := newLeaseFixture(t, seed)
	f.insertRouteHead(t, "app.example.com")
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	a := activatedFixture{leaseFixture: f, attemptID: attemptID, repo: repo}
	a.lapse(t, "2 seconds")
	if closed, err := repo.expireOne(context.Background(), attemptID); err != nil || !closed {
		t.Fatalf("expireOne closed=%v err=%v", closed, err)
	}
	if _, _, _, flagged := f.slotRow(t); !flagged {
		t.Fatal("setup: ordinary close must flag the slot")
	}
	return a
}

func TestResolveActivationNoActivationMatchesMarkReconciled(t *testing.T) {
	ctx := context.Background()
	// Fixtures cannot coexist (each resets the tables), so the two paths run
	// one after the other on identical shapes and their effects are compared.
	type effect struct {
		fence   int64
		current *int64
		rec     *int64
		flagged bool
		events  string
	}
	observe := func(a activatedFixture) effect {
		fence, current, rec, flagged := a.slotRow(t)
		return effect{fence, current, rec, flagged, strings.Join(a.jobEvents(t), ",")}
	}
	a := plainBlock(t, 130)
	if err := a.repo.MarkReconciled(ctx, a.principal, a.envID, "operator break-glass"); err != nil {
		t.Fatal(err)
	}
	want := observe(a)
	b := plainBlock(t, 131)
	if err := b.repo.ResolveActivation(ctx, b.principal, b.envID, ActivationOutcomeNoActivation, "operator break-glass"); err != nil {
		t.Fatalf("ResolveActivation(no_activation) on a plain block: %v", err)
	}
	if got := observe(b); got != want || got.flagged || got.current != nil || got.rec != nil || !strings.HasSuffix(got.events, ",reconciled") {
		t.Fatalf("effect %+v, want %+v", got, want)
	}
	audit := b.auditRows(t)
	if len(audit) != 1 || !strings.HasSuffix(audit[0], "|environment_mark_reconciled|success|outcome=no_activation; operator break-glass") {
		t.Fatalf("audit=%v", audit)
	}
}

func TestResolveActivationRefusals(t *testing.T) {
	t.Run("plain block rejects activation outcomes", func(t *testing.T) {
		a := plainBlock(t, 140)
		a.assertRefused(t, ActivationOutcomeActivated, ErrAgentLeaseOutcomeMismatch)
		a.assertRefused(t, ActivationOutcomeNotActivated, ErrAgentLeaseOutcomeMismatch)
	})
	t.Run("activation block rejects no_activation", func(t *testing.T) {
		a := newActivatedFixture(t, 142, true)
		a.blockViaTx2Failure(t)
		a.assertRefused(t, ActivationOutcomeNoActivation, ErrAgentLeaseOutcomeMismatch)
	})
	t.Run("switch_authorized is not resolvable", func(t *testing.T) {
		a := newActivatedFixture(t, 144, true)
		// Forced in SQL: the real code never flags a slot over a
		// switch_authorized operation, so the shape is built by hand.
		if _, err := a.pool.Exec(context.Background(), `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, a.attemptID); err != nil {
			t.Fatal(err)
		}
		a.assertRefused(t, ActivationOutcomeActivated, ErrAgentLeaseActivationUnresolved)
		a.assertRefused(t, ActivationOutcomeNotActivated, ErrAgentLeaseActivationUnresolved)
		a.assertRefused(t, ActivationOutcomeNoActivation, ErrAgentLeaseOutcomeMismatch)
	})
	t.Run("wrong owner", func(t *testing.T) {
		a := newActivatedFixture(t, 146, true)
		a.blockViaTx2Failure(t)
		other := secondOwnerPrincipal(t, a.pool, NewAuthRepository(a.pool), 147)
		before := a.resolveState(t)
		for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated, ActivationOutcomeNoActivation} {
			if err := a.repo.ResolveActivation(context.Background(), other, a.envID, outcome, resolveReason); !errors.Is(err, ErrServerAuthorizationDenied) {
				t.Fatalf("foreign owner %s err=%v, want denied", outcome, err)
			}
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatal("foreign owner wrote state")
		}
	})
	t.Run("unknown environment", func(t *testing.T) {
		a := newActivatedFixture(t, 148, true)
		a.blockViaTx2Failure(t)
		if err := a.repo.ResolveActivation(context.Background(), a.principal, "no-such-env", ActivationOutcomeActivated, resolveReason); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("foreign slot flag", func(t *testing.T) {
		a := newActivatedFixture(t, 150, true)
		a.blockViaTx2Failure(t)
		other := a.insertOtherAttempt(t)
		// Forced in SQL: the slot flag names an attempt other than the one the
		// unresolved operation belongs to (reachable only through corruption).
		if _, err := a.pool.Exec(context.Background(), `UPDATE environment_slots SET reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID, other); err != nil {
			t.Fatal(err)
		}
		for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated, ActivationOutcomeNoActivation} {
			a.assertRefused(t, outcome, ErrAgentLeaseCorruptState)
		}
	})
	t.Run("activated with the route moved", func(t *testing.T) {
		a := newActivatedFixture(t, 152, false)
		a.blockViaTx2Failure(t)
		// Forced in SQL: AssignRouteHostname can move the head while blocked
		// (only while no release is active, hence no predecessor here).
		if _, err := a.pool.Exec(context.Background(), `UPDATE route_heads SET hostname='moved.example.com', route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID); err != nil {
			t.Fatal(err)
		}
		a.assertRefused(t, ActivationOutcomeActivated, ErrAgentLeaseActivationRouteMoved)
		// not_activated is still available and leaves the moved head alone.
		if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
			t.Fatalf("not_activated after a moved route: %v", err)
		}
		if rev, _, flagged := a.routeHead(t); flagged || rev != 1 {
			t.Fatalf("head revision=%d flagged=%v, want 1 false", rev, flagged)
		}
	})
}

// Each pre-verify guard on its own: the block is real, then exactly one
// precondition is broken in SQL.
func TestResolveActivationPreVerifyGuards(t *testing.T) {
	for i, tc := range []struct {
		name    string
		breakIt func(*testing.T, activatedFixture)
	}{
		{"attempt closed", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, a.attemptID)
		}},
		{"job not leased", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `UPDATE agent_jobs SET state='failed', updated_at=clock_timestamp() WHERE id=$1`, a.jobID)
		}},
		{"slot not held", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID)
		}},
		{"deployment not activating", func(t *testing.T, a activatedFixture) { a.setDeploymentStatus(t, deployment.StatusFailed) }},
		// Attempt identity is immutable by trigger; the trigger is disabled for
		// the one statement to build a fence / environment the operation does
		// not match (reachable only through corruption).
		{"attempt fence mismatch", func(t *testing.T, a activatedFixture) {
			withTriggerDisabled(t, a, "agent_job_attempts", "agent_job_attempts_validate_update", func() {
				a.mustExec(t, `UPDATE agent_job_attempts SET fence=fence+5 WHERE id=$1`, a.attemptID)
			})
		}},
		{"attempt environment mismatch", func(t *testing.T, a activatedFixture) {
			a.mustExec(t, `INSERT INTO environment_slots(environment_id) VALUES ('other-env')`)
			withTriggerDisabled(t, a, "agent_job_attempts", "agent_job_attempts_validate_update", func() {
				a.mustExec(t, `UPDATE agent_job_attempts SET environment_id='other-env' WHERE id=$1`, a.attemptID)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newActivatedFixture(t, byte(160+2*i), true)
			a.blockViaTx2Failure(t)
			tc.breakIt(t, a)
			// The refusal must come from the pre-verify, BEFORE any write: a stage
			// hook firing means a write ran and a later conditional write (or
			// nothing) caught the broken precondition instead.
			var stages []string
			a.repo.failAfter = func(s string) error { stages = append(stages, s); return nil }
			defer func() { a.repo.failAfter = nil }()
			a.assertRefused(t, ActivationOutcomeActivated, ErrAgentLeaseCorruptState)
			a.assertRefused(t, ActivationOutcomeNotActivated, ErrAgentLeaseCorruptState)
			if len(stages) != 0 {
				t.Fatalf("writes ran before the refusal: stages %v", stages)
			}
		})
	}
}

// secondAdministrator adds another user and live session under the SAME owner.
func (a activatedFixture) secondAdministrator(t *testing.T, seed byte) auth.Principal {
	t.Helper()
	ctx := context.Background()
	userID := "user-c-" + releaseSuffix(t)
	a.mustExec(t, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES($1,$2,$3,'owner',$4,1)`, userID, a.ownerID, "second-admin-"+strings.ToLower(fmt.Sprint(seed)), strings.Repeat("x", 68))
	token := testToken(t, seed)
	user := LoginUser{ID: userID, OwnerID: a.ownerID, AuthRevision: 1, Active: true}
	if _, err := NewAuthRepository(a.pool).CreateSession(ctx, "session-c-"+releaseSuffix(t), token, user, nil, "login-c-"+releaseSuffix(t), "limit-c-"+releaseSuffix(t)); err != nil {
		t.Fatal(err)
	}
	p, err := NewAuthRepository(a.pool).ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (a activatedFixture) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := a.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Zero rows affected by any conditional write of not_activated is corruption
// and rolls everything back. The suppression is a REAL BEFORE UPDATE trigger
// returning NULL (the update silently affects no row); its name sorts before
// the table's own validate trigger so it fires first.
func TestResolveActivationZeroRowWriteRefuses(t *testing.T) {
	for i, tc := range []struct{ table, when string }{
		{"route_heads", "NOT NEW.reconciliation_required"},
		{"release_operations", "NEW.state = 'failed'"},
		{"agent_job_attempts", "NEW.state = 'expired'"},
		{"environment_slots", "NEW.current_attempt_id IS NULL"},
		{"agent_jobs", "NEW.state = 'failed'"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			a := newActivatedFixture(t, byte(180+2*i), true)
			a.blockViaTx2Failure(t)
			fn := "a_suppress_" + tc.table
			a.mustExec(t, `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
			t.Cleanup(func() {
				_, _ = a.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON `+tc.table)
				_, _ = a.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
			})
			a.mustExec(t, `CREATE TRIGGER `+fn+` BEFORE UPDATE ON `+tc.table+` FOR EACH ROW WHEN (`+tc.when+`) EXECUTE FUNCTION `+fn+`()`)
			a.assertRefused(t, ActivationOutcomeNotActivated, ErrAgentLeaseCorruptState)
		})
	}
}

// A failure at ANY stage rolls back everything (including the audit row and
// the reconciled event), and writes no new block.
func TestResolveActivationRollsBackAtEveryStage(t *testing.T) {
	injected := errors.New("injected")
	stages := map[string][]string{
		ActivationOutcomeNotActivated: {"deployment", "route_head_flag", "operation_close", "attempt_close", "slot_free", "job_close", "audit_insert", "job_event"},
		ActivationOutcomeActivated:    {"deployment", "release_active", "route_head", "operation_close", "attempt_close", "slot_free", "job_close", "job_event", "audit_insert"},
	}
	seed := byte(200)
	for outcome, list := range stages {
		for _, stage := range list {
			seed += 2
			t.Run(outcome+"/"+stage, func(t *testing.T) {
				a := newActivatedFixture(t, seed, true)
				a.blockViaTx2Failure(t)
				before := a.resolveState(t)
				a.repo.failAfter = func(s string) error {
					if s == stage {
						return injected
					}
					return nil
				}
				if err := a.resolve(outcome); !errors.Is(err, injected) {
					t.Fatalf("err=%v, want injected failure at %s", err, stage)
				}
				a.repo.failAfter = nil
				if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("failure at %s left state behind:\nbefore %+v\nafter  %+v", stage, before, after)
				}
				// The environment stays blocked and resolvable.
				if err := a.resolve(outcome); err != nil {
					t.Fatalf("retry after rollback: %v", err)
				}
			})
		}
	}
}

func TestResolveActivationIdempotentRepeat(t *testing.T) {
	seed := byte(30)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		seed += 2
		t.Run(outcome, func(t *testing.T) {
			a := newActivatedFixture(t, seed, true)
			a.blockViaSweeperDiversion(t)
			if err := a.resolve(outcome); err != nil {
				t.Fatal(err)
			}
			before := a.resolveState(t)
			if err := a.resolve(outcome); err != nil {
				t.Fatalf("same-outcome repeat err=%v, want nil", err)
			}
			if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
				t.Fatal("same-outcome repeat wrote state")
			}
			for _, other := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated, ActivationOutcomeNoActivation} {
				if other == outcome {
					continue
				}
				if err := a.resolve(other); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
					t.Fatalf("repeat with %s after %s err=%v, want integrity conflict", other, outcome, err)
				}
				if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("conflicting repeat %s wrote state", other)
				}
			}
			// A foreign owner learns nothing from a repeat.
			stranger := secondOwnerPrincipal(t, a.pool, NewAuthRepository(a.pool), seed+1)
			if err := a.repo.ResolveActivation(context.Background(), stranger, a.envID, outcome, resolveReason); !errors.Is(err, ErrServerAuthorizationDenied) {
				t.Fatalf("foreign repeat err=%v", err)
			}
		})
	}
	t.Run("no_activation", func(t *testing.T) {
		a := plainBlock(t, 60)
		ctx := context.Background()
		if err := a.repo.ResolveActivation(ctx, a.principal, a.envID, ActivationOutcomeNoActivation, resolveReason); err != nil {
			t.Fatal(err)
		}
		before := a.resolveState(t)
		if err := a.repo.ResolveActivation(ctx, a.principal, a.envID, ActivationOutcomeNoActivation, resolveReason); err != nil {
			t.Fatalf("repeat err=%v", err)
		}
		for _, other := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
			if err := a.repo.ResolveActivation(ctx, a.principal, a.envID, other, resolveReason); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
				t.Fatalf("repeat with %s err=%v, want integrity conflict", other, err)
			}
		}
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatal("repeat wrote state")
		}
	})
	t.Run("never blocked", func(t *testing.T) {
		a := newActivatedFixture(t, 64, true)
		if err := a.resolve(ActivationOutcomeNoActivation); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("nothing pending err=%v, want today's denial", err)
		}
	})
}

// The summary keeps the whole evidence; when the marker does not fit it is
// left out rather than cutting anything.
func TestDeclaredSummary(t *testing.T) {
	if got := declaredSummary("block", "activated"); got != "block | declared activated" {
		t.Fatalf("got %q", got)
	}
	if got := declaredSummary("", "not_activated"); got != "declared not_activated" {
		t.Fatalf("got %q", got)
	}
	full := strings.Repeat("é", 1024)
	if got := declaredSummary(full, "activated"); got != full {
		t.Fatal("a full summary must be returned unchanged")
	}
	marker := " | declared activated"
	fits := strings.Repeat("a", maxOutcomeSummaryBytes-len(marker))
	if got := declaredSummary(fits, "activated"); len(got) != maxOutcomeSummaryBytes || !strings.HasSuffix(got, marker) {
		t.Fatalf("exact fit not appended: len=%d", len(got))
	}
	if got := declaredSummary(fits+"a", "activated"); got != fits+"a" {
		t.Fatal("one byte over must leave the summary unchanged")
	}
}

// With the 2048-byte summary full the resolution still succeeds, the evidence
// is untouched, and the audit row carries the declaration.
func TestResolveActivationFullSummaryKeepsEvidence(t *testing.T) {
	a := newActivatedFixture(t, 66, true)
	a.blockViaTx2Failure(t)
	// Forced in SQL: fill the summary to the column cap.
	full := strings.Repeat("e", maxOutcomeSummaryBytes)
	a.mustExec(t, `UPDATE release_operations SET outcome_summary=$2 WHERE id=$1`, mustParseInt(t, a.operationID), full)
	if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
		t.Fatal(err)
	}
	if state, _, summary := a.operation(t); state != "failed" || summary != full {
		t.Fatalf("state=%s summary changed=%v", state, summary != full)
	}
	if audit := a.auditRows(t); len(audit) != 1 || !strings.Contains(audit[0], "outcome=not_activated") {
		t.Fatalf("audit=%v", audit)
	}
}

// End to end: the wedge is gone. After a real block and a declared
// resolution a NEW job can be claimed and activated in the environment.
func TestResolveActivationUnwedgesEnvironment(t *testing.T) {
	seed := byte(70)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		for _, cause := range blockCauses[:3] { // revoke: a revoked server cannot claim
			seed += 2
			t.Run(outcome+"/"+cause.name, func(t *testing.T) {
				ctx := context.Background()
				a := newActivatedFixture(t, seed, true)
				cause.block(t, a)
				// Before the resolution a new job cannot even be claimed.
				next := a.insertJob(t, a.serverID, a.envID, "next-"+releaseSuffix(t), 0)
				if leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5); err != nil || len(leases) != 0 {
					t.Fatalf("claim while blocked: leases=%d err=%v, want none", len(leases), err)
				}
				if err := a.resolve(outcome); err != nil {
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
}

// assertNewAttemptActivates drives a freshly claimed lease through the
// execution states and Activate: after a resolution the wedge is gone.
func (a activatedFixture) assertNewAttemptActivates(t *testing.T, outcome string, next int64, lease queue.ClaimedLease) {
	t.Helper()
	ctx := context.Background()
	f2 := a.leaseFixture
	f2.jobID = next
	depRepo := NewDeploymentRepository(a.pool)
	for _, target := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking} {
		dep, err := depRepo.GetByID(ctx, deployment.ID(lease.DeploymentID))
		if err != nil {
			t.Fatal(err)
		}
		var now time.Time
		if err := a.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		nextDep, err := dep.Transition(target, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := depRepo.Save(ctx, nextDep); err != nil {
			t.Fatal(err)
		}
	}
	result, err := a.repo.Activate(ctx, a.serverID, a.cred, lease.AttemptID, lease.Fence, digest(77))
	if err != nil {
		t.Fatalf("Activate after resolution: %v", err)
	}
	wantRevision := int64(1)
	if outcome == ActivationOutcomeActivated {
		wantRevision = 2
	}
	if state, _, expectedRev, _ := f2.activationOperationRow(t, lease.AttemptID); state != "switch_authorized" || expectedRev == nil || *expectedRev != wantRevision {
		t.Fatalf("new operation state=%s expectedRevision=%v, want switch_authorized %d", state, expectedRev, wantRevision)
	}
	if result.OperationID == "" {
		t.Fatal("no operation id")
	}
}

// --- concurrency (run with -race) ---

func (a activatedFixture) holdSlot(t *testing.T, ctx context.Context) func() {
	t.Helper()
	holder, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, a.envID); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// Two resolutions race from two administrators of the same owner, both queued
// on the slot row (two waiters confirmed via pg_stat_activity). Exactly one resolves; the
// other is the idempotent answer or the integrity conflict, never corruption.
func TestResolveActivationTwoResolutionsRace(t *testing.T) {
	cases := []struct {
		first, second string
		secondErr     error
	}{
		{ActivationOutcomeActivated, ActivationOutcomeActivated, nil},
		{ActivationOutcomeNotActivated, ActivationOutcomeNotActivated, nil},
		{ActivationOutcomeActivated, ActivationOutcomeNotActivated, ErrAgentLeaseIntegrityConflict},
		{ActivationOutcomeNotActivated, ActivationOutcomeActivated, ErrAgentLeaseIntegrityConflict},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s then %s", tc.first, tc.second), func(t *testing.T) {
			a := newActivatedFixture(t, byte(100+2*i), true)
			a.blockViaTx2Failure(t)
			p := a.blockedPrecondition(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			release := a.holdSlot(t, ctx)
			errs := make([]error, 2)
			var wg sync.WaitGroup
			// A different administrator of the same owner, so the two sessions do
			// not serialize on one session/user row and BOTH queue on the slot.
			second := a.secondAdministrator(t, byte(101+2*i))
			start := func(i int, who auth.Principal, outcome string) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = a.repo.ResolveActivation(ctx, who, a.envID, outcome, resolveReason)
				}()
			}
			start(0, a.principal, tc.first)
			waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 1)
			start(1, second, tc.second)
			waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 2)
			release()
			wg.Wait()
			if errs[0] != nil {
				t.Fatalf("first resolution err=%v", errs[0])
			}
			if !errors.Is(errs[1], tc.secondErr) && !(tc.secondErr == nil && errs[1] == nil) {
				t.Fatalf("second resolution err=%v, want %v", errs[1], tc.secondErr)
			}
			a.assertResolved(t, tc.first, p)
		})
	}
}

// A resolution and a late tx2 report queue on the SAME slot row (two backends
// waiting, confirmed). Either order must leave a consistent terminal state:
// the report is recorded as evidence before the resolution (and so appears in
// the preserved summary) or is refused after it. Start order decides queue
// order, so both orders are driven explicitly.
func TestResolveActivationRacesLateReport(t *testing.T) {
	seed := byte(110)
	for _, outcome := range []string{ActivationOutcomeActivated, ActivationOutcomeNotActivated} {
		for _, reportFirst := range []bool{true, false} {
			seed += 2
			t.Run(fmt.Sprintf("%s/reportFirst%t", outcome, reportFirst), func(t *testing.T) {
				a := newActivatedFixture(t, seed, true)
				a.blockViaTx2Failure(t)
				p := a.blockedPrecondition(t)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				release := a.holdSlot(t, ctx)
				var resolveErr, reportErr error
				var wg sync.WaitGroup
				startResolve := func() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						resolveErr = a.repo.ResolveActivation(ctx, a.principal, a.envID, outcome, resolveReason)
					}()
				}
				startReport := func() {
					wg.Add(1)
					go func() { defer wg.Done(); reportErr = a.lateReport(ctx, "success wrong digest") }()
				}
				if reportFirst {
					startReport()
					waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 1)
					startResolve()
					waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 2)
				} else {
					startResolve()
					waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 1)
					startReport()
					waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 2)
				}
				release()
				wg.Wait()
				if resolveErr != nil {
					t.Fatalf("resolution err=%v", resolveErr)
				}
				_, _, summary := a.operation(t)
				if reportFirst {
					if !errors.Is(reportErr, ErrAgentLeaseActivationLateReportRecorded) {
						t.Fatalf("report err=%v, want recorded as evidence", reportErr)
					}
					// The evidence the report added precedes the marker and survives.
					if !strings.Contains(summary, "late report #") || !strings.HasSuffix(summary, " | declared "+outcome) {
						t.Fatalf("summary=%q", summary)
					}
					p.opSummary = strings.TrimSuffix(summary, " | declared "+outcome)
					p.lateEvents = "activation_late_report,"
				} else if !errors.Is(reportErr, ErrAgentLeaseIntegrityConflict) && !errors.Is(reportErr, ErrAgentLeaseStale) {
					// After the resolution the operation is terminal: a mismatching
					// report is answered, never recorded.
					t.Fatalf("report after resolution err=%v", reportErr)
				}
				a.assertResolved(t, outcome, p)
			})
		}
	}
}

// Resolution vs the sweeper's per-candidate transaction (expireOne).
func TestResolveActivationRacesExpireOne(t *testing.T) {
	t.Run("sweeper first", func(t *testing.T) {
		// expireOne holds slot/job/attempt/operation locks (parked at its
		// operation_lock hook) over a switch_authorized operation; the
		// resolution queues on the slot, and after the sweeper commits its
		// block the resolution sees reconciliation_required and resolves.
		a := newActivatedFixture(t, 130, true)
		a.lapse(t, "2 seconds")
		reached, proceed := make(chan struct{}), make(chan struct{})
		var unpark sync.Once
		t.Cleanup(func() { unpark.Do(func() { close(proceed) }) }) // a failed assertion must not leave the parked transaction holding locks
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
		var sweepErr, resolveErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _, sweepErr = sweeper.expireOne(ctx, a.attemptID) }()
		<-reached
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolveErr = a.repo.ResolveActivation(ctx, a.principal, a.envID, ActivationOutcomeNotActivated, resolveReason)
		}()
		waitForLockWaiters(t, ctx, a.pool, "FROM environment_slots%FOR UPDATE", 1)
		unpark.Do(func() { close(proceed) })
		wg.Wait()
		if sweepErr != nil || resolveErr != nil {
			t.Fatalf("sweep err=%v resolve err=%v", sweepErr, resolveErr)
		}
		if state, _, summary := a.operation(t); state != "failed" || !strings.HasPrefix(summary, activationBlockedByAuthorityEndSummary) || !strings.HasSuffix(summary, " | declared not_activated") {
			t.Fatalf("operation=%s summary=%q", state, summary)
		}
		if got := strings.Join(a.jobEvents(t), ","); got != "activation_authorized,activation_reconciliation_required,activation_not_activated,failed,reconciled" {
			t.Fatalf("events=%s", got)
		}
	})
	t.Run("resolution first", func(t *testing.T) {
		// The resolution holds every lock (parked at audit_insert, after its
		// writes); the sweeper's SKIP LOCKED slot lock finds the slot taken and
		// skips. After the commit the attempt is closed and a second sweep is a
		// no-op as well.
		a := newActivatedFixture(t, 132, true)
		a.blockViaTx2Failure(t)
		a.lapse(t, "2 seconds")
		reached, proceed := make(chan struct{}), make(chan struct{})
		var unpark sync.Once
		t.Cleanup(func() { unpark.Do(func() { close(proceed) }) })
		resolver := NewAgentLeaseRepository(a.pool, NewDeploymentRepository(a.pool))
		resolver.failAfter = func(stage string) error {
			if stage == "audit_insert" {
				close(reached)
				<-proceed
			}
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var resolveErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolveErr = resolver.ResolveActivation(ctx, a.principal, a.envID, ActivationOutcomeActivated, resolveReason)
		}()
		<-reached
		closed, err := a.repo.expireOne(ctx, a.attemptID)
		if err != nil || closed {
			t.Fatalf("expireOne during resolution closed=%v err=%v, want a skipped no-op", closed, err)
		}
		unpark.Do(func() { close(proceed) })
		wg.Wait()
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if closed, err = a.repo.expireOne(ctx, a.attemptID); err != nil || closed {
			t.Fatalf("expireOne after resolution closed=%v err=%v", closed, err)
		}
		if got := strings.Join(a.jobEvents(t), ","); got != "activation_authorized,activation_reconciliation_required,activated,succeeded,reconciled" {
			t.Fatalf("events=%s", got)
		}
	})
}

// A block can leave the route head unflagged (the flag is set only if a head
// existed and was unflagged at block time, and may be cleared by hand).
// not_activated then touches the head not at all: the clear is conditional.
func TestResolveActivationNotActivatedLeavesUnflaggedHeadUntouched(t *testing.T) {
	a := newActivatedFixture(t, 134, true)
	a.blockViaTx2Failure(t)
	// Forced in SQL: clear the head flag only.
	a.mustExec(t, `UPDATE route_heads SET reconciliation_required=false, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID)
	headBefore := a.rowJSON(t, `SELECT * FROM route_heads WHERE environment_id=$1`, a.envID)
	if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
		t.Fatal(err)
	}
	if got := a.rowJSON(t, `SELECT * FROM route_heads WHERE environment_id=$1`, a.envID); got != headBefore {
		t.Fatalf("an unflagged head was written:\nbefore %s\nafter  %s", headBefore, got)
	}
	if state, _, _ := a.operation(t); state != "failed" {
		t.Fatalf("operation=%s", state)
	}
}

// The clock is read AFTER every row lock is held: the resolution queues
// behind a third transaction holding ONE row (the slot, the route head, or the
// candidate release) and every updated_at it writes must be later than the
// instant that transaction let go. Holding a row taken after the slot lock
// distinguishes "clock read after all locks" from "clock read right after the
// slot lock".
func TestResolveActivationReadsClockAfterLocks(t *testing.T) {
	seed := byte(136)
	for _, tc := range []struct {
		name, outcome, hold, waiter string
	}{
		{"slot/activated", ActivationOutcomeActivated, "slot", "FROM environment_slots%FOR UPDATE"},
		{"slot/not_activated", ActivationOutcomeNotActivated, "slot", "FROM environment_slots%FOR UPDATE"},
		{"head/activated", ActivationOutcomeActivated, "head", "FROM route_heads%FOR UPDATE"},
		{"head/not_activated", ActivationOutcomeNotActivated, "head", "FROM route_heads%FOR UPDATE"},
		{"release/activated", ActivationOutcomeActivated, "release", "FROM releases%FOR UPDATE"},
	} {
		seed += 2
		t.Run(tc.name, func(t *testing.T) {
			a := newActivatedFixture(t, seed, true)
			a.blockViaTx2Failure(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			holdSQL, arg := `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, any(a.envID)
			switch tc.hold {
			case "head":
				holdSQL = `SELECT 1 FROM route_heads WHERE environment_id=$1 FOR UPDATE`
			case "release":
				holdSQL, arg = `SELECT 1 FROM releases WHERE id=$1 FOR UPDATE`, any(a.candidateID)
			}
			holder, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
			if _, err := holder.Exec(ctx, holdSQL, arg); err != nil {
				t.Fatal(err)
			}
			var resolveErr error
			done := make(chan struct{})
			go func() { defer close(done); resolveErr = a.resolve(tc.outcome) }()
			waitForLockWaiters(t, ctx, a.pool, tc.waiter, 1)
			var released time.Time
			if err := a.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&released); err != nil {
				t.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			<-done
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			for table, query := range map[string]string{
				"slot": `SELECT updated_at FROM environment_slots WHERE environment_id=$1`,
				"job":  `SELECT updated_at FROM agent_jobs WHERE id=$1`,
			} {
				arg := any(a.envID)
				if table == "job" {
					arg = a.jobID
				}
				var at time.Time
				if err := a.pool.QueryRow(ctx, query, arg).Scan(&at); err != nil {
					t.Fatal(err)
				}
				if !at.After(released) {
					t.Fatalf("%s updated_at %v is not after the lock release %v: the clock was read before all locks", table, at, released)
				}
			}
		})
	}
}

// Rollback at every stage of the no_activation path, and a retry succeeds.
func TestResolveActivationNoActivationRollsBackAtEveryStage(t *testing.T) {
	injected := errors.New("injected")
	for i, stage := range []string{"slot_reconcile_clear", "audit_insert", "job_event"} {
		t.Run(stage, func(t *testing.T) {
			a := plainBlock(t, byte(40+2*i))
			before := a.resolveState(t)
			a.repo.failAfter = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			if err := a.resolve(ActivationOutcomeNoActivation); !errors.Is(err, injected) {
				t.Fatalf("err=%v, want injected failure at %s", err, stage)
			}
			a.repo.failAfter = nil
			if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("failure at %s left state behind", stage)
			}
			if err := a.resolve(ActivationOutcomeNoActivation); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
		})
	}
}

// A block can exist with no route head row at all (the block routine tolerates
// it). not_activated needs no head; activated cannot finalize without one and
// refuses, writing nothing.
func TestResolveActivationWithNoRouteHead(t *testing.T) {
	setup := func(t *testing.T, seed byte) activatedFixture {
		a := newActivatedFixture(t, seed, false)
		a.blockViaTx2Failure(t)
		// Forced in SQL: route_heads has a no-delete trigger; disabled for the
		// one DELETE (and the head's audit/event children are not present).
		withTriggerDisabled(t, a, "route_heads", "route_heads_no_delete", func() {
			a.mustExec(t, `DELETE FROM route_heads WHERE environment_id=$1`, a.envID)
		})
		return a
	}
	t.Run("not_activated", func(t *testing.T) {
		a := setup(t, 44)
		if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := a.operation(t); state != "failed" {
			t.Fatalf("operation=%s", state)
		}
		if got := a.deploymentStatus(t); got != deployment.StatusFailed {
			t.Fatalf("deployment=%s", got)
		}
		if _, current, rec, flagged := a.slotRow(t); current != nil || rec != nil || flagged {
			t.Fatal("slot not freed and clear")
		}
	})
	t.Run("activated", func(t *testing.T) {
		a := setup(t, 46)
		a.assertRefused(t, ActivationOutcomeActivated, ErrAgentLeaseCorruptState)
	})
}

// A pending attempt that is still the leased occupant with NO unresolved
// operation is corrupt: clearing the flag would leave a leased attempt holding
// the slot (the invisible wedge). Nothing is written.
func TestResolveActivationNoActivationRefusesLeasedAttempt(t *testing.T) {
	f := newLeaseFixture(t, 48)
	f.insertRouteHead(t, "app.example.com")
	attemptID := f.leaseJob(t, 1, 90)
	a := activatedFixture{leaseFixture: f, attemptID: attemptID, repo: NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))}
	// Forced in SQL: flag the slot over a still-leased occupant; no real path
	// produces this.
	a.mustExec(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, attemptID)
	a.assertRefused(t, ActivationOutcomeNoActivation, ErrAgentLeaseCorruptState)
}

// The replay is anchored to the environment's most recent attempt.
func TestResolveActivationReplayIsAnchoredToLatestAttempt(t *testing.T) {
	ctx := context.Background()
	t.Run("a newer attempt exists", func(t *testing.T) {
		a := newActivatedFixture(t, 50, true)
		a.blockViaSweeperDiversion(t)
		if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
			t.Fatal(err)
		}
		// Attempt B is claimed after A's resolution.
		a.insertJob(t, a.serverID, a.envID, "attempt-b", 0)
		if leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5); err != nil || len(leases) != 1 {
			t.Fatalf("claim B: leases=%d err=%v", len(leases), err)
		}
		// A duplicate of A's resolution is not "nothing changed": the latest
		// attempt is B, which no resolution touched.
		for _, outcome := range []string{ActivationOutcomeNotActivated, ActivationOutcomeActivated, ActivationOutcomeNoActivation} {
			a.assertRefused(t, outcome, ErrServerAuthorizationDenied)
		}
	})
	t.Run("healthy environment with an old resolution", func(t *testing.T) {
		a := newActivatedFixture(t, 52, true)
		a.blockViaSweeperDiversion(t)
		if err := a.resolve(ActivationOutcomeNotActivated); err != nil {
			t.Fatal(err)
		}
		a.insertJob(t, a.serverID, a.envID, "attempt-b", 0)
		leases, err := a.repo.Claim(ctx, a.serverID, a.cred, 5)
		if err != nil || len(leases) != 1 {
			t.Fatalf("claim B: leases=%d err=%v", len(leases), err)
		}
		// Forced in SQL: B finishes normally (a real Complete needs the whole
		// execution protocol); the slot is free and clear, nothing is pending.
		a.mustExec(t, `UPDATE agent_job_attempts SET state='succeeded', closed_at=clock_timestamp(), result_operation_id='x', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, leases[0].AttemptID)
		a.mustExec(t, `UPDATE environment_slots SET current_attempt_id=NULL, updated_at=clock_timestamp() WHERE environment_id=$1`, a.envID)
		a.mustExec(t, `UPDATE agent_jobs SET state='succeeded', updated_at=clock_timestamp() WHERE id=$1`, leases[0].JobID)
		a.assertRefused(t, ActivationOutcomeNotActivated, ErrServerAuthorizationDenied)
	})
	t.Run("duplicate of the latest resolution", func(t *testing.T) {
		a := newActivatedFixture(t, 54, true)
		a.blockViaSweeperDiversion(t)
		if err := a.resolve(ActivationOutcomeActivated); err != nil {
			t.Fatal(err)
		}
		before := a.resolveState(t)
		if err := a.resolve(ActivationOutcomeActivated); err != nil {
			t.Fatalf("duplicate err=%v", err)
		}
		a.assertRefused(t, ActivationOutcomeNotActivated, ErrAgentLeaseIntegrityConflict)
		if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
			t.Fatal("duplicate wrote state")
		}
	})
}
