//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/runtime/dockerid"
)

// drainFixture is an activated-and-finalized successor (attempt succeeded)
// with a real predecessor release whose runtime instance is derived from its
// own succeeded activate operation.
type drainFixture struct {
	activatedFixture
	predRuntime string
}

func newDrainFixture(t *testing.T, seed byte) drainFixture {
	t.Helper()
	a := newActivatedFixture(t, seed, true)
	if err := a.report(context.Background(), true, ""); err != nil {
		t.Fatal(err)
	}
	var predAttempt int64
	if err := a.pool.QueryRow(context.Background(), `SELECT attempt_id FROM release_operations WHERE release_id=$1 AND kind='activate' AND state='succeeded' ORDER BY id DESC LIMIT 1`, a.predecessorID).Scan(&predAttempt); err != nil {
		t.Fatal(err)
	}
	return drainFixture{activatedFixture: a, predRuntime: dockerid.AttemptRuntimeInstanceID(predAttempt)}
}

func (d drainFixture) record(outcome, summary string) error {
	return d.repo.RecordDrainResult(context.Background(), d.serverID, d.cred, d.attemptID, d.predecessorID, d.predRuntime, outcome, summary)
}

func (d drainFixture) rows(t *testing.T) (n int, outcome, summary string, reportedAt time.Time) {
	t.Helper()
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM release_drain_results WHERE attempt_id=$1`, d.attemptID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 1 {
		if err := d.pool.QueryRow(context.Background(), `SELECT outcome, outcome_summary, reported_at FROM release_drain_results WHERE attempt_id=$1`, d.attemptID).Scan(&outcome, &summary, &reportedAt); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestRecordDrainResultRecordsDerivedFacts(t *testing.T) {
	d := newDrainFixture(t, 120)
	if err := d.record(DrainOutcomeStopFailed, "stop predecessor failed: boom"); err != nil {
		t.Fatal(err)
	}
	var env, rel, pred, rt, outcome string
	if err := d.pool.QueryRow(context.Background(), `SELECT environment_id, release_id, predecessor_release_id, predecessor_runtime_instance_id, outcome FROM release_drain_results WHERE attempt_id=$1`, d.attemptID).Scan(&env, &rel, &pred, &rt, &outcome); err != nil {
		t.Fatal(err)
	}
	if env != d.envID || rel != d.candidateID || pred != d.predecessorID || rt != d.predRuntime || outcome != DrainOutcomeStopFailed {
		t.Fatalf("row = %s %s %s %s %s", env, rel, pred, rt, outcome)
	}
}

func TestRecordDrainResultReplayIsZeroRowsAndConflictKeepsFirst(t *testing.T) {
	d := newDrainFixture(t, 121)
	if err := d.record(DrainOutcomeStopped, "predecessor stopped"); err != nil {
		t.Fatal(err)
	}
	_, _, _, at1 := d.rows(t)
	for i := 0; i < 2; i++ {
		if err := d.record(DrainOutcomeStopped, "predecessor stopped"); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	n, outcome, _, at2 := d.rows(t)
	if n != 1 || outcome != DrainOutcomeStopped || !at1.Equal(at2) {
		t.Fatalf("replay changed the row: n=%d outcome=%s at1=%v at2=%v", n, outcome, at1, at2)
	}
	if err := d.record(DrainOutcomeStopFailed, "predecessor stopped"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("different outcome: err=%v, want integrity conflict", err)
	}
	if err := d.record(DrainOutcomeStopped, "another summary"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("different summary: err=%v, want integrity conflict", err)
	}
	if n, outcome, summary, _ := d.rows(t); n != 1 || outcome != DrainOutcomeStopped || summary != "predecessor stopped" {
		t.Fatalf("first report did not win: n=%d %s %q", n, outcome, summary)
	}
}

func TestRecordDrainResultValidationMatrix(t *testing.T) {
	ctx := context.Background()
	d := newDrainFixture(t, 122)
	otherServer := "other-" + releaseSuffix(t)
	d.insertServer(t, otherServer)
	otherCred := digest(182)
	if _, err := d.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, otherServer, otherCred[:]); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"wrong server presenting its own credential", func() error {
			return d.repo.RecordDrainResult(ctx, otherServer, otherCred, d.attemptID, d.predecessorID, d.predRuntime, DrainOutcomeStopped, "s")
		}, ErrServerAuthorizationDenied},
		{"owning server with a wrong credential", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, otherCred, d.attemptID, d.predecessorID, d.predRuntime, DrainOutcomeStopped, "s")
		}, ErrServerAuthorizationDenied},
		{"unknown attempt", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, d.cred, d.attemptID+1_000_000, d.predecessorID, d.predRuntime, DrainOutcomeStopped, "s")
		}, ErrServerAuthorizationDenied},
		{"wrong predecessor release", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, d.cred, d.attemptID, d.candidateID, d.predRuntime, DrainOutcomeStopped, "s")
		}, ErrAgentLeaseIntegrityConflict},
		{"wrong predecessor runtime instance", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, d.cred, d.attemptID, d.predecessorID, dockerid.AttemptRuntimeInstanceID(d.attemptID), DrainOutcomeStopped, "s")
		}, ErrAgentLeaseIntegrityConflict},
		{"another runtime instance id", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, d.cred, d.attemptID, d.predecessorID, "999999999", DrainOutcomeStopped, "s")
		}, ErrAgentLeaseIntegrityConflict},
		{"zero attempt", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, d.cred, 0, d.predecessorID, d.predRuntime, DrainOutcomeStopped, "s")
		}, ErrAgentLeaseInvalidInput},
		{"unknown outcome", func() error { return d.record("restarted", "s") }, ErrAgentLeaseInvalidInput},
		{"control char in summary", func() error { return d.record(DrainOutcomeStopped, "a\nb") }, ErrAgentLeaseInvalidInput},
		{"oversize summary", func() error { return d.record(DrainOutcomeStopped, strings.Repeat("a", 2049)) }, ErrAgentLeaseInvalidInput},
		{"leading-zero runtime instance", func() error {
			return d.repo.RecordDrainResult(ctx, d.serverID, d.cred, d.attemptID, d.predecessorID, "0"+d.predRuntime, DrainOutcomeStopped, "s")
		}, ErrAgentLeaseInvalidInput},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v, want %v", tc.name, err, tc.want)
		}
	}
	if n, _, _, _ := d.rows(t); n != 0 {
		t.Fatalf("a refused report wrote %d rows", n)
	}
}

func TestRecordDrainResultRefusesNonSucceededAttempt(t *testing.T) {
	a := newActivatedFixture(t, 123, true) // activated, never reported: attempt still leased
	pred := dockerid.AttemptRuntimeInstanceID(1)
	err := a.repo.RecordDrainResult(context.Background(), a.serverID, a.cred, a.attemptID, a.predecessorID, pred, DrainOutcomeStopped, "s")
	if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("err=%v, want integrity conflict", err)
	}
}

func TestRecordDrainResultFirstDeploymentHasNoPredecessor(t *testing.T) {
	a := newActivatedFixture(t, 124, false)
	if err := a.report(context.Background(), true, ""); err != nil {
		t.Fatal(err)
	}
	err := a.repo.RecordDrainResult(context.Background(), a.serverID, a.cred, a.attemptID, "rel-whatever", "5", DrainOutcomeStopped, "s")
	if !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("err=%v, want integrity conflict", err)
	}
}

func TestReadDrainResultByJob(t *testing.T) {
	ctx := context.Background()
	d := newDrainFixture(t, 126)
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if got, err := readDrainResult(ctx, tx, d.jobID); err != nil || got != nil {
		t.Fatalf("before report: %v %v", got, err)
	}
	if err := d.record(DrainOutcomeStopFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	got, err := readDrainResult(ctx, tx, d.jobID)
	if err != nil || got == nil || got.Outcome != DrainOutcomeStopFailed || got.Summary != "boom" || got.PredecessorReleaseID != d.predecessorID {
		t.Fatalf("got %+v err=%v", got, err)
	}
}
