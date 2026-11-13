package executor

import (
	"context"
	"errors"
	"reflect"
	"testing"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
	"autodeploy/internal/runtime/dockerid"
)

// candidateRig wires an Executor whose candidate starts fine and whose Docker
// fake answers the guarded-stop lookups with the exact candidate.
type candidateRig struct {
	e    *Executor
	dk   *fakeDocker
	act  *actFake
	lead agentclient.Lease
}

func newCandidateRig(t *testing.T) *candidateRig {
	t.Helper()
	lease := testLease()
	runtimeID := dockerid.AttemptRuntimeInstanceID(lease.AttemptID)
	dk := &fakeDocker{
		ensureResult: testInspect("networkabc123"),
		gateResult:   docker.HealthGateResult{Success: true},
		listResult: []docker.ContainerSummary{{ID: "cand-inspected", Labels: map[string]string{
			docker.LabelManaged:           "true",
			docker.LabelKind:              docker.KindReleaseRuntimeContainer,
			docker.LabelEnvironmentID:     lease.EnvironmentID,
			docker.LabelReleaseID:         lease.ReleaseID,
			docker.LabelRuntimeInstanceID: runtimeID,
		}}},
	}
	dk.inspectResult.ID = "cand-inspected"
	dk.inspectResult.State.Running = true
	cp := &fakeControlPlane{secrets: fullSecrets()}
	co := &fakeCheckout{result: happyCheckoutResult()}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: testImageDigest, ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)
	act := newActFake(lease)
	e.activation = act.runner()
	return &candidateRig{e: e, dk: dk, act: act, lead: lease}
}

func (r *candidateRig) assertStopped(t *testing.T) {
	t.Helper()
	name, _ := docker.ContainerName(dockerid.AttemptRuntimeInstanceID(r.lead.AttemptID))
	if !reflect.DeepEqual(r.dk.inspectNames, []string{name}) {
		t.Fatalf("inspected %v, want exactly the AutoDeploy name %q", r.dk.inspectNames, name)
	}
	if len(r.dk.listLabels) != 1 {
		t.Fatalf("label lookups = %d, want 1", len(r.dk.listLabels))
	}
	want := map[string]string{
		docker.LabelManaged:           "true",
		docker.LabelKind:              docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID:     r.lead.EnvironmentID,
		docker.LabelReleaseID:         r.lead.ReleaseID,
		docker.LabelRuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(r.lead.AttemptID),
	}
	if !reflect.DeepEqual(r.dk.listLabels[0], want) {
		t.Fatalf("label selector = %v, want %v", r.dk.listLabels[0], want)
	}
	if !reflect.DeepEqual(r.dk.stoppedIDs, []string{"cand-inspected"}) || !reflect.DeepEqual(r.dk.waitedIDs, []string{"cand-inspected"}) {
		t.Fatalf("stopped=%v waited=%v, want the inspected id only", r.dk.stoppedIDs, r.dk.waitedIDs)
	}
}

func (r *candidateRig) assertNotStopped(t *testing.T) {
	t.Helper()
	if len(r.dk.stoppedIDs) != 0 || len(r.dk.waitedIDs) != 0 {
		t.Fatalf("stopped=%v waited=%v, want no stop", r.dk.stoppedIDs, r.dk.waitedIDs)
	}
}

func TestCandidateStoppedOnHealthGateFailure(t *testing.T) {
	r := newCandidateRig(t)
	r.dk.gateResult = docker.HealthGateResult{Success: false, Reason: docker.HealthGateFailureDeadlineExceeded}
	out := r.e.Execute(context.Background(), r.lead)
	if out.Succeeded || out.FailureClass != queue.FailureClassPermanentConfiguration {
		t.Fatalf("outcome = %+v", out)
	}
	r.assertStopped(t)
}

func TestCandidateStoppedOnHealthGateError(t *testing.T) {
	r := newCandidateRig(t)
	r.dk.gateErr = errors.New("engine hiccup")
	out := r.e.Execute(context.Background(), r.lead)
	if out.Succeeded || out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	r.assertStopped(t)
}

func TestCandidateStoppedOnPreAuthorizationRefusal(t *testing.T) {
	r := newCandidateRig(t)
	r.act.activateErrs = []error{&agentclient.ConflictError{Code: agentclient.ConflictCandidateNotReady}}
	out := r.e.Execute(context.Background(), r.lead)
	if out.Succeeded || out.HandedOff || out.AuthorizationUnknown || out.FailureClass != queue.FailureClassTransient {
		t.Fatalf("outcome = %+v", out)
	}
	r.assertStopped(t)
}

