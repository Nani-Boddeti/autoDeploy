package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/deployment"
	"autodeploy/internal/runtime/docker"
	"autodeploy/internal/runtime/dockerid"
	"autodeploy/internal/runtime/traefik"
)

// stsDocker is a small stateful engine: containers by id with identity labels
// and a running flag, plus an ordered event log. It implements dockerRuntime.
type stsDocker struct {
	ctrs   map[string]*stsCtr
	events []string
	hook   func(ev string)

	ensureErr error
	gate      docker.HealthGateResult
	gateErr   error
	stopErr   map[string]error
	startErr  map[string]error
	candID    string
}

type stsCtr struct {
	id, name string
	labels   map[string]string
	running  bool
}

func (d *stsDocker) rec(ev string) {
	d.events = append(d.events, ev)
	if d.hook != nil {
		d.hook(ev)
	}
}

func (d *stsDocker) add(id, env, rel, rt string, running bool) *stsCtr {
	name, _ := docker.ContainerName(rt)
	c := &stsCtr{id: id, name: name, running: running, labels: map[string]string{
		docker.LabelManaged: "true", docker.LabelKind: docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID: env, docker.LabelReleaseID: rel, docker.LabelRuntimeInstanceID: rt,
	}}
	d.ctrs[id] = c
	return c
}

func (d *stsDocker) find(idOrName string) *stsCtr {
	for _, c := range d.ctrs {
		if c.id == idOrName || c.name == idOrName {
			return c
		}
	}
	return nil
}

func (d *stsDocker) label(c *stsCtr) string {
	switch c.id {
	case "pred-id":
		return "pred"
	case d.candID:
		return "cand"
	}
	return c.id
}

func (d *stsDocker) Probe(context.Context) (docker.VersionInfo, error) {
	return docker.VersionInfo{}, nil
}

func (d *stsDocker) InspectContainer(_ context.Context, id string) (docker.ContainerInspect, error) {
	c := d.find(id)
	if c == nil {
		return docker.ContainerInspect{}, docker.ErrContainerNotFound
	}
	out := testInspect("networkabc123")
	out.ID = c.id
	out.State.Running = c.running
	out.State.Status = map[bool]string{true: "running", false: "exited"}[c.running]
	return out, nil
}

func (d *stsDocker) ListContainersByLabels(_ context.Context, want map[string]string) ([]docker.ContainerSummary, error) {
	var out []docker.ContainerSummary
	for _, c := range d.ctrs {
		ok := true
		for k, v := range want {
			if c.labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, docker.ContainerSummary{ID: c.id, Labels: c.labels})
		}
	}
	return out, nil
}

func (d *stsDocker) StopContainer(ctx context.Context, id string, _ time.Duration) error {
	c := d.find(id)
	d.rec("stop:" + d.label(c))
	if err := d.stopErr[id]; err != nil {
		return err
	}
	c.running = false
	return ctx.Err() // the stop landed but the response was cut off
}

func (d *stsDocker) WaitStopped(_ context.Context, id string, _, _ time.Duration) (docker.ContainerInspect, error) {
	return docker.ContainerInspect{}, nil
}

func (d *stsDocker) StartContainer(ctx context.Context, id string) error {
	c := d.find(id)
	d.rec("start:" + d.label(c))
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.startErr[id]; err != nil {
		return err
	}
	c.running = true
	return nil
}

func (d *stsDocker) EnsureReleaseRuntimeContainer(_ context.Context, req docker.RunContainerRequest, _ string) (docker.ContainerInspect, error) {
	d.rec("ensure")
	if d.ensureErr != nil {
		return docker.ContainerInspect{}, d.ensureErr
	}
	d.add(d.candID, req.Identity.EnvironmentID, req.Identity.ReleaseID, req.Identity.RuntimeInstanceID, true)
	out := testInspect("networkabc123")
	out.ID = d.candID
	return out, nil
}

func (d *stsDocker) RunHealthGate(context.Context, string, string, int, docker.HealthGateTarget) (docker.HealthGateResult, error) {
	d.rec("gate")
	return d.gate, d.gateErr
}

