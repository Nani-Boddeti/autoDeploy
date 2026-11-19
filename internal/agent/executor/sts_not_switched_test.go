package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/healthgate"
	"autodeploy/internal/runtime/docker"
)

// Slice 22-D: the stop-then-start not_switched outcome (ADR 0005). Every
// negative below isolates ONE unmet condition of notSwitchedProof (or of the
// surrounding decision) against a rig where everything else holds.

var (
	preStopDigestString = "sha256:" + strings.Repeat("1", 64)
	movedDigestString   = "sha256:" + strings.Repeat("2", 64)
)

// notSwitchedRig is an opted-in stop-then-start rig whose route file holds the
// predecessor's route before the attempt and whose candidate gate fails.
func notSwitchedRig(t *testing.T) *stsRig {
	t.Helper()
	r := newSTSRig(t, true)
	r.lease.ActivationNotSwitched = true
	r.act.disk = preStopDigestString
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: "status=503"}
	return r
}

func runNotSwitched(t *testing.T, r *stsRig) state.PendingActivation {
	t.Helper()
	out := r.e.Execute(context.Background(), r.lease)
	return assertHandedOffFailure(t, r.act, out)
}

func TestSTSNotSwitchedPositive(t *testing.T) {
	cases := map[string]func(*stsRig){
		"gate fails":  func(*stsRig) {},
		"gate errors": func(r *stsRig) { r.dk.gate = docker.HealthGateResult{}; r.dk.gateErr = errors.New("engine hiccup") },
		"start fails": func(r *stsRig) { r.dk.ensureErr = docker.ErrInconsistentLocalState },
		"window closed": func(r *stsRig) {
			r.dk.gate = docker.HealthGateResult{Success: true}
			r.e.activation.cfg.ProbeTimeout = 20 * time.Minute
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			r := notSwitchedRig(t)
			set(r)
			rec := runNotSwitched(t, r)
			if !rec.NotSwitched {
				t.Fatalf("record = %+v, want not_switched (summary %q)", rec, rec.FailureSummary)
			}
			if want := mustParseRoute(t, preStopDigestString); rec.PreStopRouteDigest != want {
				t.Fatalf("pre-stop digest = %x, want the on-disk digest read before the stop", rec.PreStopRouteDigest)
			}
			if !strings.Contains(rec.FailureSummary, "predecessor health probe ok") || !strings.Contains(rec.FailureSummary, "route unchanged") {
				t.Fatalf("summary %q lacks the probe evidence", rec.FailureSummary)
			}
			if r.running("cand") || !r.running("pred") {
				t.Fatal("candidate must be stopped and the predecessor serving")
			}
			// Order: candidate stopped, predecessor restarted, THEN probed.
			stopCand, startPred, probe := indexOf(r.seq, "stop:cand"), indexOf(r.seq, "start:pred"), indexOf(r.seq, "probe:pred")
			if name != "start fails" && (stopCand < 0 || startPred < stopCand) || probe < startPred || probe < 0 {
				t.Fatalf("sequence = %v, want candidate stop < predecessor restart < probe", r.seq)
			}
			if r.act.probeN != 0 || len(r.act.writes) != 0 {
				t.Fatal("route must not be touched")
			}
		})
	}
}

func mustParseRoute(t *testing.T, s string) [32]byte {
	t.Helper()
	d, ok := parseRouteDigestString(s)
	if !ok {
		t.Fatalf("bad digest %q", s)
	}
	return d
}

