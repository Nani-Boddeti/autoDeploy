package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"autodeploy/internal/agent/state"
	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
	"autodeploy/internal/runtime/dockerid"
	"autodeploy/internal/runtime/traefik"
)

const rollbackTargetRelease = "relTarget"

type rollbackRig struct {
	*stsRig
	co    *fakeCheckout
	built int
}

// newRollbackRig builds a rollback lease (strategy as given) over an engine that
// holds the running predecessor (the current active release) and, when
// targetPresent, the target's retained container (stopped unless targetRunning).
// The target container carries the image digest label.
func newRollbackRig(t *testing.T, strategy string, targetPresent, targetRunning bool) *rollbackRig {
	t.Helper()
	lease := testLease()
	lease.ReleaseStrategy = strategy
	lease.DrainSeconds = 30
	targetRT := dockerid.AttemptRuntimeInstanceID(33)
	lease.ReleaseID, lease.RollbackTargetReleaseID, lease.RollbackTargetRuntimeInstanceID = rollbackTargetRelease, rollbackTargetRelease, targetRT
	predRT := dockerid.AttemptRuntimeInstanceID(41)
	// candID = the target's container id, so the event log names it "cand".
	dk := &stsDocker{ctrs: map[string]*stsCtr{}, gate: docker.HealthGateResult{Success: true}, candID: "target-id", stopErr: map[string]error{}, startErr: map[string]error{}}
	dk.add("pred-id", lease.EnvironmentID, stsPredRelease, predRT, true)
	if targetPresent {
		dk.add("target-id", lease.EnvironmentID, rollbackTargetRelease, targetRT, targetRunning).labels[docker.LabelImageDigest] = testImageDigest
	}
	act := newActFake(lease)
	act.resp.PredecessorReleaseID, act.resp.PredecessorRuntimeInstanceID = stsPredRelease, predRT
	act.resp.BackendRuntimeInstanceID = targetRT
	_, routeDigest, err := traefik.Render(act.route(act.resp))
	if err != nil {
		t.Fatal(err)
	}
	act.resp.RouteDigest, _ = parseRouteDigestString(routeDigest)
	rr := &rollbackRig{co: &fakeCheckout{result: happyCheckoutResult()}}
	cp := &fakeControlPlane{secrets: fullSecrets()}
	e := &Executor{
		activation: act.runner(), checkout: rr.co, docker: dk, controlPlane: cp,
		build: func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
			rr.built++
			return docker.BuildResult{}, errors.New("a rollback must not build")
		},
		serverID: "server1", network: "env-network", buildOutput: discardWriter{}, logger: testLogger(),
	}
	rr.stsRig = &stsRig{e: e, dk: dk, act: act, lease: lease, cp: cp}
	act.onActivated = func() { rr.seq = append(rr.seq, "activate") }
	dk.hook = func(ev string) { rr.seq = append(rr.seq, ev) }
	origWrite := e.activation.write
	e.activation.write = func(dir string, rt traefik.EnvironmentRoute) (traefik.WriteResult, error) {
		rr.seq = append(rr.seq, "route-write")
		return origWrite(dir, rt)
	}
	return rr
}

func (r *rollbackRig) assertNoBuildOrCreate(t *testing.T) {
	t.Helper()
	if r.co.checkoutN != 0 || r.built != 0 {
		t.Fatalf("checkout=%d build=%d, a rollback does neither", r.co.checkoutN, r.built)
	}
	if indexOf(r.seq, "ensure") >= 0 {
		t.Fatalf("sequence %v created a container", r.seq)
	}
}

func TestRollbackBlueGreenStartsExistingGatesAndActivates(t *testing.T) {
	r := newRollbackRig(t, "blue_green", true, false)
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff || out.Succeeded || out.AuthorizationUnknown {
		t.Fatalf("outcome = %+v", out)
	}
	if want := []string{"start:cand", "gate", "activate", "route-write"}; !reflect.DeepEqual(r.seq, want) {
		t.Fatalf("sequence = %v, want %v", r.seq, want)
	}
	r.assertNoBuildOrCreate(t)
	wantPhases := []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking}
	if !reflect.DeepEqual(r.cp.progressCalls, wantPhases) {
		t.Fatalf("progress = %v, want %v", r.cp.progressCalls, wantPhases)
	}
	rec := r.act.last(t)
	if rec.Phase != state.PhaseResultReady || !rec.Succeeded || rec.ReleaseID != rollbackTargetRelease || rec.BackendRuntimeInstanceID != r.lease.RollbackTargetRuntimeInstanceID {
		t.Fatalf("record = %+v", rec)
	}
	if rec.PredecessorStopSent || rec.PredecessorReleaseID != stsPredRelease {
		t.Fatalf("blue/green rollback must leave the old active to the post-success drain: %+v", rec)
	}
	if r.act.activateN != 1 {
		t.Fatalf("activate calls = %d", r.act.activateN)
	}
	if !r.running("cand") || !r.running("pred") {
		t.Fatal("both containers run until the drain")
	}
}

func TestRollbackAlreadyRunningTargetIsAccepted(t *testing.T) {
	r := newRollbackRig(t, "blue_green", true, true)
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	if indexOf(r.seq, "start:cand") >= 0 {
		t.Fatalf("sequence %v started an already running container", r.seq)
	}
	if !r.act.last(t).Succeeded {
		t.Fatal("rollback did not succeed")
	}
}