type stsRig struct {
	e     *Executor
	dk    *stsDocker
	act   *actFake
	lease agentclient.Lease
	cp    *fakeControlPlane
	seq   []string
}

const (
	stsPredRelease = "relOld"
)

func newSTSRig(t *testing.T, withPredecessor bool) *stsRig {
	t.Helper()
	lease := testLease()
	lease.ReleaseStrategy = "stop_then_start"
	lease.DrainSeconds = 30
	predRT := dockerid.AttemptRuntimeInstanceID(41)
	dk := &stsDocker{ctrs: map[string]*stsCtr{}, gate: docker.HealthGateResult{Success: true}, candID: "cand-id",
		stopErr: map[string]error{}, startErr: map[string]error{}}
	act := newActFake(lease)
	if withPredecessor {
		dk.add("pred-id", lease.EnvironmentID, stsPredRelease, predRT, true)
		act.resp.PredecessorReleaseID, act.resp.PredecessorRuntimeInstanceID = stsPredRelease, predRT
	}
	cp := &fakeControlPlane{secrets: fullSecrets()}
	co := &fakeCheckout{result: happyCheckoutResult()}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: testImageDigest, ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := &Executor{
		activation: act.runner(), checkout: co, docker: dk, build: build, controlPlane: cp,
		serverID: "server1", network: "env-network", buildOutput: discardWriter{}, logger: testLogger(),
	}
	r := &stsRig{e: e, dk: dk, act: act, lease: lease, cp: cp}
	// One ordered log across the control plane, store and engine.
	act.onActivated = func() { r.seq = append(r.seq, "activate") }
	dk.hook = func(ev string) { r.seq = append(r.seq, ev) }
	origWrite := e.activation.write
	e.activation.write = func(dir string, rt traefik.EnvironmentRoute) (traefik.WriteResult, error) {
		r.seq = append(r.seq, "route-write")
		return origWrite(dir, rt)
	}
	return r
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (r *stsRig) running(name string) bool {
	for _, c := range r.dk.ctrs {
		if r.dk.label(c) == name {
			return c.running
		}
	}
	return false
}

func TestSTSSequenceOrder(t *testing.T) {
	r := newSTSRig(t, true)
	// The stop-intent must be durable before the stop is sent.
	r.dk.hook = func(ev string) {
		r.seq = append(r.seq, ev)
		if ev == "stop:pred" && !r.act.last(t).PredecessorStopSent {
			t.Error("PredecessorStopSent was not persisted before the predecessor stop")
		}
	}
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff || out.Succeeded {
		t.Fatalf("outcome = %+v", out)
	}
	want := []string{"activate", "stop:pred", "ensure", "gate", "route-write"}
	if !reflect.DeepEqual(r.seq, want) {
		t.Fatalf("sequence = %v, want %v", r.seq, want)
	}
	if r.act.preflightN != 1 || r.act.activateNAtPreflight != 0 {
		t.Fatalf("preflight must run once, before Activate (preflightN=%d activateNAtPreflight=%d)", r.act.preflightN, r.act.activateNAtPreflight)
	}
	wantPhases := []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking}
	if !reflect.DeepEqual(r.cp.progressCalls, wantPhases) {
		t.Fatalf("progress = %v, want %v", r.cp.progressCalls, wantPhases)
	}
	rec := r.act.last(t)
	if rec.Phase != state.PhaseResultReady || !rec.Succeeded || !rec.PredecessorStopSent {
		t.Fatalf("record = %+v", rec)
	}
	if _, owed := state.DrainFromActivation(rec, time.Now()); owed {
		t.Fatal("a stop-then-start success must not arm a drain")
	}
	if r.running("pred") || !r.running("cand") {
		t.Fatal("predecessor must stay stopped and the candidate running after success")
	}
}

