package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/queue"
)

// Only queue.FailureClassTransient is ever retried, so every mapping here
// decides whether a deployment gets another attempt at all. These cases
// are written against that consequence, not against the enum's spelling.
func TestClassifyHealthGateResultPerReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result HealthGateResult
		want   queue.FailureClass
	}{
		{
			name:   "gate ran its deadline with the container up: the app never became healthy",
			result: HealthGateResult{Reason: HealthGateFailureDeadlineExceeded},
			want:   queue.FailureClassPermanentConfiguration,
		},
		{
			name:   "agent context went away: nothing about the app failed, must stay retryable",
			result: HealthGateResult{Reason: HealthGateFailureContextCancelled},
			want:   queue.FailureClassTransient,
		},
		{
			name:   "kernel OOM-killed the container: a host condition, not an app defect",
			result: HealthGateResult{Reason: HealthGateFailureContainerNotRunning, OOMKilled: true, ExitCode: 137},
			want:   queue.FailureClassTransient,
		},
		{
			name:   "container exited on its own: a crash on boot reproduces on retry",
			result: HealthGateResult{Reason: HealthGateFailureContainerNotRunning, ExitCode: 1},
			want:   queue.FailureClassPermanentConfiguration,
		},
		{
			name:   "unexplained failure fails closed rather than guessing it is retryable",
			result: HealthGateResult{},
			want:   queue.FailureClassCorruptState,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyHealthGateResult(tc.result); got != tc.want {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
		})
	}
}

// A cancelled gate must be retryable; an OOM kill must be retryable; an
// application that exited must not be. Stated as retryability directly,
// because that is the behavior that actually matters.
func TestClassifyHealthGateResultRetryability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  HealthGateResult
		want    bool
		because string
	}{
		{"context cancelled", HealthGateResult{Reason: HealthGateFailureContextCancelled}, true,
			"an agent restart mid-gate must not permanently fail a deployment"},
		{"oom killed", HealthGateResult{Reason: HealthGateFailureContainerNotRunning, OOMKilled: true}, true,
			"host memory pressure may not recur on the next attempt"},
		{"app exited", HealthGateResult{Reason: HealthGateFailureContainerNotRunning, ExitCode: 1}, false,
			"a crash on boot reproduces identically on retry"},
		{"never became healthy", HealthGateResult{Reason: HealthGateFailureDeadlineExceeded}, false,
			"the app came up but never passed the gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyHealthGateResult(tc.result).Retryable(); got != tc.want {
				t.Fatalf("retryable = %v, want %v: %s", got, tc.want, tc.because)
			}
		})
	}
}

// The gate's own deadline can expire either at the top-of-loop check or
// while an inspect call is in flight. Both are the same real condition and
// must classify identically -- otherwise an identical stuck candidate is
// retried or permanently failed depending on which internal timer noticed
// first.
func TestRunHealthGateDeadlineDuringInspectClassifiesAsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	state := func(ctx context.Context) (ContainerStatus, error) {
		<-ctx.Done()
		// What the engine client really returns when the context expires
		// mid-request: a transient-classed EngineError.
		return ContainerStatus{}, &EngineError{Class: ErrClassTransient, Op: "inspect", Err: ctx.Err()}
	}

	result, err := runHealthGate(ctx, fixedProbe(true), state, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate returned an error rather than a deadline result: %v", err)
	}
	if result.Reason != HealthGateFailureDeadlineExceeded {
		t.Fatalf("reason = %q, want deadline_exceeded", result.Reason)
	}
	if got := ClassifyHealthGateResult(result); got != queue.FailureClassPermanentConfiguration {
		t.Fatalf("class = %q, want permanent_configuration (the same verdict the top-of-loop path gives)", got)
	}
}

