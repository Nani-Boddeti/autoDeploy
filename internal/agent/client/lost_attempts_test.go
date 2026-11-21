package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autodeploy/internal/agent/state"
)

type completeBody struct {
	AttemptID    int64  `json:"attempt_id"`
	Fence        int64  `json:"fence"`
	Outcome      string `json:"outcome"`
	FailureClass string `json:"failure_class"`
	OperationID  string `json:"operation_id"`
}

type lostEnv struct {
	agent     *Agent
	stateDir  string
	attempts  state.AttemptStore
	acts      state.ActivationStore
	fake      *fakeControlPlane
	logs      *bytes.Buffer
	mu        sync.Mutex
	completes []completeBody
	cancelAck int32
	// respond decides the answer to the n-th (1-based) Complete call.
	respond func(n int) (int, string)
}

func newLostEnv(t *testing.T, respond func(n int) (int, string)) *lostEnv {
	t.Helper()
	old := lostAttemptRetryBackoff
	lostAttemptRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { lostAttemptRetryBackoff = old })

	e := &lostEnv{fake: &fakeControlPlane{}, logs: &bytes.Buffer{}, respond: respond}
	mux := http.NewServeMux()
	mux.Handle("/", e.fake.mux())
	mux.HandleFunc(pathComplete, func(w http.ResponseWriter, r *http.Request) {
		var b completeBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		e.mu.Lock()
		e.completes = append(e.completes, b)
		n := len(e.completes)
		e.mu.Unlock()
		status, code := http.StatusOK, ""
		if e.respond != nil {
			status, code = e.respond(n)
		}
		if status != http.StatusOK {
			writeJSON(w, status, map[string]string{"error": code})
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	})
	mux.HandleFunc(pathCancelAck, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&e.cancelAck, 1)
		writeJSON(w, http.StatusOK, struct{}{})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(e.logs, nil))
	e.agent = newTestAgent(t, srv, nil, logger)
	e.stateDir = t.TempDir()
	store, err := state.New(e.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	e.agent.State = store
	if e.attempts, err = e.agent.State.Attempts(); err != nil {
		t.Fatal(err)
	}
	if e.acts, err = e.agent.State.Activations(); err != nil {
		t.Fatal(err)
	}
	e.agent.Attempts = &e.attempts
	e.agent.Activations = &e.acts
	return e
}

func (e *lostEnv) seed(t *testing.T, id int64, phase string) {
	t.Helper()
	err := e.attempts.SaveAttempt(state.AttemptRecord{AttemptID: id, JobID: 5, DeploymentID: "dep_1", EnvironmentID: "env_1", Fence: 7, Phase: phase})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *lostEnv) exists(t *testing.T, id int64) bool {
	t.Helper()
	_, ok, err := e.attempts.LoadAttempt(id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (e *lostEnv) completeCalls() []completeBody {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]completeBody(nil), e.completes...)
}

func TestLostAttemptBeforeStartingReportedFailedTransient(t *testing.T) {
	for i, phase := range []string{"claimed", "fetching", "building"} {
		t.Run(phase, func(t *testing.T) {
			e := newLostEnv(t, nil)
			id := int64(10 + i)
			e.seed(t, id, phase)
			if !e.agent.recoverLostAttempts(context.Background()) {
				t.Fatal("recovery must allow the claim loop")
			}
			calls := e.completeCalls()
			if len(calls) != 1 {
				t.Fatalf("complete calls = %d, want 1", len(calls))
			}
			c := calls[0]
			if c.AttemptID != id || c.Fence != 7 || c.Outcome != "failed" || c.FailureClass != "transient" || c.OperationID == "" {
				t.Fatalf("complete = %+v", c)
			}
			if e.exists(t, id) {
				t.Fatal("record must be deleted after an accepted report")
			}
			if n := strings.Count(e.logs.String(), "lost attempt reported failed (agent restart)"); n != 1 {
				t.Fatalf("log lines = %d, want 1; logs=%s", n, e.logs)
			}
		})
	}
}