func TestSTSFirstDeploymentSkipsStop(t *testing.T) {
	r := newSTSRig(t, false)
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	if want := []string{"activate", "ensure", "gate", "route-write"}; !reflect.DeepEqual(r.seq, want) {
		t.Fatalf("sequence = %v, want %v", r.seq, want)
	}
	rec := r.act.last(t)
	if !rec.Succeeded || rec.PredecessorStopSent {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSTSStopFailureNeverStartsCandidate(t *testing.T) {
	r := newSTSRig(t, true)
	r.dk.stopErr["pred-id"] = errors.New("engine refused")
	out := r.e.Execute(context.Background(), r.lease)
	rec := assertHandedOffFailure(t, r.act, out, "predecessor stop failed", "candidate never started", "predecessor restart ok")
	if !rec.PredecessorStopSent {
		t.Fatal("intent must stay recorded")
	}
	for _, ev := range r.seq {
		if ev == "ensure" || ev == "gate" || ev == "route-write" {
			t.Fatalf("sequence %v continued past the failed stop", r.seq)
		}
	}
	if !r.running("pred") {
		t.Fatal("predecessor was not restarted")
	}
}

func TestSTSCandidateStartAndGateFailuresRestorePredecessor(t *testing.T) {
	cases := map[string]struct {
		set  func(*stsDocker)
		want []string
	}{
		"start fails": {func(d *stsDocker) { d.ensureErr = docker.ErrInconsistentLocalState }, []string{"candidate start failed", "predecessor restart ok"}},
		"gate fails": {func(d *stsDocker) {
			d.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: "status=503"}
		}, []string{"candidate failed health gate", `last_probe="status=503"`, "candidate stopped", "predecessor restart ok"}},
		"gate errors": {func(d *stsDocker) { d.gateErr = errors.New("engine hiccup") }, []string{"health gate errored", "predecessor restart ok"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newSTSRig(t, true)
			tc.set(r.dk)
			out := r.e.Execute(context.Background(), r.lease)
			assertHandedOffFailure(t, r.act, out, append([]string{"predecessor stopped"}, tc.want...)...)
			if r.running("cand") || !r.running("pred") {
				t.Fatalf("cand running=%v pred running=%v, want candidate stopped, predecessor restarted", r.running("cand"), r.running("pred"))
			}
			// Candidate stop strictly before the predecessor restart.
			stopCand, startPred := indexOf(r.seq, "stop:cand"), indexOf(r.seq, "start:pred")
			if name != "start fails" && (stopCand < 0 || startPred < stopCand) {
				t.Fatalf("sequence = %v, want candidate stop before predecessor restart", r.seq)
			}
			if r.act.probeN != 0 || len(r.act.writes) != 0 {
				t.Fatal("route must not be touched")
			}
		})
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestSTSRestartFailureIsReported(t *testing.T) {
	r := newSTSRig(t, true)
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded}
	r.dk.startErr["pred-id"] = errors.New("no such image")
	out := r.e.Execute(context.Background(), r.lead())
	assertHandedOffFailure(t, r.act, out, "predecessor restart failed")
}

func (r *stsRig) lead() agentclient.Lease { return r.lease }

func TestSTSPredecessorNotRestartedWhenCandidateCannotBeStopped(t *testing.T) {
	r := newSTSRig(t, true)
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded}
	r.dk.stopErr["cand-id"] = errors.New("stuck")
	out := r.e.Execute(context.Background(), r.lease)
	assertHandedOffFailure(t, r.act, out, "candidate could NOT be stopped", "predecessor restart not attempted")
	if indexOf(r.seq, "start:pred") >= 0 {
		t.Fatalf("predecessor restarted while the candidate may still run: %v", r.seq)
	}
}

func TestSTSSwitchFailureRollsBackOnlyBeforeRouteChange(t *testing.T) {
	t.Run("before the route is written", func(t *testing.T) {
		r := newSTSRig(t, true)
		r.act.recordErr = errors.New("disk full") // revision-guard: nothing written
		out := r.e.Execute(context.Background(), r.lease)
		assertHandedOffFailure(t, r.act, out, "route switch failed before the route changed", "candidate stopped", "predecessor restart ok", "revision-guard")
		if r.running("cand") || !r.running("pred") {
			t.Fatal("expected candidate stopped, predecessor restarted")
		}
	})
	t.Run("after the route points at the candidate", func(t *testing.T) {
		r := newSTSRig(t, true)
		r.act.probeErr = errors.New("502") // route written, data plane not ready
		out := r.e.Execute(context.Background(), r.lease)
		assertHandedOffFailure(t, r.act, out, "left as found", "data-plane-probe")
		if !r.running("cand") || r.running("pred") {
			t.Fatal("containers must be left as found when the route may point at the candidate")
		}
	})
}

func TestSTSPredecessorEqualToCandidateIsRefused(t *testing.T) {
	r := newSTSRig(t, true)
	candRT := dockerid.AttemptRuntimeInstanceID(r.lease.AttemptID)
	r.act.resp.PredecessorRuntimeInstanceID = candRT
	out := r.e.Execute(context.Background(), r.lease)
	assertHandedOffFailure(t, r.act, out, "predecessor identity equals the candidate")
	if indexOf(r.seq, "stop:pred") >= 0 || indexOf(r.seq, "ensure") >= 0 {
		t.Fatalf("nothing may be stopped or started: %v", r.seq)
	}
}

func TestSTSPredecessorLabelMismatchIsNotStopped(t *testing.T) {
	r := newSTSRig(t, true)
	r.dk.ctrs["pred-id"].labels[docker.LabelReleaseID] = "someone-else"
	out := r.e.Execute(context.Background(), r.lease)
	assertHandedOffFailure(t, r.act, out, "predecessor stop refused", "predecessor not stopped")
	if indexOf(r.seq, "stop:pred") >= 0 || indexOf(r.seq, "ensure") >= 0 || indexOf(r.seq, "start:pred") >= 0 {
		t.Fatalf("a mismatched container must be neither stopped, restarted nor replaced: %v", r.seq)
	}
}

func TestSTSPersistFailureStopsNothing(t *testing.T) {
	r := newSTSRig(t, true)
	r.act.saveErrOn = map[int]error{2: errors.New("disk full"), 3: errors.New("disk full"), 4: errors.New("disk full")}
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	if indexOf(r.seq, "stop:pred") >= 0 || !r.running("pred") {
		t.Fatalf("predecessor must keep running when the stop intent cannot be persisted: %v", r.seq)
	}
}

func TestSTSRoutedByLeaseStrategyOnly(t *testing.T) {
	r := newSTSRig(t, true)
	r.lease.ReleaseStrategy = "blue_green"
	r.e.Execute(context.Background(), r.lease)
	if indexOf(r.seq, "stop:pred") >= 0 {
		t.Fatalf("blue/green must not stop the predecessor: %v", r.seq)
	}
	if !reflect.DeepEqual(r.cp.progressCalls[len(r.cp.progressCalls)-1:], []deployment.Status{deployment.StatusHealthChecking}) {
		t.Fatalf("progress = %v", r.cp.progressCalls)
	}
	if want := []string{"ensure", "gate", "activate", "route-write"}; !reflect.DeepEqual(r.seq, want) {
		t.Fatalf("blue/green sequence = %v, want %v", r.seq, want)
	}
}

func TestRestartDockerSeamHasNoRemoveOrKill(t *testing.T) {
	typ := reflect.TypeOf((*RestartDocker)(nil)).Elem()
	want := map[string]bool{"InspectContainer": true, "ListContainersByLabels": true, "StopContainer": true, "WaitStopped": true, "StartContainer": true}
	if typ.NumMethod() != len(want) {
		t.Fatalf("seam has %d methods, want %d", typ.NumMethod(), len(want))
	}
	for i := 0; i < typ.NumMethod(); i++ {
		if name := typ.Method(i).Name; !want[name] {
			t.Fatalf("unexpected seam method %q", name)
		}
	}
}

func TestRestartPredecessorGuards(t *testing.T) {
	predRT := dockerid.AttemptRuntimeInstanceID(41)
	target := stopTarget{EnvironmentID: "env1", ReleaseID: stsPredRelease, RuntimeInstanceID: predRT}
	mk := func() *stsDocker {
		d := &stsDocker{ctrs: map[string]*stsCtr{}, stopErr: map[string]error{}, startErr: map[string]error{}}
		d.add("pred-id", "env1", stsPredRelease, predRT, false)
		return d
	}
	d := mk()
	if res := restartPredecessor(context.Background(), d, target); res.Status != restartStarted || !d.ctrs["pred-id"].running {
		t.Fatalf("res = %+v", res)
	}
	d = mk()
	d.ctrs["pred-id"].labels[docker.LabelEnvironmentID] = "other"
	if res := restartPredecessor(context.Background(), d, target); res.Status != restartMismatch || indexOf(d.events, "start:pred-id") >= 0 || len(d.events) != 0 {
		t.Fatalf("res = %+v events=%v", res, d.events)
	}
	d = mk()
	t2 := target
	t2.ForbidRuntimeInstanceID = predRT
	if res := restartPredecessor(context.Background(), d, t2); res.Status != restartMismatch || len(d.events) != 0 {
		t.Fatalf("forbidden target must be refused: %+v", res)
	}
	if res := restartPredecessor(context.Background(), mk(), stopTarget{EnvironmentID: "env1"}); res.Status != restartInvalid {
		t.Fatalf("res = %+v", res)
	}
	d = &stsDocker{ctrs: map[string]*stsCtr{}}
	if res := restartPredecessor(context.Background(), d, target); res.Status != restartGone {
		t.Fatalf("res = %+v", res)
	}
}

// --- recovery ---

func stsStranded(t *testing.T, flag bool) (state.PendingActivation, *stsDocker) {
	t.Helper()
	candRT, predRT := dockerid.AttemptRuntimeInstanceID(42), dockerid.AttemptRuntimeInstanceID(41)
	rec := authorizedRec(42, "env_a")
	rec.ReleaseID, rec.BackendRuntimeInstanceID = "relNew", candRT
	rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID = stsPredRelease, predRT
	rec.DrainSeconds = 30
	rec.PredecessorStopSent = flag
	d := &stsDocker{ctrs: map[string]*stsCtr{}, candID: "cand-id", stopErr: map[string]error{}, startErr: map[string]error{}}
	return rec, d
}

func TestRecoveryRestartsPredecessorAtEachCrashPoint(t *testing.T) {
	absent := func(string, string) (string, bool, error) { return "", false, nil }
	for name, setup := range map[string]func(rec state.PendingActivation, d *stsDocker){
		"crash before the stop was sent": func(rec state.PendingActivation, d *stsDocker) {
			d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, true)
		},
		"crash after the stop": func(rec state.PendingActivation, d *stsDocker) {
			d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, false)
		},
		"crash after the candidate started": func(rec state.PendingActivation, d *stsDocker) {
			d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, false)
			d.add("cand-id", rec.EnvironmentID, rec.ReleaseID, rec.BackendRuntimeInstanceID, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec, d := stsStranded(t, true)
			setup(rec, d)
			f := &recoveryFake{records: []state.PendingActivation{rec}}
			if n := recoverActivationsWith(f, "/routes", absent, testLogger(), d); n != 1 {
				t.Fatalf("converted=%d", n)
			}
			got := f.saved[0]
			for _, w := range []string{"predecessor stop intent was recorded", "predecessor restart ok"} {
				if !strings.Contains(got.FailureSummary, w) {
					t.Fatalf("summary %q lacks %q", got.FailureSummary, w)
				}
			}
			if !got.PredecessorStopSent || got.Succeeded {
				t.Fatalf("record = %+v", got)
			}
			if !d.ctrs["pred-id"].running {
				t.Fatal("predecessor not running after recovery")
			}
			if c := d.ctrs["cand-id"]; c != nil && c.running {
				t.Fatal("candidate still running next to the restarted predecessor")
			}
			if d.ctrs["cand-id"] != nil {
				if sc, sp := indexOf(d.events, "stop:cand"), indexOf(d.events, "start:pred"); sc < 0 || sp < sc {
					t.Fatalf("events = %v, want stop:cand before start:pred", d.events)
				}
			}
		})
	}
}

