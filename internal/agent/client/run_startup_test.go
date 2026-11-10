package agentclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// fakeDelivery records its lifecycle in the shared event log.
type fakeDelivery struct {
	log     *eventLog
	stopped chan struct{}
}

func (d *fakeDelivery) Run(ctx context.Context) error {
	defer close(d.stopped)
	d.log.add("reporter-run")
	<-ctx.Done()
	return ctx.Err()
}

func (d *fakeDelivery) Kick() { d.log.add("kick") }

func TestRunRecoversBeforeFirstClaimAndStartsReporter(t *testing.T) {
	events := &eventLog{}
	fake := &fakeControlPlane{}
	mux := fake.mux()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathClaim {
			events.add("claim")
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	agent := newTestAgent(t, srv, exec, nil)
	delivery := &fakeDelivery{log: events, stopped: make(chan struct{})}
	agent.Reporter = delivery
	agent.Recover = func() { events.add("recover") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := events.snapshot()
		if len(got) > 0 && got[len(got)-1] == "claim" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no claim observed; events=%v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := events.snapshot()
	if got[0] != "recover" {
		t.Fatalf("events=%v, want recovery first", got)
	}
	kickIdx, claimIdx := -1, -1
	for i, e := range got {
		switch {
		case e == "kick" && kickIdx < 0:
			kickIdx = i
		case e == "claim" && claimIdx < 0:
			claimIdx = i
		}
	}
	if kickIdx < 0 || kickIdx > claimIdx {
		t.Fatalf("events=%v, want a reporter kick right after recovery and before the first claim", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	select {
	case <-delivery.stopped:
	default:
		t.Fatal("reporter goroutine still running after Run returned (leak)")
	}
}

func TestRunWithoutActivationHooksBehavesAsBefore(t *testing.T) {
	fake := &fakeControlPlane{}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()
	agent := newTestAgent(t, srv, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if fake.claimCalls != 0 {
		t.Fatalf("claimCalls=%d", fake.claimCalls)
	}
}