func TestSTSNotSwitchedNegatives(t *testing.T) {
	cases := map[string]func(t *testing.T, r *stsRig){
		"lease echo false": func(_ *testing.T, r *stsRig) { r.lease.ActivationNotSwitched = false },
		"predecessor probe fails": func(_ *testing.T, r *stsRig) {
			r.dk.predGate = &docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: "status=503"}
		},
		"predecessor probe errors": func(_ *testing.T, r *stsRig) { r.dk.predGateErr = errors.New("engine hiccup") },
		"predecessor probe times out": func(t *testing.T, r *stsRig) {
			old := predecessorProbeBound
			predecessorProbeBound = func(healthgate.Policy) time.Duration { return 100 * time.Millisecond }
			t.Cleanup(func() { predecessorProbeBound = old })
			r.dk.predGateBlock = true
		},
		"predecessor restart fails": func(_ *testing.T, r *stsRig) { r.dk.startErr["pred-id"] = errors.New("no such image") },
		"candidate cannot be stopped": func(_ *testing.T, r *stsRig) {
			r.dk.stopErr["cand-id"] = errors.New("stuck")
		},
		"stop mismatch": func(_ *testing.T, r *stsRig) { r.dk.ctrs["pred-id"].labels[docker.LabelReleaseID] = "someone-else" },
		"predecessor absent": func(t *testing.T, r *stsRig) {
			*r = *newSTSRig(t, false)
			r.lease.ActivationNotSwitched = true
			r.act.disk = preStopDigestString
			r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded}
		},
		"predecessor already stopped": func(_ *testing.T, r *stsRig) { r.dk.ctrs["pred-id"].running = false },
		"route digest changed": func(_ *testing.T, r *stsRig) {
			r.dk.onGate = func(context.Context) { r.act.disk = movedDigestString }
		},
		"route digest unreadable": func(_ *testing.T, r *stsRig) {
			r.dk.onGate = func(context.Context) { r.act.readErr = errors.New("EIO") }
		},
		"route file removed": func(_ *testing.T, r *stsRig) {
			r.dk.onGate = func(context.Context) { r.act.disk = "" }
		},
		"route digest unreadable before the stop": func(_ *testing.T, r *stsRig) { r.act.readErr = errors.New("EIO") },
		"revision file at the authorized revision": func(_ *testing.T, r *stsRig) {
			r.act.stored = r.act.resp.RouteRevision
		},
		"revision file above the authorized revision": func(_ *testing.T, r *stsRig) {
			r.act.stored = r.act.resp.RouteRevision + 4
		},
		"revision file unreadable": func(_ *testing.T, r *stsRig) { r.act.loadErr = errors.New("EIO") },
		"switch entered: data plane probe fails": func(_ *testing.T, r *stsRig) {
			r.dk.gate = docker.HealthGateResult{Success: true}
			r.act.probeErr = errors.New("502")
		},
		"switch entered: before the route write": func(_ *testing.T, r *stsRig) {
			r.dk.gate = docker.HealthGateResult{Success: true}
			r.act.recordErr = errors.New("disk full")
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			r := notSwitchedRig(t)
			set(t, r)
			rec := runNotSwitched(t, r)
			if rec.NotSwitched {
				t.Fatalf("claimed not_switched: %q", rec.FailureSummary)
			}
		})
	}
}