func TestRecoveryLeavesContainersWhenRouteMayPointAtCandidate(t *testing.T) {
	for name, read := range map[string]func(rec state.PendingActivation) func(string, string) (string, bool, error){
		"on-disk equals authorized": func(rec state.PendingActivation) func(string, string) (string, bool, error) {
			return func(string, string) (string, bool, error) { return routeDigestString(rec.RouteDigest), true, nil }
		},
		"unreadable": func(state.PendingActivation) func(string, string) (string, bool, error) {
			return func(string, string) (string, bool, error) { return "", false, errors.New("EIO") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec, d := stsStranded(t, true)
			d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, false)
			d.add("cand-id", rec.EnvironmentID, rec.ReleaseID, rec.BackendRuntimeInstanceID, true)
			f := &recoveryFake{records: []state.PendingActivation{rec}}
			recoverActivationsWith(f, "/routes", read(rec), testLogger(), d)
			if len(d.events) != 0 {
				t.Fatalf("containers touched: %v", d.events)
			}
			if !strings.Contains(f.saved[0].FailureSummary, "left as found") {
				t.Fatalf("summary = %q", f.saved[0].FailureSummary)
			}
		})
	}
}

func TestRecoveryWithoutDockerOrWithoutFlagDoesNotRestart(t *testing.T) {
	absent := func(string, string) (string, bool, error) { return "", false, nil }
	rec, d := stsStranded(t, true)
	d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, false)
	f := &recoveryFake{records: []state.PendingActivation{rec}}
	recoverActivationsWith(f, "/routes", absent, testLogger(), nil)
	if !strings.Contains(f.saved[0].FailureSummary, "predecessor restart not attempted") {
		t.Fatalf("summary = %q", f.saved[0].FailureSummary)
	}

	rec, d = stsStranded(t, false) // crash before the intent was persisted: host untouched
	d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, true)
	f = &recoveryFake{records: []state.PendingActivation{rec}}
	recoverActivationsWith(f, "/routes", absent, testLogger(), d)
	if len(d.events) != 0 || strings.Contains(f.saved[0].FailureSummary, "predecessor stop intent was recorded") {
		t.Fatalf("a record without the flag must not touch containers: events=%v summary=%q", d.events, f.saved[0].FailureSummary)
	}
}

