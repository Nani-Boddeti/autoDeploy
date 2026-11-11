package executor

import (
	"errors"
	"log"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"autodeploy/internal/agent/state"
)

type recoveryFake struct {
	records   []state.PendingActivation
	corrupt   []string
	scanErr   error
	saveErrOn map[int64]error
	saved     []state.PendingActivation
}

func (f *recoveryFake) ScanActivations() ([]state.PendingActivation, []string, error) {
	return f.records, f.corrupt, f.scanErr
}

func (f *recoveryFake) SaveActivation(p state.PendingActivation) error {
	if err := f.saveErrOn[p.AttemptID]; err != nil {
		return err
	}
	f.saved = append(f.saved, p)
	return nil
}

func authorizedRec(attempt int64, env string) state.PendingActivation {
	rec := state.PendingActivation{
		AttemptID: attempt, Fence: 3, EnvironmentID: env, OperationID: "9001", RouteRevision: 5,
		Phase: state.PhaseAuthorized,
	}
	rec.RouteDigest[0], rec.ImageDigest[0] = 0xab, 0xcd
	return rec
}

func TestRecoverActivationsConvertsAuthorizedToFailure(t *testing.T) {
	rec := authorizedRec(7, "env_a")
	authorized := routeDigestString(rec.RouteDigest)
	cases := []struct {
		name   string
		read   func(string, string) (string, bool, error)
		wantIn string
	}{
		{"absent", func(string, string) (string, bool, error) { return "", false, nil }, "on-disk route digest: absent"},
		{"equal", func(string, string) (string, bool, error) { return authorized, true, nil }, "on-disk route digest: " + authorized},
		{"different", func(string, string) (string, bool, error) { return "sha256:" + strings.Repeat("1", 64), true, nil }, "on-disk route digest: sha256:" + strings.Repeat("1", 64)},
		{"unreadable", func(string, string) (string, bool, error) { return "", false, errors.New("EIO") }, "on-disk route digest: unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &recoveryFake{records: []state.PendingActivation{rec}}
			if n := recoverActivations(f, "/routes", tc.read, log.New(&strings.Builder{}, "", 0)); n != 1 {
				t.Fatalf("converted=%d", n)
			}
			if len(f.saved) != 1 {
				t.Fatalf("saved=%d", len(f.saved))
			}
			got := f.saved[0]
			if got.Phase != state.PhaseResultReady || got.Succeeded || got.ObservedRouteDigest != ([32]byte{}) {
				t.Fatalf("record = %+v, want a result_ready FAILURE even when digests are equal", got)
			}
			for _, want := range []string{tc.wantIn, "authorized route digest: " + authorized, "agent restarted mid-activation", "route not rolled back"} {
				if !strings.Contains(got.FailureSummary, want) {
					t.Fatalf("summary %q lacks %q", got.FailureSummary, want)
				}
			}
			if len(got.FailureSummary) > maxFailureSummaryBytes || !utf8.ValidString(got.FailureSummary) {
				t.Fatalf("summary not bounded/valid: %d bytes", len(got.FailureSummary))
			}
		})
	}
}

func TestRecoverActivationsLeavesOtherRecordsUntouched(t *testing.T) {
	ready := authorizedRec(1, "env_a")
	ready.Phase = state.PhaseResultReady
	ready.FailureSummary = "x"
	f := &recoveryFake{records: []state.PendingActivation{ready}, corrupt: []string{"bad.json"}}
	read := func(string, string) (string, bool, error) {
		t.Fatal("must not read the route for a non-authorized record")
		return "", false, nil
	}
	if n := recoverActivations(f, "/routes", read, nil); n != 0 || len(f.saved) != 0 {
		t.Fatalf("converted=%d saved=%d, want none", n, len(f.saved))
	}
}

