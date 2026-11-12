package agentclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// recoveryAgent wires an eventLog-backed Recover and Reporter onto a test
// agent whose control plane offers one lease.
func recoveryAgent(t *testing.T, fake *fakeControlPlane) (*Agent, *fakeExecutor, *eventLog, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(fake.mux())
	events := &eventLog{}
	exec := newFakeExecutor()
	agent := newTestAgent(t, srv, exec, nil)
	agent.Reporter = &fakeDelivery{log: events, stopped: make(chan struct{})}
	agent.Recover = func() { events.add("recover") }
	return agent, exec, events, srv.Close
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("claimAndRun did not return")
	}
}

// A lease whose executor leaves a stranded `authorized` record (result save
// and direct report both failed) must be recovered and reported without an
// agent restart.
func TestRecoverRunsAndKicksReporterAfterLeaseReturns(t *testing.T) {
	fake := &fakeControlPlane{leaseToOffer: fixtureLease(t, "env_r1", 1)}
	agent, exec, events, closeSrv := recoveryAgent(t, fake)
	defer closeSrv()

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()
	<-exec.started
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("events while executor running = %v, want none", got)
	}
	exec.done <- Outcome{HandedOff: true, OperationID: "op"}
	waitDone(t, done)

	if got, want := events.snapshot(), []string{"recover", "kick"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
}

// Recover must never run while the executor goroutine is still running, even
// when the lease has already been cancelled or a recovery tick passes.
func TestRecoverNeverRunsWhileExecutorRuns(t *testing.T) {
	fake := &fakeControlPlane{leaseToOffer: fixtureLease(t, "env_r2", 1)}
	agent, exec, events, closeSrv := recoveryAgent(t, fake)
	defer closeSrv()

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()
	<-exec.started
	// Several renewal ticks pass (30ms interval) while the executor blocks.
	time.Sleep(200 * time.Millisecond)
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("recover ran while the executor was running: %v", got)
	}
	exec.done <- Outcome{Succeeded: true, OperationID: "op"}
	waitDone(t, done)
	if got := events.snapshot(); len(got) == 0 || got[0] != "recover" {
		t.Fatalf("events=%v, want recover after the executor returned", got)
	}
}

// A stale-lease exit also recovers, and only after the executor returned.
func TestRecoverRunsAfterStaleLeaseExit(t *testing.T) {
	fake := &fakeControlPlane{
		leaseToOffer: fixtureLease(t, "env_r3", 1),
		renewStatus:  func(int) int { return http.StatusConflict },
	}
	agent, exec, events, closeSrv := recoveryAgent(t, fake)
	defer closeSrv()

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()
	<-exec.started
	waitForCtxDone(t, exec) // stale renewal cancelled the executor
	time.Sleep(50 * time.Millisecond)
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("recover ran before the executor returned: %v", got)
	}
	exec.done <- Outcome{HandedOff: true, OperationID: "op"}
	waitDone(t, done)
	if got, want := events.snapshot(), []string{"recover", "kick"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
}

// Context cancellation mid-lease: recovery runs after the executor returned.
func TestRecoverRunsAfterContextCancelExit(t *testing.T) {
	fake := &fakeControlPlane{leaseToOffer: fixtureLease(t, "env_r4", 1)}
	agent, exec, events, closeSrv := recoveryAgent(t, fake)
	defer closeSrv()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { agent.claimAndRun(ctx); close(done) }()
	<-exec.started
	cancel()
	waitForCtxDone(t, exec)
	time.Sleep(50 * time.Millisecond)
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("recover ran before the executor returned: %v", got)
	}
	exec.done <- Outcome{HandedOff: true, OperationID: "op"}
	waitDone(t, done)
	if got, want := events.snapshot(), []string{"recover", "kick"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
}