func TestLostAttemptStartingOrLaterOnlyLogged(t *testing.T) {
	for i, phase := range []string{"starting", "health_checking", "activating", "something_new"} {
		t.Run(phase, func(t *testing.T) {
			e := newLostEnv(t, nil)
			id := int64(20 + i)
			e.seed(t, id, phase)
			e.agent.recoverLostAttempts(context.Background())
			if n := len(e.completeCalls()); n != 0 {
				t.Fatalf("complete calls = %d, want none", n)
			}
			if e.exists(t, id) {
				t.Fatal("record must be deleted")
			}
			if !strings.Contains(e.logs.String(), "left to lease expiry") || strings.Count(e.logs.String(), "lost attempt in") != 1 {
				t.Fatalf("logs=%s", e.logs)
			}
		})
	}
}

func TestLostAttemptWithActivationRecordLeftToActivationRecovery(t *testing.T) {
	e := newLostEnv(t, nil)
	e.seed(t, 30, "building")
	var d [32]byte
	d[0] = 1
	if err := e.acts.SaveActivation(state.PendingActivation{
		AttemptID: 30, Fence: 7, EnvironmentID: "env_1", OperationID: "9001",
		RouteDigest: d, ImageDigest: d, RouteRevision: 1, Phase: state.PhaseAuthorized,
	}); err != nil {
		t.Fatal(err)
	}
	e.agent.recoverLostAttempts(context.Background())
	if n := len(e.completeCalls()); n != 0 {
		t.Fatalf("complete calls = %d, want none", n)
	}
	if e.exists(t, 30) {
		t.Fatal("attempt record must be deleted")
	}
	if _, ok, _ := e.acts.LoadActivation(30); !ok {
		t.Fatal("activation record must be untouched")
	}
}

func TestLostAttemptDefinitiveRefusalDeletesRecord(t *testing.T) {
	cases := map[string]struct {
		status int
		code   string
	}{
		"stale":        {http.StatusConflict, "stale_lease"},
		"unauthorized": {http.StatusUnauthorized, ""},
		"integrity":    {http.StatusConflict, "integrity_conflict"},
		"invalid":      {http.StatusBadRequest, "invalid_request"},
		"unknown code": {http.StatusConflict, "some_future_code"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newLostEnv(t, func(int) (int, string) { return tc.status, tc.code })
			e.seed(t, 40, "building")
			e.agent.recoverLostAttempts(context.Background())
			if e.exists(t, 40) {
				t.Fatal("record must be deleted on a definitive refusal")
			}
			if !strings.Contains(e.logs.String(), "lost attempt already closed by the control plane") {
				t.Fatalf("logs=%s", e.logs)
			}
		})
	}
}

func TestLostAttemptTransientKeepsRecordAndClaimLoopStarts(t *testing.T) {
	e := newLostEnv(t, func(int) (int, string) { return http.StatusServiceUnavailable, "service_unavailable" })
	e.seed(t, 50, "building")
	e.agent.Executor = newFakeExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.agent.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&e.fake.claimCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if atomic.LoadInt32(&e.fake.claimCalls) == 0 {
		t.Fatal("claim loop must still start")
	}
	if !e.exists(t, 50) {
		t.Fatal("record must be kept for the next restart")
	}
	if len(e.completeCalls()) < 3 {
		t.Fatalf("complete calls = %d, want at least 3 bounded tries", len(e.completeCalls()))
	}
}

func TestLostAttemptRecordIsIdempotentAcrossRestarts(t *testing.T) {
	e := newLostEnv(t, func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, ""
		}
		return http.StatusConflict, "stale_lease"
	})
	e.seed(t, 60, "fetching")
	e.agent.recoverLostAttempts(context.Background())
	e.agent.recoverLostAttempts(context.Background())
	if n := len(e.completeCalls()); n != 1 {
		t.Fatalf("complete calls = %d, want 1 (record gone after first restart)", n)
	}
}