func TestRecoveryDoesNotRestartWhenCandidateCannotBeStopped(t *testing.T) {
	absent := func(string, string) (string, bool, error) { return "", false, nil }
	rec, d := stsStranded(t, true)
	d.add("pred-id", rec.EnvironmentID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, false)
	d.add("cand-id", rec.EnvironmentID, rec.ReleaseID, rec.BackendRuntimeInstanceID, true)
	d.stopErr["cand-id"] = errors.New("stuck")
	f := &recoveryFake{records: []state.PendingActivation{rec}}
	recoverActivationsWith(f, "/routes", absent, testLogger(), d)
	if indexOf(d.events, "start:pred") >= 0 || !strings.Contains(f.saved[0].FailureSummary, "candidate could NOT be stopped") {
		t.Fatalf("events=%v summary=%q", d.events, f.saved[0].FailureSummary)
	}
}

// directRelease builds the stsRelease and authorized record that
// activateWith would hand to beforeSwitch, so a test can pick the window.
func (r *stsRig) directRelease() (*stsRelease, *state.PendingActivation) {
	rec := authorizedRec(r.lease.AttemptID, r.lease.EnvironmentID)
	rec.ReleaseID = r.lease.ReleaseID
	rec.BackendRuntimeInstanceID = dockerid.AttemptRuntimeInstanceID(r.lease.AttemptID)
	rec.PredecessorReleaseID = r.act.resp.PredecessorReleaseID
	rec.PredecessorRuntimeInstanceID = r.act.resp.PredecessorRuntimeInstanceID
	rec.DrainSeconds = 30
	identity := docker.ReleaseResourceIdentity{
		EnvironmentID: r.lease.EnvironmentID, ReleaseID: r.lease.ReleaseID,
		RuntimeInstanceID: rec.BackendRuntimeInstanceID,
	}
	return &stsRelease{e: r.e, lease: r.lease, identity: identity}, &rec
}