func TestRollbackGateFailureStopsTheTargetAgain(t *testing.T) {
	r := newRollbackRig(t, "blue_green", true, false)
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: "status=503"}
	out := r.e.Execute(context.Background(), r.lease)
	if out.Succeeded || out.HandedOff || out.FailureClass == "" {
		t.Fatalf("outcome = %+v, want a failed attempt", out)
	}
	if r.running("cand") {
		t.Fatal("the retained container must be stopped again after a failed gate")
	}
	if !r.running("pred") {
		t.Fatal("the active release must be untouched")
	}
	if r.act.activateN != 0 || len(r.act.writes) != 0 {
		t.Fatalf("activate=%d writes=%d: nothing may be authorized", r.act.activateN, len(r.act.writes))
	}
	if indexOf(r.seq, "stop:cand") < 0 {
		t.Fatalf("sequence %v never stopped the target", r.seq)
	}
}

func TestRollbackMissingContainerIsPermanentConfiguration(t *testing.T) {
	for _, strategy := range []string{"blue_green", "stop_then_start"} {
		t.Run(strategy, func(t *testing.T) {
			r := newRollbackRig(t, strategy, false, false)
			out := r.e.Execute(context.Background(), r.lease)
			if out.Succeeded || out.HandedOff || out.FailureClass != queue.FailureClassPermanentConfiguration {
				t.Fatalf("outcome = %+v, want permanent_configuration", out)
			}
			r.assertNoBuildOrCreate(t)
			if r.act.activateN != 0 || indexOf(r.seq, "stop:pred") >= 0 {
				t.Fatalf("a missing target must change nothing: activate=%d seq=%v", r.act.activateN, r.seq)
			}
			if !r.running("pred") {
				t.Fatal("the active release must keep running")
			}
		})
	}
}

func TestRollbackStopThenStartStopsActiveThenStartsTarget(t *testing.T) {
	r := newRollbackRig(t, "stop_then_start", true, false)
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	if want := []string{"activate", "stop:pred", "start:cand", "gate", "route-write"}; !reflect.DeepEqual(r.seq, want) {
		t.Fatalf("sequence = %v, want %v", r.seq, want)
	}
	r.assertNoBuildOrCreate(t)
	rec := r.act.last(t)
	if !rec.Succeeded || !rec.PredecessorStopSent {
		t.Fatalf("record = %+v", rec)
	}
	if r.running("pred") || !r.running("cand") {
		t.Fatal("old active must stay stopped, target running")
	}
}

func TestRollbackStopThenStartGateFailureRestoresActive(t *testing.T) {
	r := newRollbackRig(t, "stop_then_start", true, false)
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: "status=503"}
	out := r.e.Execute(context.Background(), r.lease)
	assertHandedOffFailure(t, r.act, out, "predecessor stopped", "candidate failed health gate", "candidate stopped", "predecessor restart ok")
	if r.running("cand") || !r.running("pred") {
		t.Fatalf("cand running=%v pred running=%v, want target stopped and old active restarted", r.running("cand"), r.running("pred"))
	}
}

func TestRollbackStartFailureIsReportedAndNothingAuthorized(t *testing.T) {
	r := newRollbackRig(t, "blue_green", true, false)
	r.dk.startErr["target-id"] = &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-start", Err: errors.New("daemon busy")}
	out := r.e.Execute(context.Background(), r.lease)
	if out.Succeeded || out.HandedOff || out.FailureClass == "" {
		t.Fatalf("outcome = %+v", out)
	}
	if r.act.activateN != 0 {
		t.Fatal("authorized despite a failed start")
	}
}

func TestRollbackMalformedLeaseIsCorruptState(t *testing.T) {
	r := newRollbackRig(t, "blue_green", true, false)
	lease := r.lease
	lease.RollbackTargetRuntimeInstanceID = ""
	if out := r.e.Execute(context.Background(), lease); out.FailureClass != queue.FailureClassCorruptState || r.dk.events != nil {
		t.Fatalf("outcome = %+v events=%v", out, r.dk.events)
	}
	lease = r.lease
	lease.ReleaseID = "someone-else"
	if out := r.e.Execute(context.Background(), lease); out.FailureClass != queue.FailureClassCorruptState {
		t.Fatalf("outcome = %+v", out)
	}
}

// A crash in a retained (rollback) stop-then-start: recovery stops the target's
// container, restarts the predecessor, and never creates or removes anything.
func TestRollbackRecoveryWithPredecessorStopSentRestoresPredecessor(t *testing.T) {
	absent := func(string, string) (string, bool, error) { return "", false, nil }
	for name, targetRunning := range map[string]bool{"crash after the target started": true, "crash before the target started": false} {
		t.Run(name, func(t *testing.T) {
			rec, d := stsStranded(t, true)
			targetRT := dockerid.AttemptRuntimeInstanceID(33)
			rec.ReleaseID, rec.BackendRuntimeInstanceID = rollbackTargetRelease, targetRT
			d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, false)
			d.add("cand-id", rec.EnvironmentID, rollbackTargetRelease, targetRT, targetRunning)
			f := &recoveryFake{records: []state.PendingActivation{rec}}
			if n := recoverActivationsWith(f, "/routes", absent, testLogger(), d); n != 1 {
				t.Fatalf("converted=%d", n)
			}
			got := f.saved[0]
			if !strings.Contains(got.FailureSummary, "predecessor restart ok") || got.Succeeded || !got.PredecessorStopSent {
				t.Fatalf("record = %+v", got)
			}
			if !d.ctrs["pred-id"].running || d.ctrs["cand-id"].running {
				t.Fatalf("pred running=%v target running=%v, want predecessor restored and target stopped", d.ctrs["pred-id"].running, d.ctrs["cand-id"].running)
			}
			if indexOf(d.events, "ensure") >= 0 || len(d.ctrs) != 2 {
				t.Fatalf("recovery created or removed a container: events=%v containers=%d", d.events, len(d.ctrs))
			}
		})
	}
}