// The OOM evidence has to survive the gate loop, or classification cannot
// use it.
func TestRunHealthGateCarriesOOMEvidence(t *testing.T) {
	state := func(context.Context) (ContainerStatus, error) {
		return ContainerStatus{State: ContainerStateNotRunning, ExitCode: 137, OOMKilled: true}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := runHealthGate(ctx, fixedProbe(true), state, testInterval, testTimeout, 3)
	if err != nil {
		t.Fatalf("runHealthGate: %v", err)
	}
	if !result.OOMKilled || result.ExitCode != 137 {
		t.Fatalf("result = %+v, want the OOM evidence carried through", result)
	}
	if !ClassifyHealthGateResult(result).Retryable() {
		t.Fatal("an OOM-killed container must be retryable")
	}
}

// containerRunningState must read OOMKilled off the same inspect call it
// already makes, or the evidence never exists in the first place.
func TestContainerRunningStateReadsOOMKilled(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Id":"c1","State":{"Status":"exited","Running":false,"ExitCode":137,"OOMKilled":true}}`))
	})
	c, err := newClient(startFakeEngine(t, h))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	st, err := c.containerRunningState(context.Background(), "c1")
	if err != nil {
		t.Fatalf("containerRunningState: %v", err)
	}
	if st.State != ContainerStateNotRunning || !st.OOMKilled || st.ExitCode != 137 {
		t.Fatalf("status = %+v, want not-running with OOM evidence", st)
	}
}

// writeJSON encodes v as the fake engine's JSON response.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode fake response: %v", err)
	}
}

// adoptFakeEngine emulates losing the create race: the first lookup finds
// nothing, the create is refused with 409, and the second lookup finds the
// container the winner created.
type adoptFakeEngine struct {
	mu          sync.Mutex
	listCalls   int
	winner      ContainerSummary
	conflict    bool
	startCalls  []string
	createCalls int
}

func (f *adoptFakeEngine) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.listCalls++
		first := f.listCalls == 1
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.Write([]byte(`[]`))
			return
		}
		if f.conflict {
			// The name is held by something that is NOT this identity.
			w.Write([]byte(`[]`))
			return
		}
		writeJSON(t, w, []ContainerSummary{f.winner})
	})
	mux.HandleFunc("/v1.44/networks/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, NetworkInspect{ID: strings.Repeat("a", 64), Name: "env-net"})
	})
	mux.HandleFunc("/v1.44/containers/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.createCalls++
		f.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"message":"Conflict. The container name is already in use"}`))
	})
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.44/containers/"), "/start")
			f.mu.Lock()
			f.startCalls = append(f.startCalls, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			writeJSON(t, w, runningInspect("winner"))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	})
	return mux
}

// Losing a benign create race must not poison the deployment: the race has
// already resolved correctly and exactly one container exists, so the
// loser adopts it. corrupt_state is never retried, so getting this wrong
// permanently fails a deployment that actually succeeded.
func TestEnsureAdoptsContainerAfterLosingCreateRace(t *testing.T) {
	f := &adoptFakeEngine{winner: ContainerSummary{ID: "winner", State: "running"}}
	c, err := newClient(startFakeEngine(t, f.handler(t)))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	got, err := c.EnsureReleaseRuntimeContainer(context.Background(), validRunRequest(), "env-net")
	if err != nil {
		t.Fatalf("losing the create race must adopt the winner, not fail: %v", err)
	}
	if got.ID != "winner" {
		t.Fatalf("adopted %q, want winner", got.ID)
	}
	if len(f.startCalls) != 1 || f.startCalls[0] != "winner" {
		t.Fatalf("start calls = %v, want the adopted container started once", f.startCalls)
	}
}

// A name held by something that does NOT match this identity is genuine
// local inconsistency and must stay non-retryable.
func TestEnsureNameConflictWithoutMatchingIdentityStaysCorruptState(t *testing.T) {
	f := &adoptFakeEngine{conflict: true}
	c, err := newClient(startFakeEngine(t, f.handler(t)))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.EnsureReleaseRuntimeContainer(context.Background(), validRunRequest(), "env-net")
	if err == nil {
		t.Fatal("want an error when the name is held by a different identity")
	}
	if !errors.Is(err, ErrContainerNameConflict) {
		t.Fatalf("err = %v, want a name conflict", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassCorruptState {
		t.Fatalf("class = %q, want corrupt_state", got)
	}
}
