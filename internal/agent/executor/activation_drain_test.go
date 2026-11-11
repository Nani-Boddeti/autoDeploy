package executor

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
)

// recordingArmer records ArmDrain calls and the delete state at call time.
type recordingArmer struct {
	f           *actFake
	err         error
	failFirst   int // fail this many calls, then succeed
	armed       []state.PendingDrain
	deletedSeen []int // len(f.deleted) when each ArmDrain ran
}

func (r *recordingArmer) ArmDrain(d state.PendingDrain) error {
	r.deletedSeen = append(r.deletedSeen, len(r.f.deleted))
	if r.err != nil {
		return r.err
	}
	if r.failFirst > 0 {
		r.failFirst--
		return errors.New("transient")
	}
	r.armed = append(r.armed, d)
	return nil
}

func predFake() (*actFake, *recordingArmer, *Executor, agentclient.Lease) {
	lease := testLease()
	lease.DrainSeconds = 45
	f := newActFake(lease)
	f.resp.PredecessorReleaseID, f.resp.PredecessorRuntimeInstanceID = "rel_old", "41"
	e := f.executor()
	arm := &recordingArmer{f: f}
	e.activation.cfg.Drains = arm
	return f, arm, e, lease
}

func TestAuthorizedRecordCarriesDrainFields(t *testing.T) {
	f, _, e, lease := predFake()
	e.activate(context.Background(), lease, testImageDigest)
	rec := f.saved[0]
	if rec.Phase != state.PhaseAuthorized || rec.ReleaseID != lease.ReleaseID ||
		rec.BackendRuntimeInstanceID != f.resp.BackendRuntimeInstanceID ||
		rec.PredecessorReleaseID != "rel_old" || rec.PredecessorRuntimeInstanceID != "41" || rec.DrainSeconds != 45 {
		t.Fatalf("authorized record = %+v", rec)
	}
	if len(f.reports) != 0 || len(f.deleted) != 0 {
		t.Fatal("normal path must leave arming to the reporter")
	}
}

func TestDirectReport200ArmsDrainBeforeDelete(t *testing.T) {
	f, arm, e, lease := predFake()
	f.saveErrFrom = 2
	out := e.activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || len(arm.armed) != 1 {
		t.Fatalf("outcome=%+v armed=%d", out, len(arm.armed))
	}
	d := arm.armed[0]
	if d.PredecessorRuntimeInstanceID != "41" || d.BackendRuntimeInstanceID != f.resp.BackendRuntimeInstanceID ||
		d.DrainSeconds != 45 || d.ObservedRouteDigest != f.resp.RouteDigest || d.Phase != state.DrainPending || d.ArmedAt.IsZero() {
		t.Fatalf("drain = %+v", d)
	}
	if arm.deletedSeen[0] != 0 || len(f.deleted) != 1 {
		t.Fatalf("drain must be armed before the delete: deletedSeen=%v deleted=%v", arm.deletedSeen, f.deleted)
	}
}

func TestDirectReportTerminal409NeverArms(t *testing.T) {
	for _, code := range []string{agentclient.ConflictLateReportRecorded, agentclient.ConflictRouteMoved, agentclient.ConflictIntegrity} {
		f, arm, e, lease := predFake()
		f.saveErrFrom = 2
		f.reportErrs = []error{&agentclient.ConflictError{Code: code}}
		e.activate(context.Background(), lease, testImageDigest)
		if len(arm.deletedSeen) != 0 {
			t.Fatalf("%s: armed on a 409", code)
		}
		if len(f.deleted) != 1 {
			t.Fatalf("%s: terminal 409 must still delete the authorized record", code)
		}
	}
}

func TestDirectReportFailureResultNeverArms(t *testing.T) {
	f, arm, e, lease := predFake()
	f.saveErrFrom = 2
	f.probeErr = errors.New("probe boom")
	e.activate(context.Background(), lease, testImageDigest)
	if len(f.reports) != 1 || len(arm.deletedSeen) != 0 || len(f.deleted) != 1 {
		t.Fatalf("reports=%d arm calls=%d deleted=%v", len(f.reports), len(arm.deletedSeen), f.deleted)
	}
}

func TestDirectReportNoPredecessorNeverArms(t *testing.T) {
	f, arm, e, lease := predFake()
	f.resp.PredecessorReleaseID, f.resp.PredecessorRuntimeInstanceID = "", ""
	f.saveErrFrom = 2
	e.activate(context.Background(), lease, testImageDigest)
	if len(arm.deletedSeen) != 0 || len(f.deleted) != 1 {
		t.Fatalf("arm calls=%d deleted=%v", len(arm.deletedSeen), f.deleted)
	}
}

func TestDirectReportArmFailureKeepsAuthorizedRecord(t *testing.T) {
	f, arm, e, lease := predFake()
	arm.err = errors.New("disk full")
	f.saveErrFrom = 2
	out := e.activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || len(f.deleted) != 0 {
		t.Fatalf("outcome=%+v deleted=%v: authorized record must be kept", out, f.deleted)
	}
	if f.kicks != 1 {
		t.Fatalf("kicks=%d", f.kicks)
	}
}

func TestDirectReportNilDrainStoreStillDeletes(t *testing.T) {
	f, _, e, lease := predFake()
	e.activation.cfg.Drains = nil
	var logs strings.Builder
	e.activation.logger = log.New(&logs, "", 0)
	f.saveErrFrom = 2
	e.activate(context.Background(), lease, testImageDigest)
	if len(f.deleted) != 1 {
		t.Fatalf("deleted=%v", f.deleted)
	}
	if !strings.Contains(logs.String(), "ERROR attempt 42: a predecessor drain is owed but no drain store") {
		t.Fatalf("lost drain not logged: %s", logs.String())
	}
}

func TestDirectReportArmRetriesTransientFailure(t *testing.T) {
	f, arm, e, lease := predFake()
	arm.failFirst = 2
	f.saveErrFrom = 2
	e.activate(context.Background(), lease, testImageDigest)
	if len(arm.armed) != 1 || len(f.deleted) != 1 || len(arm.deletedSeen) != 3 {
		t.Fatalf("armed=%d deleted=%v arm calls=%d", len(arm.armed), f.deleted, len(arm.deletedSeen))
	}
}