func TestSTSExpiredWindowStillRestartsPredecessor(t *testing.T) {
	r := newSTSRig(t, true)
	rel, rec := r.directRelease()
	// The window ends while the predecessor stop is in flight: the stop lands
	// (container down) but its response is cut off.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.dk.hook = func(ev string) {
		r.seq = append(r.seq, ev)
		if ev == "stop:pred" {
			cancel()
		}
	}
	summary := rel.beforeSwitch(ctx, rec)
	if !strings.Contains(summary, "predecessor restart ok") {
		t.Fatalf("summary = %q", summary)
	}
	if !r.running("pred") {
		t.Fatal("an expired authorized window left the predecessor down")
	}
	if indexOf(r.seq, "ensure") >= 0 {
		t.Fatalf("candidate must not start after the window expired: %v", r.seq)
	}
}

func TestSTSWindowTooShortToSwitchRollsBack(t *testing.T) {
	r := newSTSRig(t, true)
	rel, rec := r.directRelease()
	// ProbeTimeout is 1s in the rig; the rule needs probe + verify + margin.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary := rel.beforeSwitch(ctx, rec)
	for _, w := range []string{"authorized window too short", "predecessor restart ok", "candidate stopped"} {
		if !strings.Contains(summary, w) {
			t.Fatalf("summary %q lacks %q", summary, w)
		}
	}
	if len(r.act.writes) != 0 || r.running("cand") || !r.running("pred") {
		t.Fatalf("route written=%d cand=%v pred=%v", len(r.act.writes), r.running("cand"), r.running("pred"))
	}
	// A comfortable window passes the gate.
	r2 := newSTSRig(t, true)
	rel2, rec2 := r2.directRelease()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if s := rel2.beforeSwitch(ctx2, rec2); s != "" {
		t.Fatalf("unexpected failure: %q", s)
	}
}