func TestCorruptAttemptRecordRefusesToClaim(t *testing.T) {
	e := newLostEnv(t, nil)
	e.seed(t, 70, "building")
	dir := filepath.Join(e.stateDir, "attempts")
	if err := os.WriteFile(filepath.Join(dir, "attempt-70.json"), []byte(`{"version":1,"bogus":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if e.agent.recoverLostAttempts(context.Background()) {
		t.Fatal("corrupt record must refuse the claim loop")
	}
	if len(e.completeCalls()) != 0 {
		t.Fatal("no Complete on corrupt state")
	}
	e.agent.Executor = newFakeExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = e.agent.Run(ctx)
	if atomic.LoadInt32(&e.fake.claimCalls) != 0 {
		t.Fatal("agent must not claim with corrupt attempt state")
	}
}

// --- lifecycle: the record advances and is deleted in-process ---

type recordingExecutor struct {
	t       *testing.T
	store   state.AttemptStore
	phases  []string
	outcome Outcome
	cancel  bool
	fake    *fakeControlPlane
}

func (r *recordingExecutor) Execute(ctx context.Context, lease Lease) Outcome {
	rec := PhaseRecorderFrom(ctx)
	if rec == nil {
		r.t.Error("executor context carries no phase recorder")
		return r.outcome
	}
	if got, ok, _ := r.store.LoadAttempt(lease.AttemptID); !ok || got.Phase != state.AttemptPhaseClaimed || got.Fence != lease.Fence {
		r.t.Errorf("record before any phase = %+v ok=%v", got, ok)
	}
	for _, p := range []string{"fetching", "building", "starting"} {
		if err := rec(p); err != nil {
			r.t.Error(err)
		}
		got, _, _ := r.store.LoadAttempt(lease.AttemptID)
		r.phases = append(r.phases, got.Phase)
	}
	if r.cancel {
		<-ctx.Done()
	}
	return r.outcome
}

func runLifecycle(t *testing.T, outcome Outcome, cancel bool) (*lostEnv, *recordingExecutor) {
	t.Helper()
	e := newLostEnv(t, nil)
	e.fake.leaseToOffer = fixtureLease(t, "env_lc", 1)
	e.fake.cancelRequested = cancel
	ex := &recordingExecutor{t: t, store: e.attempts, outcome: outcome, cancel: cancel}
	e.agent.Executor = ex
	e.agent.claimAndRun(context.Background())
	return e, ex
}

func TestAttemptRecordAdvancesBeforeEachPhaseAndIsDeletedOnComplete(t *testing.T) {
	e, ex := runLifecycle(t, Outcome{Succeeded: true, OperationID: "op-1-1"}, false)
	if strings.Join(ex.phases, ",") != "fetching,building,starting" {
		t.Fatalf("phases = %v", ex.phases)
	}
	if e.exists(t, 1) {
		t.Fatal("record must be deleted after Complete accepted")
	}
}

func TestAttemptRecordDeletedOnCancelAck(t *testing.T) {
	e, _ := runLifecycle(t, Outcome{OperationID: "op-1-1"}, true)
	if atomic.LoadInt32(&e.cancelAck) != 1 {
		t.Fatal("expected a cancel ack")
	}
	if e.exists(t, 1) {
		t.Fatal("record must be deleted on cancel ack")
	}
}

func TestAttemptRecordDeletedOnActivationHandOff(t *testing.T) {
	e, _ := runLifecycle(t, Outcome{HandedOff: true, OperationID: "9001"}, false)
	if e.exists(t, 1) {
		t.Fatal("record must be deleted on hand-off")
	}
}

func TestAttemptRecordKeptWhenCompleteStaysTransient(t *testing.T) {
	e := newLostEnv(t, func(int) (int, string) { return http.StatusServiceUnavailable, "service_unavailable" })
	e.fake.leaseToOffer = fixtureLease(t, "env_lt", 1)
	e.agent.Executor = &recordingExecutor{t: t, store: e.attempts, outcome: Outcome{Succeeded: true, OperationID: "op-1-1"}}
	e.agent.claimAndRun(context.Background())
	if !e.exists(t, 1) {
		t.Fatal("record must survive an unreported completion")
	}
}
