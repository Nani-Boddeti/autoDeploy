package agenthttp

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"
)

// Wire contract between the REAL agent client/reporter and the REAL control
// plane handler for slice 22-D (the repository behind the handler is a fake:
// the database half is covered by the store tests). A failure here means the
// two halves disagree about the bytes.

func contractClient(t *testing.T, h Handler) (*agentclient.Client, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	tok, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := agentclient.NewClient(srv.URL, "server_1", tok, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv.Close
}

func TestRealClientClaimGetsTheNotSwitchedEcho(t *testing.T) {
	sts, bg, rollback := stsLease(), claimGoldenLease(), stsLease()
	bg.JobID, rollback.JobID, rollback.RollbackTargetReleaseID = 5, 6, "rel-target"
	for name, tc := range map[string]struct {
		lease    queue.ClaimedLease
		wantEcho bool
	}{"stop_then_start deployment": {sts, true}, "blue_green": {bg, false}, "rollback": {rollback, false}} {
		t.Run(name, func(t *testing.T) {
			fc := &capClaimer{fakeClaimer: fakeClaimer{leases: []queue.ClaimedLease{tc.lease}}}
			c, closeSrv := contractClient(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc})
			defer closeSrv()
			leases, err := c.Claim(context.Background(), true)
			if err != nil || len(leases) != 1 {
				t.Fatalf("leases=%v err=%v", leases, err)
			}
			if leases[0].ActivationNotSwitched != tc.wantEcho {
				t.Fatalf("ActivationNotSwitched=%v, want %v", leases[0].ActivationNotSwitched, tc.wantEcho)
			}
			if !fc.gotCaps.ActivationNotSwitched || !fc.gotCaps.HealthPolicy || fc.capCalls != 1 {
				t.Fatalf("caps=%+v capCalls=%d, want both declared in one claim", fc.gotCaps, fc.capCalls)
			}
		})
	}
}

func TestRealClientNotSwitchedReportReachesTheOutcomeMethod(t *testing.T) {
	rep := &outcomeReporter{}
	c, closeSrv := contractClient(t, Handler{Authenticator: &fakeAuthenticator{}, ActivationReporter: rep})
	defer closeSrv()
	res, err := agentclient.ActivationNotSwitched("gate failed; predecessor restarted and healthy")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReportActivationResult(context.Background(), 2, 3, "7", res); err != nil {
		t.Fatal(err)
	}
	if rep.outcomeCalls != 1 || rep.gotOutcome != queue.ActivationOutcomeNotSwitched || rep.seen.succeeded || rep.seen.summary == "" {
		t.Fatalf("outcomeCalls=%d outcome=%q seen=%+v", rep.outcomeCalls, rep.gotOutcome, rep.seen)
	}
}

// The downgrade case end to end: the control plane cannot serve the outcome
// (400 invalid_request), the REAL reporter strips it durably and the plain
// failure lands on the plain method, exactly once.
func TestRealReporterDowngradesAgainstAServerWithoutTheOutcome(t *testing.T) {
	rep := &fakeActivationReporter{}
	c, closeSrv := contractClient(t, Handler{Authenticator: &fakeAuthenticator{}, ActivationReporter: rep})
	defer closeSrv()

	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := st.Activations()
	if err != nil {
		t.Fatal(err)
	}
	auth := state.PendingActivation{
		AttemptID: 2, Fence: 3, EnvironmentID: "env_1", OperationID: "7",
		RouteDigest: [32]byte{1}, ImageDigest: [32]byte{2}, RouteRevision: 5, Phase: state.PhaseAuthorized,
		ReleaseID: "rel_new", BackendRuntimeInstanceID: "rt_new", PredecessorReleaseID: "rel_old",
		PredecessorRuntimeInstanceID: "rt_old", DrainSeconds: 30, PredecessorStopSent: true, PreStopRouteDigest: [32]byte{0x11},
	}
	if err := store.SaveActivation(auth); err != nil {
		t.Fatal(err)
	}
	ready := auth
	ready.Phase, ready.FailureSummary, ready.NotSwitched = state.PhaseResultReady, "gate failed", true
	if err := store.SaveActivation(ready); err != nil {
		t.Fatal(err)
	}

	reporter := &agentclient.ActivationReporter{Client: c, Store: store, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reporter.Run(ctx) }()
	deadline := time.After(15 * time.Second)
	for {
		recs, err := store.ListActivations()
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("record never delivered: %+v", recs)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if !rep.seen.called || rep.seen.succeeded || rep.seen.summary != "gate failed" {
		t.Fatalf("plain method not reached with the plain failure: %+v", rep.seen)
	}
}
