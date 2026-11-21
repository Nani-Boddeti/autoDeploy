package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

// Slice 23-F precondition (ADR 0004 amendment 2026-11-21): the control plane
// vacates an expired slot without the reconciliation block while the
// deployment is at or before building, so no path may create or start a
// container unless the starting report was accepted under the current fence.

var errProgressRefused = errors.New("progress refused")

func TestPhaseRequiresAcceptanceIsStartingOnly(t *testing.T) {
	for phase, want := range map[deployment.Status]bool{
		deployment.StatusFetching:       false,
		deployment.StatusBuilding:       false,
		deployment.StatusStarting:       true,
		deployment.StatusHealthChecking: false,
	} {
		if got := phaseRequiresAcceptance(phase); got != want {
			t.Errorf("phaseRequiresAcceptance(%s) = %t, want %t", phase, got, want)
		}
	}
}

func TestRefusedStartingProgressNeverCreatesContainerBlueGreen(t *testing.T) {
	cp := &fakeControlPlane{secrets: fullSecrets(), progressErrOn: map[deployment.Status]error{deployment.StatusStarting: errProgressRefused}}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)
	out := e.Execute(context.Background(), testLease())
	if out.Succeeded || out.HandedOff || out.FailureClass != queue.FailureClassTransient {
		t.Fatalf("outcome = %+v, want a transient failure", out)
	}
	if dk.ensureN != 0 || dk.gateN != 0 || len(dk.startedIDs) != 0 {
		t.Fatalf("container calls after a refused starting report: ensure=%d gate=%d started=%v", dk.ensureN, dk.gateN, dk.startedIDs)
	}
}

func TestRefusedStartingProgressNeverCreatesContainerStopThenStart(t *testing.T) {
	r := newSTSRig(t, true)
	r.cp.progressErrOn = map[deployment.Status]error{deployment.StatusStarting: errProgressRefused}
	out := r.e.Execute(context.Background(), r.lease)
	if out.Succeeded || out.HandedOff || out.FailureClass != queue.FailureClassTransient {
		t.Fatalf("outcome = %+v, want a transient failure", out)
	}
	if len(r.seq) != 0 || r.act.activateN != 0 || r.act.preflightN != 0 {
		t.Fatalf("engine/activation calls after a refused starting report: seq=%v activate=%d preflight=%d", r.seq, r.act.activateN, r.act.preflightN)
	}
}

func TestRefusedStartingProgressNeverStartsRetainedContainerOnRollback(t *testing.T) {
	for _, strategy := range []string{"blue_green", releaseStrategyStopThenStart} {
		t.Run(strategy, func(t *testing.T) {
			r := newRollbackRig(t, strategy, true, false)
			r.cp.progressErrOn = map[deployment.Status]error{deployment.StatusStarting: errProgressRefused}
			out := r.e.Execute(context.Background(), r.lease)
			if out.Succeeded || out.HandedOff || out.FailureClass != queue.FailureClassTransient {
				t.Fatalf("outcome = %+v, want a transient failure", out)
			}
			if len(r.seq) != 0 || r.act.activateN != 0 {
				t.Fatalf("engine/activation calls after a refused starting report: seq=%v activate=%d", r.seq, r.act.activateN)
			}
		})
	}
}

func TestRefusedEarlyProgressRemainsBestEffort(t *testing.T) {
	cp := &fakeControlPlane{secrets: fullSecrets(), progressErrOn: map[deployment.Status]error{
		deployment.StatusFetching: errProgressRefused, deployment.StatusBuilding: errProgressRefused,
	}}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)
	e.Execute(context.Background(), testLease())
	if dk.ensureN != 1 {
		t.Fatalf("ensure calls = %d, want 1: fetching/building reports stay best-effort", dk.ensureN)
	}
}

// --- Retry of the required starting report (availability, 23-F follow-up) ---

type scriptedProgressCP struct {
	fakeControlPlane
	startingErrs []error // one per starting call; nil entry = accept; exhausted = accept
	startingN    int
	onStarting   func()
}

