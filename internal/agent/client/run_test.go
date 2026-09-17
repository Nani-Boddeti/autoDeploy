package agentclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
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
	"autodeploy/internal/server"
)

// fakeExecutor lets a test control exactly when and how one claimed lease's
// work finishes, without depending on any real Docker/BuildKit executor
// (none exists in this slice; see run.go's Executor doc).
type fakeExecutor struct {
	started chan Lease
	done    chan Outcome
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{started: make(chan Lease, 1), done: make(chan Outcome, 1)}
}

func (f *fakeExecutor) Execute(_ context.Context, lease Lease) Outcome {
	f.started <- lease
	return <-f.done
}

// fakeControlPlane is a minimal, mutex-guarded implementation of the
// internal/agent/http wire contracts this package's Client actually calls,
// enough to drive the lease-handling loop end to end in tests.
type fakeControlPlane struct {
	mu sync.Mutex

	heartbeatCalls int32
	claimCalls     int32
	completeCalls  int32
	cancelAckCalls int32

	leaseToOffer   *leaseWire // nil once claimed, so a second claim returns none
	renewSequences []int
	renewStatus    func(callNumber int) int // 200 unless overridden
	renewCallCount int
}

func (f *fakeControlPlane) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(pathHeartbeat, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.heartbeatCalls, 1)
		writeJSON(w, http.StatusOK, map[string]any{"server_status": "active", "heartbeat_interval_seconds": 3600})
	})
	mux.HandleFunc(pathClaim, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.claimCalls, 1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.leaseToOffer == nil {
			writeJSON(w, http.StatusOK, map[string]any{"leases": []leaseWire{}})
			return
		}
		lease := *f.leaseToOffer
		f.leaseToOffer = nil
		writeJSON(w, http.StatusOK, map[string]any{"leases": []leaseWire{lease}})
	})
	mux.HandleFunc(pathRenew, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Sequence int `json:"sequence"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.renewSequences = append(f.renewSequences, req.Sequence)
		f.renewCallCount++
		status := http.StatusOK
		if f.renewStatus != nil {
			status = f.renewStatus(f.renewCallCount)
		}
		f.mu.Unlock()
		if status != http.StatusOK {
			writeJSON(w, status, map[string]string{"error": statusCode(status)})
			return
		}
		expires := time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339Nano)
		writeJSON(w, http.StatusOK, map[string]any{"lease_expires_at": expires, "authority_expires_at": expires, "cancel_requested": false})
	})
	mux.HandleFunc(pathComplete, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.completeCalls, 1)
		writeJSON(w, http.StatusOK, struct{}{})
	})
	mux.HandleFunc(pathCancelAck, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.cancelAckCalls, 1)
		writeJSON(w, http.StatusOK, struct{}{})
	})
	return mux
}

func statusCode(status int) string {
	if status == http.StatusConflict {
		return "stale_lease"
	}
	return "service_unavailable"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newTestAgent(t *testing.T, srv *httptest.Server, executor Executor, logger *log.Logger) *Agent {
	t.Helper()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(srv.URL, "server_test", token, srv.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{Client: client, State: store, Executor: executor, Logger: logger, RenewInterval: 30 * time.Millisecond}
}

func TestNoClaimCallsWhenExecutorNil(t *testing.T) {
	fake := &fakeControlPlane{}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()

	agent := newTestAgent(t, srv, nil, nil)
	agent.tick(context.Background())
	agent.tick(context.Background())

	if fake.heartbeatCalls != 2 {
		t.Fatalf("expected 2 heartbeats, got %d", fake.heartbeatCalls)
	}
	if fake.claimCalls != 0 {
		t.Fatalf("expected 0 claim calls with a nil executor, got %d", fake.claimCalls)
	}
}

func TestRenewSequenceIncrementsAcrossSuccessfulRenewals(t *testing.T) {
	fake := &fakeControlPlane{leaseToOffer: fixtureLease(t, "env_a", 1)}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()

	executor := newFakeExecutor()
	agent := newTestAgent(t, srv, executor, nil)

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()

	<-executor.started
	// Let at least two renewal ticks land before finishing the lease.
	waitForSequenceCount(t, fake, 2)
	executor.done <- Outcome{Succeeded: true, OperationID: "op-1", ResultDigest: [32]byte{1}}
	<-done

	fake.mu.Lock()
	seqs := append([]int(nil), fake.renewSequences...)
	fake.mu.Unlock()
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("renew sequences not monotonic from 1: %v", seqs)
		}
	}
	if fake.completeCalls != 1 {
		t.Fatalf("expected exactly one complete call, got %d", fake.completeCalls)
	}
}

func TestRenewReplaysSameSequenceAfterNetworkError(t *testing.T) {
	fake := &fakeControlPlane{
		leaseToOffer: fixtureLease(t, "env_b", 1),
		renewStatus: func(callNumber int) int {
			if callNumber <= 2 {
				return http.StatusServiceUnavailable
			}
			return http.StatusOK
		},
	}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()

	executor := newFakeExecutor()
	agent := newTestAgent(t, srv, executor, nil)

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()

	<-executor.started
	waitForSequenceCount(t, fake, 3)
	executor.done <- Outcome{Succeeded: true, OperationID: "op-2", ResultDigest: [32]byte{2}}
	<-done

	fake.mu.Lock()
	seqs := append([]int(nil), fake.renewSequences...)
	fake.mu.Unlock()
	if len(seqs) < 3 || seqs[0] != 1 || seqs[1] != 1 || seqs[2] != 1 {
		t.Fatalf("expected the failed renewal to replay sequence 1, got %v", seqs)
	}
}

func TestStaleLeaseStopsAndMarksReconciliationRequired(t *testing.T) {
	fake := &fakeControlPlane{
		leaseToOffer: fixtureLease(t, "env_c", 5),
		renewStatus:  func(int) int { return http.StatusConflict },
	}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()

	executor := newFakeExecutor()
	agent := newTestAgent(t, srv, executor, nil)

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()

	<-executor.started
	// Executor must observe ctx cancellation once the stale lease is
	// detected and return promptly; the test unblocks it explicitly to
	// avoid depending on ctx-cancellation timing inside the fake.
	go func() {
		time.Sleep(200 * time.Millisecond)
		executor.done <- Outcome{Succeeded: true, OperationID: "op-3", ResultDigest: [32]byte{3}}
	}()
	<-done

	if fake.completeCalls != 0 || fake.cancelAckCalls != 0 {
		t.Fatalf("stale lease must never report a completion or cancel-ack: complete=%d cancelAck=%d", fake.completeCalls, fake.cancelAckCalls)
	}
	fence, existed, err := agent.State.Load("env_c")
	if err != nil || !existed || !fence.ReconciliationRequired || fence.HighestAcceptedFence != 5 {
		t.Fatalf("fence=%+v existed=%v err=%v", fence, existed, err)
	}
}

func TestLowerFenceRejectedClientSide(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state.Fence{EnvironmentID: "env_d", HighestAcceptedFence: 10}); err != nil {
		t.Fatal(err)
	}
	executor := newFakeExecutor()
	fake := &fakeControlPlane{}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(srv.URL, "server_test", token, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Client: client, State: store, Executor: executor, RenewInterval: 30 * time.Millisecond}

	agent.runLease(context.Background(), Lease{AttemptID: 1, Fence: 3, EnvironmentID: "env_d"})

	select {
	case <-executor.started:
		t.Fatal("executor must not run for a lease whose fence does not advance the stored fence")
	default:
	}
	fence, _, err := store.Load("env_d")
	if err != nil || fence.HighestAcceptedFence != 10 {
		t.Fatalf("stored fence must be unchanged: %+v %v", fence, err)
	}
}

func TestFailClosedOnCorruptFenceState(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state.Fence{EnvironmentID: "env_e", HighestAcceptedFence: 1}); err != nil {
		t.Fatal(err)
	}
	corruptFenceFile(t, dir)

	executor := newFakeExecutor()
	fake := &fakeControlPlane{}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(srv.URL, "server_test", token, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Client: client, State: store, Executor: executor, RenewInterval: 30 * time.Millisecond}

	agent.runLease(context.Background(), Lease{AttemptID: 1, Fence: 99, EnvironmentID: "env_e"})

	select {
	case <-executor.started:
		t.Fatal("executor must not run when local fence state is corrupt")
	default:
	}
}

func TestLogsContainNoTokenOrDigest(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	fake := &fakeControlPlane{
		leaseToOffer: fixtureLease(t, "env_f", 1),
		renewStatus:  func(int) int { return http.StatusConflict },
	}
	srv := httptest.NewTLSServer(fake.mux())
	defer srv.Close()

	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(srv.URL, "server_test", token, srv.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor := newFakeExecutor()
	agent := &Agent{Client: client, State: store, Executor: executor, Logger: logger, RenewInterval: 30 * time.Millisecond}

	done := make(chan struct{})
	go func() { agent.claimAndRun(context.Background()); close(done) }()
	<-executor.started
	executor.done <- Outcome{Succeeded: true, OperationID: "op-log", ResultDigest: [32]byte{9}}
	<-done

	var digest [32]byte
	digest[0] = 9
	digestHex := hex.EncodeToString(digest[:])
	logged := buf.String()
	if strings.Contains(logged, token.String()) {
		t.Fatal("log output must never contain the raw credential token")
	}
	if strings.Contains(logged, digestHex) {
		t.Fatal("log output must never contain a result digest")
	}
}

// fixtureLease builds a claimable leaseWire with valid RFC3339Nano
// deadlines far enough in the future not to be treated as expired by
// anything this package checks.
func fixtureLease(t *testing.T, environmentID string, fence int64) *leaseWire {
	t.Helper()
	expires := time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339Nano)
	return &leaseWire{
		JobID: 1, AttemptID: 1, AttemptNumber: 1, Fence: fence,
		ProjectID: "proj_1", EnvironmentID: environmentID, DeploymentID: "dep_1",
		RepositoryID: 1, Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40),
		LeaseExpiresAt: expires, AuthorityExpiresAt: expires,
	}
}

func waitForSequenceCount(t *testing.T, fake *fakeControlPlane, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		count := len(fake.renewSequences)
		fake.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d renew calls", n)
}

func corruptFenceFile(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		// Skip the durable environment-index manifest itself: corrupting
		// it would exercise a different (also-tested) failure mode.
		if filepath.Base(m) == ".fence-index.json" {
			continue
		}
		if err := os.WriteFile(m, []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("no fence state file found to corrupt")
}