func TestSTSSwitchFailureWithUnreadableDigestLeavesContainers(t *testing.T) {
	r := newSTSRig(t, true)
	r.act.probeErr = errors.New("502")
	r.act.readErr = errors.New("EIO")
	out := r.e.Execute(context.Background(), r.lease)
	assertHandedOffFailure(t, r.act, out, "left as found", "on-disk route digest: unreadable")
	if !r.running("cand") || r.running("pred") || indexOf(r.seq, "start:pred") >= 0 {
		t.Fatalf("containers must be left as found: %v", r.seq)
	}
}

func TestSTSRollbackOutcomeSurvivesTruncation(t *testing.T) {
	r := newSTSRig(t, true)
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: strings.Repeat("x", 5000)}
	out := r.e.Execute(context.Background(), r.lease)
	rec := assertHandedOffFailure(t, r.act, out, "predecessor restart ok", "authorized route digest: sha256:")
	if strings.Index(rec.FailureSummary, "predecessor restart ok") > strings.Index(rec.FailureSummary, "candidate failed health gate") {
		t.Fatalf("rollback outcome must lead the reason: %q", rec.FailureSummary)
	}
}

func TestRestartPredecessorAlreadyRunningIsNotStarted(t *testing.T) {
	predRT := dockerid.AttemptRuntimeInstanceID(41)
	d := &stsDocker{ctrs: map[string]*stsCtr{}}
	d.add("pred-id", "env1", stsPredRelease, predRT, true)
	res := restartPredecessor(context.Background(), d, stopTarget{EnvironmentID: "env1", ReleaseID: stsPredRelease, RuntimeInstanceID: predRT})
	if res.Status != restartStarted || indexOf(d.events, "start:pred") >= 0 {
		t.Fatalf("res=%+v events=%v", res, d.events)
	}
}
