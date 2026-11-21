package executor

import (
	"context"
	"errors"
	"testing"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

type orderClient struct{ events *[]string }

func (o orderClient) ReportProgress(_ context.Context, _, _ int64, phase deployment.Status) error {
	*o.events = append(*o.events, "report:"+string(phase))
	return nil
}

func TestTrackerRecordsPhaseBeforeReporting(t *testing.T) {
	var events []string
	tr := &phaseTracker{record: func(p string) error { events = append(events, "record:"+p); return nil }}
	r := newProgressReporter(orderClient{&events})
	for _, p := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusBuilding, deployment.StatusStarting} {
		if err := tr.advance(context.Background(), r, 1, 1, p); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"record:fetching", "report:fetching", "record:building", "report:building", "record:starting", "report:starting"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

func TestTrackerRecordFailureStopsPhase(t *testing.T) {
	var events []string
	tr := &phaseTracker{record: func(string) error { return errors.New("disk full") }}
	r := newProgressReporter(orderClient{&events})
	if err := tr.advance(context.Background(), r, 1, 1, deployment.StatusStarting); err == nil {
		t.Fatal("expected an error")
	}
	if len(events) != 0 {
		t.Fatalf("phase must not be reported after a failed record: %v", events)
	}
}

// orderCP, orderDocker and orderCheckout append every side effect to one
// shared log so a test can assert that the attempt record advanced first.
type orderCP struct {
	*fakeControlPlane
	events *[]string
}

func (o orderCP) MaterializeSecrets(ctx context.Context, attemptID, fence int64) ([]agentclient.MaterializedSecret, error) {
	*o.events = append(*o.events, "env:materialize")
	return o.fakeControlPlane.MaterializeSecrets(ctx, attemptID, fence)
}

type orderDocker struct {
	*fakeDocker
	events *[]string
}

func (o orderDocker) EnsureReleaseRuntimeContainer(ctx context.Context, req docker.RunContainerRequest, net string) (docker.ContainerInspect, error) {
	*o.events = append(*o.events, "container:ensure")
	return o.fakeDocker.EnsureReleaseRuntimeContainer(ctx, req, net)
}

type orderCheckout struct {
	*fakeCheckout
	events *[]string
}

func (o orderCheckout) Checkout(ctx context.Context, attemptID, fence int64) (CheckoutResult, error) {
	*o.events = append(*o.events, "checkout")
	return o.fakeCheckout.Checkout(ctx, attemptID, fence)
}

// Slice 23-G: the durable attempt record reads each phase BEFORE the work of
// that phase can have any effect. In particular "starting" must be recorded
// before the first environment/container call, so a restart that sees an
// earlier phase can never have a container behind it. Moving a side effect
// ahead of tracker.advance fails this test.
func TestAttemptRecordReadsPhaseBeforeItsFirstSideEffect(t *testing.T) {
	var events []string
	cp := &fakeControlPlane{secrets: fullSecrets()}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	inner := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	build := func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		events = append(events, "build")
		return inner(ctx, req)
	}
	e := newTestExecutor(cp, co, dk, build)
	e.controlPlane = orderCP{cp, &events}
	e.docker = orderDocker{dk, &events}
	e.checkout = orderCheckout{co, &events}

	ctx := agentclient.WithPhaseRecorder(context.Background(), func(phase string) error {
		events = append(events, "record:"+phase)
		return nil
	})
	if out := e.Execute(ctx, testLease()); !out.HandedOff {
		t.Fatalf("outcome = %+v, want hand-off", out)
	}
	index := func(ev string) int {
		for i, x := range events {
			if x == ev {
				return i
			}
		}
		t.Fatalf("event %q never happened: %v", ev, events)
		return -1
	}
	for _, c := range [][2]string{
		{"record:fetching", "checkout"},
		{"record:building", "build"},
		{"record:starting", "env:materialize"},
		{"record:starting", "container:ensure"},
	} {
		if index(c[0]) >= index(c[1]) {
			t.Fatalf("%s must precede %s: %v", c[0], c[1], events)
		}
	}
}

// The lost-attempt Complete built by the agent after a restart must be
// byte-identical to the failed/transient outcome the executor would have
// reported for the same attempt, so a Complete the old process managed to send
// replays idempotently instead of conflicting on the server (it compares
// operation id and digest).
func TestLostAttemptRestartOperationMatchesExecutorOutcome(t *testing.T) {
	lease := testLease()
	want := (&Executor{}).outcome(lease, false, queue.FailureClassTransient)
	opID, digest := agentclient.LostAttemptRestartOperation(lease.AttemptID, lease.Fence)
	if opID != want.OperationID || digest != want.ResultDigest {
		t.Fatalf("restart op=%q digest=%x, executor outcome op=%q digest=%x", opID, digest, want.OperationID, want.ResultDigest)
	}
}