func (f *scriptedProgressCP) ReportProgress(ctx context.Context, attemptID, fence int64, phase deployment.Status) error {
	if phase != deployment.StatusStarting {
		return f.fakeControlPlane.ReportProgress(ctx, attemptID, fence, phase)
	}
	f.startingN++
	if f.onStarting != nil {
		f.onStarting()
	}
	if f.startingN <= len(f.startingErrs) {
		return f.startingErrs[f.startingN-1]
	}
	return nil
}

func fastBackoff(t *testing.T, d ...time.Duration) {
	t.Helper()
	old := startingRetryBackoff
	startingRetryBackoff = d
	t.Cleanup(func() { startingRetryBackoff = old })
}

func runRetryScenario(t *testing.T, cp *scriptedProgressCP, ctx context.Context) (agentclient.Outcome, *fakeDocker) {
	t.Helper()
	cp.secrets = fullSecrets()
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(&cp.fakeControlPlane, co, dk, build)
	e.controlPlane = cp
	return e.Execute(ctx, testLease()), dk
}

func TestStartingReportRetriesTransientThenProceeds(t *testing.T) {
	fastBackoff(t, time.Millisecond, time.Millisecond)
	transient := fmt.Errorf("%w: boom", agentclient.ErrTransportFailure)
	cp := &scriptedProgressCP{startingErrs: []error{transient, agentclient.ErrServiceUnavailable}}
	_, dk := runRetryScenario(t, cp, context.Background())
	if cp.startingN != 3 || dk.ensureN != 1 {
		t.Fatalf("starting calls=%d ensure=%d, want 3 and 1", cp.startingN, dk.ensureN)
	}
}

func TestStartingReportDenialIsNotRetried(t *testing.T) {
	fastBackoff(t, time.Millisecond, time.Millisecond)
	for _, denial := range []error{agentclient.ErrStaleLease, agentclient.ErrUnauthorized, agentclient.ErrIntegrityConflict, agentclient.ErrInvalidRequest, errors.New("unrecognized")} {
		cp := &scriptedProgressCP{startingErrs: []error{denial, nil}}
		out, dk := runRetryScenario(t, cp, context.Background())
		if cp.startingN != 1 || dk.ensureN != 0 || out.Succeeded || out.HandedOff {
			t.Fatalf("denial %v: starting calls=%d ensure=%d outcome=%+v, want 1, 0, aborted", denial, cp.startingN, dk.ensureN, out)
		}
	}
}

func TestStartingReportAbortsAfterThreeTransientFailures(t *testing.T) {
	fastBackoff(t, time.Millisecond, time.Millisecond)
	tr := agentclient.ErrServiceUnavailable
	cp := &scriptedProgressCP{startingErrs: []error{tr, tr, tr, nil}}
	out, dk := runRetryScenario(t, cp, context.Background())
	if cp.startingN != 3 || dk.ensureN != 0 || out.Succeeded || out.HandedOff || out.FailureClass != queue.FailureClassTransient {
		t.Fatalf("starting calls=%d ensure=%d outcome=%+v, want 3, 0, transient abort", cp.startingN, dk.ensureN, out)
	}
}

func TestStartingReportBackoffHonoursContextCancellation(t *testing.T) {
	fastBackoff(t, time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp := &scriptedProgressCP{startingErrs: []error{agentclient.ErrTransportFailure, nil}}
	cp.onStarting = func() { go func() { time.Sleep(20 * time.Millisecond); cancel() }() }
	done := make(chan struct{})
	var dk *fakeDocker
	var out agentclient.Outcome
	go func() { out, dk = runRetryScenario(t, cp, ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return promptly after cancellation during backoff")
	}
	if cp.startingN != 1 || dk.ensureN != 0 || out.Succeeded || out.HandedOff {
		t.Fatalf("starting calls=%d ensure=%d outcome=%+v, want 1, 0, aborted", cp.startingN, dk.ensureN, out)
	}
}