func TestCandidateStoppedOnTraefikPreflightFailure(t *testing.T) {
	r := newCandidateRig(t)
	r.act.preflightErr = errors.New("traefik api down")
	out := r.e.Execute(context.Background(), r.lead)
	if out.Succeeded || out.HandedOff || out.AuthorizationUnknown {
		t.Fatalf("outcome = %+v", out)
	}
	r.assertStopped(t)
}

func TestCandidateNotStoppedAfterAuthorization(t *testing.T) {
	r := newCandidateRig(t)
	r.act.writeErr = errors.New("disk full") // authorized, then the switch fails
	out := r.e.Execute(context.Background(), r.lead)
	if !out.HandedOff || out.Succeeded {
		t.Fatalf("outcome = %+v, want handed-off failure", out)
	}
	r.assertNotStopped(t)
	if len(r.dk.inspectNames) != 0 {
		t.Fatalf("docker consulted after authorization: %v", r.dk.inspectNames)
	}
}

func TestCandidateNotStoppedWhenAuthorizationUnknown(t *testing.T) {
	r := newCandidateRig(t)
	r.act.activateErrs = []error{agentclient.ErrServiceUnavailable, agentclient.ErrServiceUnavailable, agentclient.ErrServiceUnavailable}
	out := r.e.Execute(context.Background(), r.lead)
	if !out.AuthorizationUnknown {
		t.Fatalf("outcome = %+v, want authorization unknown", out)
	}
	r.assertNotStopped(t)
}

func TestCandidateNotStoppedOnSuccess(t *testing.T) {
	r := newCandidateRig(t)
	out := r.e.Execute(context.Background(), r.lead)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v, want handed off", out)
	}
	r.assertNotStopped(t)
}

func TestCandidateNotStoppedWhenStartFailed(t *testing.T) {
	r := newCandidateRig(t)
	r.dk.ensureErr = docker.ErrInconsistentLocalState
	r.e.Execute(context.Background(), r.lead)
	if len(r.dk.inspectNames) != 0 {
		t.Fatalf("stop attempted although no candidate was started: %v", r.dk.inspectNames)
	}
}

func TestCandidateIdentityMismatchIsNotStopped(t *testing.T) {
	for name, mutate := range map[string]func(*candidateRig){
		"wrong release label": func(r *candidateRig) { r.dk.listResult[0].Labels[docker.LabelReleaseID] = "other" },
		"no label match":      func(r *candidateRig) { r.dk.listResult = nil },
		"different id":        func(r *candidateRig) { r.dk.listResult[0].ID = "someone-else" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newCandidateRig(t)
			r.dk.gateResult = docker.HealthGateResult{Success: false, Reason: docker.HealthGateFailureDeadlineExceeded}
			mutate(r)
			out := r.e.Execute(context.Background(), r.lead)
			if out.Succeeded || out.FailureClass != queue.FailureClassPermanentConfiguration {
				t.Fatalf("outcome = %+v, want unchanged health-gate class", out)
			}
			r.assertNotStopped(t)
		})
	}
}

func TestCandidateStopErrorDoesNotChangeOutcome(t *testing.T) {
	for name, set := range map[string]func(*fakeDocker){
		"stop fails": func(d *fakeDocker) { d.stopErr = errors.New("engine refused") },
		"wait fails": func(d *fakeDocker) { d.waitErr = errors.New("still running") },
		"inspect":    func(d *fakeDocker) { d.inspectErr = errors.New("socket gone") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newCandidateRig(t)
			r.dk.gateResult = docker.HealthGateResult{Success: false, Reason: docker.HealthGateFailureDeadlineExceeded}
			set(r.dk)
			out := r.e.Execute(context.Background(), r.lead)
			if out.Succeeded || out.FailureClass != queue.FailureClassPermanentConfiguration {
				t.Fatalf("outcome = %+v, want unchanged health-gate class", out)
			}
		})
	}
}

func TestCandidateStoppedEvenWhenLeaseContextCancelled(t *testing.T) {
	r := newCandidateRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.dk.blockGate = true
	r.dk.onGate = cancel // cancel the lease exactly when the gate runs
	out := r.e.Execute(ctx, r.lead)
	if out.Succeeded {
		t.Fatalf("outcome = %+v", out)
	}
	r.assertStopped(t)
}

func TestGuardedStopRefusesIncompleteIdentity(t *testing.T) {
	dk := &fakeDocker{}
	res := guardedStop(context.Background(), dk, stopTarget{EnvironmentID: "env1", ReleaseID: "", RuntimeInstanceID: "42"}, 0, 0, 0)
	if res.Status != stopInvalid || len(dk.inspectNames) != 0 || len(dk.listLabels) != 0 {
		t.Fatalf("res=%+v inspected=%v", res, dk.inspectNames)
	}
}