// A predecessor probe that cannot answer within the bound must not claim the
// outcome, and must return (the bound is real, not a hang).
func TestSTSNotSwitchedProbeIsBounded(t *testing.T) {
	old := predecessorProbeBound
	predecessorProbeBound = func(healthgate.Policy) time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() { predecessorProbeBound = old })
	r := notSwitchedRig(t)
	r.dk.predGateBlock = true
	done := make(chan state.PendingActivation, 1)
	go func() { done <- runNotSwitched(t, r) }()
	select {
	case rec := <-done:
		if rec.NotSwitched || !strings.Contains(rec.FailureSummary, "predecessor health probe failed") {
			t.Fatalf("record = %+v", rec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the predecessor probe is not bounded")
	}
}

func TestSTSNotSwitchedProbeBoundFollowsThePolicyDeadline(t *testing.T) {
	if got, want := predecessorProbeBound(healthgate.Policy{}), 60*time.Second+predecessorProbeSlack; got != want {
		t.Fatalf("default bound = %s, want %s", got, want)
	}
	p := healthgate.DefaultPolicy()
	p.DeadlineSeconds = 300
	if got, want := predecessorProbeBound(p), 300*time.Second+predecessorProbeSlack; got != want {
		t.Fatalf("tuned bound = %s, want %s", got, want)
	}
}

// Not opted in: the record keeps exactly the pre-22-D shape (no snapshot).
func TestSTSNotOptedInTakesNoSnapshot(t *testing.T) {
	r := notSwitchedRig(t)
	r.lease.ActivationNotSwitched = false
	rec := runNotSwitched(t, r)
	if rec.PreStopRouteDigest != ([32]byte{}) || rec.NotSwitched {
		t.Fatalf("record = %+v", rec)
	}
	if indexOf(r.seq, "probe:pred") >= 0 {
		t.Fatal("no predecessor probe without the opt-in")
	}
}

// The stop intent cannot be persisted: nothing is stopped, plain failure.
func TestSTSNotSwitchedStopIntentPersistFailure(t *testing.T) {
	r := notSwitchedRig(t)
	r.act.saveErrOn = map[int]error{2: errors.New("disk full"), 3: errors.New("disk full"), 4: errors.New("disk full")}
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	for _, rec := range r.act.saved {
		if rec.NotSwitched {
			t.Fatalf("record %+v claims not_switched", rec)
		}
	}
	if indexOf(r.seq, "stop:pred") >= 0 || indexOf(r.seq, "probe:pred") >= 0 {
		t.Fatalf("sequence = %v", r.seq)
	}
}

// The result record cannot be persisted: the direct report is a plain failure.
func TestSTSNotSwitchedResultPersistFailureReportsPlain(t *testing.T) {
	r := notSwitchedRig(t)
	r.act.saveErrFrom = 3 // 1 authorized, 2 stop intent, 3.. result
	out := r.e.Execute(context.Background(), r.lease)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	if len(r.act.reports) == 0 {
		t.Fatal("expected a direct report")
	}
	for _, rep := range r.act.reports {
		if rep.result.NotSwitched() {
			t.Fatal("a persist failure must never send the not_switched outcome")
		}
	}
	for _, rec := range r.act.saved {
		if rec.NotSwitched {
			t.Fatalf("record %+v claims not_switched", rec)
		}
	}
}

// A retained (rollback) stop-then-start job never claims the outcome, even if
// its lease somehow carried the echo and every other proof held.
func TestSTSNotSwitchedRollbackJobNeverClaims(t *testing.T) {
	r := newRollbackRig(t, "stop_then_start", true, false)
	r.lease.ActivationNotSwitched = true
	r.act.disk = preStopDigestString
	r.dk.gate = docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded}
	out := r.e.Execute(context.Background(), r.lease)
	rec := assertHandedOffFailure(t, r.act, out, "predecessor restart ok")
	if rec.NotSwitched {
		t.Fatalf("rollback job claimed not_switched: %q", rec.FailureSummary)
	}
}

// A record recovered after an agent restart never claims the outcome, even one
// that carries a pre-stop snapshot (the proofs lived in the dead process).
func TestRecoverActivationsNeverClaimsNotSwitched(t *testing.T) {
	rec := authorizedRec(7, "env_a")
	rec.ReleaseID, rec.BackendRuntimeInstanceID = "relNew", "rt-new"
	rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, rec.DrainSeconds = "relOld", "rt-old", 30
	rec.PredecessorStopSent = true
	rec.PreStopRouteDigest[0] = 0x11
	f := &recoveryFake{records: []state.PendingActivation{rec}}
	read := func(string, string) (string, bool, error) { return preStopDigestString, true, nil }
	if n := recoverActivations(f, "/routes", read, nil); n != 1 {
		t.Fatalf("converted=%d", n)
	}
	got := f.saved[0]
	if got.NotSwitched || got.Phase != state.PhaseResultReady || got.Succeeded {
		t.Fatalf("recovered record = %+v", got)
	}
}

var _ = agentclient.Lease{}

func allProven() notSwitchedFacts {
	return notSwitchedFacts{
		leaseOptedIn: true, notRetained: true, hasPredecessor: true, stopSent: true, stopConfirmed: true,
		candidateStopped: true, predecessorUp: true, snapshotTaken: true, predecessorProbe: true,
		revisionBelow: true, routeUnchanged: true,
	}
}