func TestRecoverActivationsScanErrorIsLoggedNotFatal(t *testing.T) {
	var buf strings.Builder
	f := &recoveryFake{scanErr: errors.New("permission denied")}
	if n := recoverActivations(f, "/routes", nil, log.New(&buf, "", 0)); n != 0 {
		t.Fatalf("converted=%d", n)
	}
	if !strings.Contains(buf.String(), "ERROR") || !strings.Contains(buf.String(), "cannot scan") {
		t.Fatalf("scan error not logged loudly: %q", buf.String())
	}
}

func TestRecoverActivationsOneSaveErrorDoesNotStopTheOthers(t *testing.T) {
	var buf strings.Builder
	f := &recoveryFake{
		records:   []state.PendingActivation{authorizedRec(1, "env_a"), authorizedRec(2, "env_b"), authorizedRec(3, "env_c")},
		saveErrOn: map[int64]error{2: errors.New("disk full")},
	}
	read := func(string, string) (string, bool, error) { return "", false, nil }
	if n := recoverActivations(f, "/routes", read, log.New(&buf, "", 0)); n != 2 {
		t.Fatalf("converted=%d, want 2", n)
	}
	if len(f.saved) != 2 || f.saved[0].AttemptID != 1 || f.saved[1].AttemptID != 3 {
		t.Fatalf("saved = %+v", f.saved)
	}
	if !strings.Contains(buf.String(), "attempt 2") || !strings.Contains(buf.String(), "ERROR") {
		t.Fatalf("save error not logged: %q", buf.String())
	}
}

func TestRecoverActivationsSummaryStaysBoundedForLongEnvironmentRoute(t *testing.T) {
	rec := authorizedRec(1, "env_a")
	f := &recoveryFake{records: []state.PendingActivation{rec}}
	read := func(string, string) (string, bool, error) {
		return "sha256:" + strings.Repeat("é", 4000), true, nil // invalid-looking oversized evidence
	}
	recoverActivations(f, "/routes", read, nil)
	if len(f.saved) != 1 || len(f.saved[0].FailureSummary) > maxFailureSummaryBytes || !utf8.ValidString(f.saved[0].FailureSummary) {
		t.Fatalf("summary not bounded/valid")
	}
}

func TestRecoverActivationsAgainstRealStore(t *testing.T) {
	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acts, err := st.Activations()
	if err != nil {
		t.Fatal(err)
	}
	rec := authorizedRec(9, "env_a")
	if err := acts.SaveActivation(rec); err != nil {
		t.Fatal(err)
	}
	if n := RecoverActivations(acts, t.TempDir(), nil); n != 1 {
		t.Fatalf("converted=%d", n)
	}
	got, ok, err := acts.LoadActivation(9)
	if err != nil || !ok || got.Phase != state.PhaseResultReady || got.Succeeded || !strings.Contains(got.FailureSummary, "on-disk route digest: absent") {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestRecoverActivationsWithDrainFieldsConvertsToFailureAndArmsNothing(t *testing.T) {
	rec := authorizedRec(7, "env_a")
	rec.ReleaseID, rec.BackendRuntimeInstanceID = "rel_new", "7"
	rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID = "rel_old", "6"
	rec.DrainSeconds = 30
	f := &recoveryFake{records: []state.PendingActivation{rec}}
	read := func(string, string) (string, bool, error) { return "", false, nil }
	if n := recoverActivations(f, "/routes", read, log.New(&strings.Builder{}, "", 0)); n != 1 {
		t.Fatalf("converted=%d", n)
	}
	got := f.saved[0]
	if got.Phase != state.PhaseResultReady || got.Succeeded || got.PredecessorRuntimeInstanceID != "6" || got.DrainSeconds != 30 {
		t.Fatalf("record = %+v", got)
	}
	// Recovery has no drain-store access, and a failure result never owes a drain.
	if _, owed := state.DrainFromActivation(got, time.Now()); owed {
		t.Fatal("recovered failure owes a drain")
	}
}