// Each fact is a veto on its own: with every other fact true, flipping just
// one must refuse the claim.
func TestNotSwitchedFactsEachFactVetoes(t *testing.T) {
	if !allProven().provable() {
		t.Fatal("all facts proven must be provable")
	}
	flips := map[string]func(*notSwitchedFacts){
		"leaseOptedIn":     func(f *notSwitchedFacts) { f.leaseOptedIn = false },
		"notRetained":      func(f *notSwitchedFacts) { f.notRetained = false },
		"hasPredecessor":   func(f *notSwitchedFacts) { f.hasPredecessor = false },
		"stopSent":         func(f *notSwitchedFacts) { f.stopSent = false },
		"stopConfirmed":    func(f *notSwitchedFacts) { f.stopConfirmed = false },
		"candidateStopped": func(f *notSwitchedFacts) { f.candidateStopped = false },
		"predecessorUp":    func(f *notSwitchedFacts) { f.predecessorUp = false },
		"snapshotTaken":    func(f *notSwitchedFacts) { f.snapshotTaken = false },
		"predecessorProbe": func(f *notSwitchedFacts) { f.predecessorProbe = false },
		"revisionBelow":    func(f *notSwitchedFacts) { f.revisionBelow = false },
		"routeUnchanged":   func(f *notSwitchedFacts) { f.routeUnchanged = false },
	}
	for name, flip := range flips {
		f := allProven()
		flip(&f)
		if f.provable() {
			t.Errorf("%s=false still provable", name)
		}
	}
	// cheap() gates the slow probe: it must not need the I/O facts, and any
	// cheap fact missing must skip the probe.
	f := allProven()
	f.predecessorProbe, f.revisionBelow, f.routeUnchanged = false, false, false
	if !f.cheap() {
		t.Error("cheap() must not depend on the probe or disk facts")
	}
	for name, flip := range flips {
		if name == "predecessorProbe" || name == "revisionBelow" || name == "routeUnchanged" {
			continue
		}
		f := allProven()
		flip(&f)
		if f.cheap() {
			t.Errorf("cheap() ignores %s", name)
		}
	}
}

// The probe only runs for a failure that could still qualify.
func TestSTSNotSwitchedProbeSkippedWhenCheapFactsFail(t *testing.T) {
	r := notSwitchedRig(t)
	r.dk.startErr["pred-id"] = errors.New("no such image")
	runNotSwitched(t, r)
	if indexOf(r.seq, "probe:pred") >= 0 {
		t.Fatalf("probe ran although the predecessor restart failed: %v", r.seq)
	}
}

// resultFromRecord (the direct report after a result-persist failure) never
// carries the outcome, even for a record that holds it.
func TestResultFromRecordNeverCarriesNotSwitched(t *testing.T) {
	rec := state.PendingActivation{Phase: state.PhaseResultReady, FailureSummary: "x", NotSwitched: true}
	if resultFromRecord(rec).NotSwitched() {
		t.Fatal("direct report must be a plain failure")
	}
}

// ctxStrictDocker makes the predecessor probe honour its context, as the real
// engine client does: a probe on a cancelled context fails instead of passing.
type ctxStrictDocker struct{ *stsDocker }

func (d *ctxStrictDocker) RunHealthGate(ctx context.Context, id, net string, rev int, p healthgate.Policy, t docker.HealthGateTarget) (docker.HealthGateResult, error) {
	if id == "pred-id" {
		if err := ctx.Err(); err != nil {
			d.rec("probe:pred")
			return docker.HealthGateResult{Reason: docker.HealthGateFailureContextCancelled}, err
		}
	}
	return d.stsDocker.RunHealthGate(ctx, id, net, rev, p, t)
}

// The authorized window (the deadline-bound context the switch runs under)
// expires during the candidate gate, so the context reaching the predecessor
// probe is already done. The probe runs on a detached context, so the proof
// still runs, passes, and the outcome is not_switched. Without WithoutCancel
// in probePredecessor the probe fails on the expired parent.
func TestSTSNotSwitchedProbeSurvivesExpiredWindowContext(t *testing.T) {
	r := notSwitchedRig(t)
	// Window = 100ms: the lease's extra window is subtracted back out.
	r.e.activation.timeout = 100*time.Millisecond - stsWindowExtra(r.lease.HealthPolicy)
	var expired bool
	r.dk.onGate = func(ctx context.Context) {
		select {
		case <-ctx.Done():
			expired = true
		case <-time.After(10 * time.Second):
		}
	}
	r.e.docker = &ctxStrictDocker{stsDocker: r.dk}
	out := r.e.Execute(context.Background(), r.lease)
	rec := assertHandedOffFailure(t, r.act, out)
	if !expired {
		t.Fatal("the authorized window did not expire before the probe")
	}
	if indexOf(r.seq, "probe:pred") < 0 {
		t.Fatalf("sequence = %v, want the predecessor probe to run", r.seq)
	}
	if !rec.NotSwitched || !strings.Contains(rec.FailureSummary, "predecessor health probe ok") {
		t.Fatalf("record = %+v, want not_switched with a passing probe (summary %q)", rec, rec.FailureSummary)
	}
}
