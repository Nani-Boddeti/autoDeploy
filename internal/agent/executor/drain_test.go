package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"autodeploy/internal/agent/state"
	"autodeploy/internal/runtime/docker"
)

var _ DrainDocker = (*docker.Client)(nil)
var _ DrainStore = state.DrainStore{}

const (
	drainTestEnv      = "env_1"
	drainTestNewRel   = "rel_new"
	drainTestOldRel   = "rel_old"
	drainTestNewRT    = "42"
	drainTestOldRT    = "41"
	drainTestTargetID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var drainTestDigest = [32]byte{7}

type drainDockerFake struct {
	mu         sync.Mutex
	calls      []string
	stopTarget []string
	inspect    docker.ContainerInspect
	inspectErr error
	list       []docker.ContainerSummary
	listErr    error
	stopErrs   []error // consumed one per StopContainer call; empty = success
	waitErrs   []error
	stopBlock  bool // StopContainer blocks until ctx is done
}

func (f *drainDockerFake) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *drainDockerFake) count(c string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.calls {
		if x == c {
			n++
		}
	}
	return n
}

func (f *drainDockerFake) InspectContainer(_ context.Context, id string) (docker.ContainerInspect, error) {
	f.record("inspect")
	if id != "autodeploy-rt-"+drainTestOldRT {
		return docker.ContainerInspect{}, fmt.Errorf("unexpected inspect target %q", id)
	}
	return f.inspect, f.inspectErr
}

func (f *drainDockerFake) ListContainersByLabels(_ context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
	f.record("list")
	if len(labels) == 0 {
		return nil, errors.New("empty selector")
	}
	return f.list, f.listErr
}

func (f *drainDockerFake) StopContainer(ctx context.Context, id string, grace time.Duration) error {
	f.record("stop")
	f.mu.Lock()
	f.stopTarget = append(f.stopTarget, fmt.Sprintf("%s/%s", id, grace))
	var err error
	if len(f.stopErrs) > 0 {
		err, f.stopErrs = f.stopErrs[0], f.stopErrs[1:]
	}
	block := f.stopBlock
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *drainDockerFake) WaitStopped(_ context.Context, _ string, _, _ time.Duration) (docker.ContainerInspect, error) {
	f.record("wait")
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if len(f.waitErrs) > 0 {
		err, f.waitErrs = f.waitErrs[0], f.waitErrs[1:]
	}
	return docker.ContainerInspect{}, err
}

func predecessorLabels() map[string]string {
	return map[string]string{
		docker.LabelManaged:           "true",
		docker.LabelKind:              docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID:     drainTestEnv,
		docker.LabelReleaseID:         drainTestOldRel,
		docker.LabelRuntimeInstanceID: drainTestOldRT,
	}
}

func newDrainDockerFake() *drainDockerFake {
	f := &drainDockerFake{
		list: []docker.ContainerSummary{{ID: drainTestTargetID, Labels: predecessorLabels()}},
	}
	f.inspect.ID = drainTestTargetID
	f.inspect.State.Running = true
	f.inspect.State.Status = "running"
	return f
}

type drainHarness struct {
	t      *testing.T
	root   string
	store  state.DrainStore
	docker *drainDockerFake
	d      *Drainer
	t0     time.Time
	now    time.Time
	digest string
	exists bool
	rdErr  error
	logs   *bytes.Buffer
	mono   time.Time
}

func newDrainHarness(t *testing.T) *drainHarness {
	t.Helper()
	root := t.TempDir()
	st, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := st.Drains()
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Second)
	h := &drainHarness{
		t: t, root: root, store: store, docker: newDrainDockerFake(), t0: t0, now: t0,
		digest: routeDigestString(drainTestDigest), exists: true, logs: &bytes.Buffer{}, mono: t0,
	}
	h.d = h.newDrainer()
	return h
}

func (h *drainHarness) newDrainer() *Drainer {
	return &Drainer{
		Store: h.store, Docker: h.docker, RouteDir: "/route-dir",
		Logger:      log.New(h.logs, "", 0),
		ReadDigest:  func(string, string) (string, bool, error) { return h.digest, h.exists, h.rdErr },
		Now:         func() time.Time { return h.now },
		Monotonic:   func() time.Time { return h.mono },
		StopGrace:   30 * time.Second,
		MaxAttempts: 3, RetryBackoff: 30 * time.Second, MaxRetryBackoff: 10 * time.Minute,
		PollInterval: 30 * time.Second,
	}
}

func (h *drainHarness) arm(attempt int64, drainSeconds int) state.PendingDrain {
	h.t.Helper()
	rec := state.PendingDrain{
		AttemptID: attempt, EnvironmentID: drainTestEnv,
		ReleaseID: drainTestNewRel, BackendRuntimeInstanceID: drainTestNewRT,
		PredecessorReleaseID: drainTestOldRel, PredecessorRuntimeInstanceID: drainTestOldRT,
		DrainSeconds: drainSeconds, ArmedAt: h.t0, ObservedRouteDigest: drainTestDigest,
		Phase: state.DrainPending,
	}
	if err := h.store.ArmDrain(rec); err != nil {
		h.t.Fatal(err)
	}
	return rec
}

func (h *drainHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *drainHarness) get(attempt int64) (state.PendingDrain, bool) {
	h.t.Helper()
	rec, ok, err := h.store.LoadDrain(attempt)
	if err != nil {
		h.t.Fatal(err)
	}
	return rec, ok
}

func (h *drainHarness) assertNoDockerCalls() {
	h.t.Helper()
	if len(h.docker.calls) != 0 {
		h.t.Fatalf("docker called: %v", h.docker.calls)
	}
}

func (h *drainHarness) assertPermanent(attempt int64, wantSummary string) state.PendingDrain {
	h.t.Helper()
	rec, ok := h.get(attempt)
	if !ok || rec.Phase != state.DrainStopFailed || rec.Retryable || !strings.Contains(rec.FailureSummary, wantSummary) {
		h.t.Fatalf("record = %+v ok=%v, want permanent stop_failed containing %q", rec, ok, wantSummary)
	}
	if len(rec.FailureSummary) > 2048 || !utf8.ValidString(rec.FailureSummary) {
		h.t.Fatalf("summary not bounded/valid")
	}
	if h.docker.count("stop") != 0 {
		h.t.Fatal("stop called")
	}
	return rec
}

func TestDrainSuccessStopsVerifiedContainerAndDeletesRecord(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(30 * time.Second)
	h.d.pass(context.Background())
	if _, ok := h.get(1); ok {
		t.Fatal("record not deleted after a successful drain")
	}
	if got := strings.Join(h.docker.calls, ","); got != "inspect,list,stop,wait" {
		t.Fatalf("calls = %s", got)
	}
	if len(h.docker.stopTarget) != 1 || h.docker.stopTarget[0] != drainTestTargetID+"/30s" {
		t.Fatalf("stop target = %v, want the verified container id with the 30s grace", h.docker.stopTarget)
	}
	if !strings.Contains(h.logs.String(), "predecessor stopped") {
		t.Fatalf("no success log: %s", h.logs)
	}
}

func TestDrainNotYetDueMakesNoDockerCallsAndReportsWait(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 20)
	h.advance(5 * time.Second)
	wait := h.d.pass(context.Background())
	h.assertNoDockerCalls()
	if wait != 15*time.Second {
		t.Fatalf("wait = %s, want 15s", wait)
	}
	if rec, ok := h.get(1); !ok || rec.Phase != state.DrainPending {
		t.Fatal("record changed")
	}
}

func TestDrainWaitIsCappedAtPollInterval(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 300)
	if wait := h.d.pass(context.Background()); wait != 30*time.Second {
		t.Fatalf("wait = %s, want the 30s poll cap", wait)
	}
}

func TestDrainRestartResumesRemainingTime(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(20 * time.Second)
	restarted := h.newDrainer() // a fresh process: no in-memory state
	if wait := restarted.pass(context.Background()); wait != 10*time.Second {
		t.Fatalf("wait = %s, want the remaining 10s", wait)
	}
	h.assertNoDockerCalls()
	h.advance(10 * time.Second)
	restarted.pass(context.Background())
	if _, ok := h.get(1); ok || h.docker.count("stop") != 1 {
		t.Fatal("drain did not run when the remaining time elapsed")
	}
}

func TestDrainExpiredWindowRunsImmediately(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(24 * time.Hour)
	h.newDrainer().pass(context.Background())
	if _, ok := h.get(1); ok || h.docker.count("stop") != 1 {
		t.Fatal("an expired window must drain immediately")
	}
}

func TestDrainClockJumpedBackNeverWaitsLongerThanDrainSeconds(t *testing.T) {
	h := newDrainHarness(t)
	h.d.PollInterval = 24 * time.Hour
	rec := h.arm(1, 120)
	h.now = h.t0.Add(-3 * time.Hour)
	remaining, due, tracked := h.d.dueIn(rec)
	if due || !tracked || remaining != 120*time.Second {
		t.Fatalf("remaining=%s due=%v tracked=%v, want 120s", remaining, due, tracked)
	}
	if wait := h.d.pass(context.Background()); wait != 120*time.Second {
		t.Fatalf("pass wait = %s, want 120s", wait)
	}
}

func TestDrainWallClockStepBackStillStopsWithinDrainSeconds(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 60)
	h.now = h.t0.Add(-6 * time.Hour) // wall clock stepped far back
	h.d.pass(context.Background())
	if h.docker.count("stop") != 0 {
		t.Fatal("stopped before the window")
	}
	h.mono = h.mono.Add(59 * time.Second)
	h.now = h.now.Add(59 * time.Second)
	h.d.pass(context.Background())
	if h.docker.count("stop") != 0 {
		t.Fatal("stopped early")
	}
	h.mono = h.mono.Add(2 * time.Second) // 61s of real elapsed time
	h.now = h.now.Add(2 * time.Second)   // wall clock is still hours before ArmedAt
	h.d.pass(context.Background())
	if _, ok := h.get(1); ok || h.docker.count("stop") != 1 {
		t.Fatalf("stop did not happen within DrainSeconds of the first pass; stops=%d", h.docker.count("stop"))
	}
}

func TestDrainRouteChangedIsPermanentAndNeverStops(t *testing.T) {
	cases := map[string]func(h *drainHarness){
		"different digest": func(h *drainHarness) { h.digest = routeDigestString([32]byte{9}) },
		"missing route":    func(h *drainHarness) { h.exists, h.digest = false, "" },
		"unreadable":       func(h *drainHarness) { h.rdErr = errors.New("permission denied") },
		"malformed digest": func(h *drainHarness) { h.digest = "garbage" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newDrainHarness(t)
			h.arm(1, 30)
			h.advance(time.Minute)
			mutate(h)
			h.d.pass(context.Background())
			h.assertNoDockerCalls()
			h.assertPermanent(1, "route changed since the switch; predecessor not stopped")
		})
	}
}

func TestDrainContainerGoneDeletesRecordWithoutStop(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	// The shape the real client returns for a 404 inspect.
	h.docker.inspectErr = &docker.EngineError{Class: docker.ErrClassConfiguration, Op: "container-inspect", Status: 404,
		Err: fmt.Errorf("%w: No such container", docker.ErrContainerNotFound)}
	h.d.pass(context.Background())
	if _, ok := h.get(1); ok {
		t.Fatal("record must be deleted: nothing to stop")
	}
	if h.docker.count("stop") != 0 || h.docker.count("list") != 0 {
		t.Fatalf("calls = %v", h.docker.calls)
	}
	if !strings.Contains(h.logs.String(), "INFO") {
		t.Fatal("no INFO log")
	}
}

func TestDrainLabelMismatchIsPermanentAndNeverStops(t *testing.T) {
	keys := []string{docker.LabelManaged, docker.LabelKind, docker.LabelEnvironmentID, docker.LabelReleaseID, docker.LabelRuntimeInstanceID}
	for _, key := range keys {
		for _, mode := range []string{"different", "missing"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				h := newDrainHarness(t)
				h.arm(1, 30)
				h.advance(time.Minute)
				labels := predecessorLabels()
				if mode == "missing" {
					delete(labels, key)
				} else {
					labels[key] = "other"
				}
				h.docker.list[0].Labels = labels
				h.d.pass(context.Background())
				h.assertPermanent(1, "identity mismatch; not stopped")
			})
		}
	}
}

func TestDrainIdentityShapeMismatchIsPermanent(t *testing.T) {
	cases := map[string]func(f *drainDockerFake){
		"no match":        func(f *drainDockerFake) { f.list = nil },
		"two matches":     func(f *drainDockerFake) { f.list = append(f.list, f.list[0]) },
		"different id":    func(f *drainDockerFake) { f.list[0].ID = strings.Repeat("b", 64) },
		"empty inspected": func(f *drainDockerFake) { f.inspect.ID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newDrainHarness(t)
			h.arm(1, 30)
			h.advance(time.Minute)
			mutate(h.docker)
			h.d.pass(context.Background())
			h.assertPermanent(1, "identity mismatch; not stopped")
		})
	}
}

func TestDrainRefusesContainerLabelledWithTheNewRelease(t *testing.T) {
	for name, set := range map[string]func(map[string]string){
		"release":  func(l map[string]string) { l[docker.LabelReleaseID] = drainTestNewRel },
		"instance": func(l map[string]string) { l[docker.LabelRuntimeInstanceID] = drainTestNewRT },
	} {
		t.Run(name, func(t *testing.T) {
			h := newDrainHarness(t)
			h.arm(1, 30)
			h.advance(time.Minute)
			set(h.docker.list[0].Labels)
			h.d.pass(context.Background())
			h.assertPermanent(1, "name the new release")
		})
	}
}

func TestDrainRecordNamingTheNewReleaseIsRefusedBeforeAnyIO(t *testing.T) {
	h := newDrainHarness(t)
	rec := h.arm(1, 30)
	h.advance(time.Minute)
	rec.PredecessorRuntimeInstanceID = rec.BackendRuntimeInstanceID // an in-memory corrupted copy
	h.d.process(context.Background(), rec)
	h.assertNoDockerCalls()
}

func TestDrainAlreadyStoppedDeletesWithoutStopCall(t *testing.T) {
	for _, status := range []string{"exited", "dead", "created"} {
		t.Run(status, func(t *testing.T) {
			h := newDrainHarness(t)
			h.arm(1, 30)
			h.advance(time.Minute)
			h.docker.inspect.State.Running, h.docker.inspect.State.Status = false, status
			h.d.pass(context.Background())
			if _, ok := h.get(1); ok || h.docker.count("stop") != 0 {
				t.Fatalf("calls=%v", h.docker.calls)
			}
		})
	}
}

func TestDrainRestartingOrPausedIsNotTreatedAsStopped(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.inspect.State.Running, h.docker.inspect.State.Restarting, h.docker.inspect.State.Status = false, true, "restarting"
	h.d.pass(context.Background())
	if h.docker.count("stop") != 1 {
		t.Fatal("a restarting container must still be stopped")
	}
}

func transientErr() error {
	return &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-stop", Err: errors.New("daemon busy")}
}

func TestDrainTransientStopErrorRetriesWithBackoffThenSucceeds(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.stopErrs = []error{transientErr()}
	h.d.pass(context.Background())
	rec, ok := h.get(1)
	if !ok || rec.Phase != state.DrainStopFailed || !rec.Retryable || rec.Attempts != 1 {
		t.Fatalf("record = %+v", rec)
	}
	// Before the backoff elapses nothing is called again. The store stamps
	// UpdatedAt from the real clock, so anchor the fake clock to it.
	h.now = rec.UpdatedAt.Add(10 * time.Second)
	if wait := h.d.pass(context.Background()); h.docker.count("stop") != 1 || wait > 30*time.Second || wait <= 0 {
		t.Fatalf("stops=%d wait=%s", h.docker.count("stop"), wait)
	}
	h.now = rec.UpdatedAt.Add(31 * time.Second)
	h.d.pass(context.Background())
	if _, ok := h.get(1); ok || h.docker.count("stop") != 2 {
		t.Fatalf("retry did not complete: stops=%d", h.docker.count("stop"))
	}
}

func TestDrainTransientErrorsExhaustToPermanent(t *testing.T) {
	h := newDrainHarness(t) // MaxAttempts = 3
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.stopErrs = []error{transientErr(), transientErr(), transientErr(), transientErr()}
	for i := 0; i < 3; i++ {
		h.d.pass(context.Background())
		h.advance(time.Hour)
	}
	rec, _ := h.get(1)
	if rec.Phase != state.DrainStopFailed || rec.Retryable || rec.Attempts != 3 || !strings.Contains(rec.FailureSummary, "gave up after 3 attempts") {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.Contains(h.logs.String(), "retries exhausted") {
		t.Fatal("exhaustion not logged loudly")
	}
	h.d.pass(context.Background())
	if h.docker.count("stop") != 3 {
		t.Fatalf("stop called %d times, want exactly MaxAttempts", h.docker.count("stop"))
	}
}

func TestDrainStopTimeoutIsRetried(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.waitErrs = []error{fmt.Errorf("%w: container still \"running\"", docker.ErrStopTimeout)}
	h.d.pass(context.Background())
	rec, _ := h.get(1)
	if rec.Phase != state.DrainStopFailed || !rec.Retryable || rec.Attempts != 1 {
		t.Fatalf("record = %+v", rec)
	}
	h.advance(time.Minute)
	h.d.pass(context.Background())
	if _, ok := h.get(1); ok {
		t.Fatal("retry after a timeout did not complete")
	}
}

func TestDrainConfigurationErrorIsPermanent(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.stopErrs = []error{&docker.EngineError{Class: docker.ErrClassConfiguration, Op: "container-stop", Status: 403, Err: errors.New("forbidden")}}
	h.d.pass(context.Background())
	rec, _ := h.get(1)
	if rec.Phase != state.DrainStopFailed || rec.Retryable {
		t.Fatalf("record = %+v", rec)
	}
	h.advance(time.Hour)
	h.d.pass(context.Background())
	if h.docker.count("stop") != 1 {
		t.Fatal("a permanent failure was retried")
	}
}

func TestDrainGoneDuringStopOrWaitDeletesRecord(t *testing.T) {
	gone := fmt.Errorf("%w: x", docker.ErrContainerNotFound)
	for name, mutate := range map[string]func(*drainDockerFake){
		"stop": func(f *drainDockerFake) { f.stopErrs = []error{gone} },
		"wait": func(f *drainDockerFake) { f.waitErrs = []error{gone} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newDrainHarness(t)
			h.arm(1, 30)
			h.advance(time.Minute)
			mutate(h.docker)
			h.d.pass(context.Background())
			if _, ok := h.get(1); ok {
				t.Fatal("record not deleted")
			}
		})
	}
}

func TestDrainLabelLookupTransientErrorRetries(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.listErr = transientErr()
	h.d.pass(context.Background())
	rec, _ := h.get(1)
	if rec.Phase != state.DrainStopFailed || !rec.Retryable || h.docker.count("stop") != 0 {
		t.Fatalf("record = %+v stops=%d", rec, h.docker.count("stop"))
	}
}

func TestDrainSummaryIsBoundedAndValidUTF8(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.stopErrs = []error{errors.New(strings.Repeat("é", 4000) + "\xff")}
	h.d.pass(context.Background())
	rec, _ := h.get(1)
	if rec.Phase != state.DrainStopFailed || len(rec.FailureSummary) > 2048 || !utf8.ValidString(rec.FailureSummary) {
		t.Fatalf("summary len=%d", len(rec.FailureSummary))
	}
}

func TestDrainCorruptFileIsNeverTouchedAndLogRateLimited(t *testing.T) {
	h := newDrainHarness(t)
	junk := filepath.Join(h.root, "drains", "attempt-0007.json")
	if err := os.WriteFile(junk, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.d.pass(context.Background())
	h.advance(time.Minute)
	h.d.pass(context.Background())
	if n := strings.Count(h.logs.String(), "attempt-0007.json"); n != 1 {
		t.Fatalf("logged %d times within the hour, want 1:\n%s", n, h.logs)
	}
	h.advance(2 * time.Hour)
	h.d.pass(context.Background())
	if n := strings.Count(h.logs.String(), "attempt-0007.json"); n != 2 {
		t.Fatalf("logged %d times after the interval, want 2", n)
	}
	if b, err := os.ReadFile(junk); err != nil || string(b) != "{not json" {
		t.Fatalf("corrupt file touched: %v %q", err, b)
	}
	h.assertNoDockerCalls()
}

type scanErrStore struct{ DrainStore }

func (scanErrStore) ScanDrains() ([]state.PendingDrain, []string, error) {
	return nil, nil, errors.New("dir unreadable")
}

func TestDrainScanErrorLogsAndRetriesLater(t *testing.T) {
	h := newDrainHarness(t)
	h.d.Store = scanErrStore{h.store}
	if wait := h.d.pass(context.Background()); wait != 30*time.Second {
		t.Fatalf("wait = %s", wait)
	}
	if !strings.Contains(h.logs.String(), "cannot scan pending drains") {
		t.Fatal("not logged")
	}
}

func TestDrainPermanentFailureIsLoggedRateLimitedAndNotRetried(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.digest = routeDigestString([32]byte{9})
	h.d.pass(context.Background())
	h.d.pass(context.Background())
	h.d.pass(context.Background())
	if n := strings.Count(h.logs.String(), "will not be retried automatically"); n != 1 {
		t.Fatalf("evidence log count = %d, want 1 within the hour\n%s", n, h.logs)
	}
}

func TestDrainRunLoopSleepsUntilDueThenDrains(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	h.d.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if _, ok := h.get(1); !ok {
			cancel()
			return ctx.Err()
		}
		h.advance(d)
		return nil
	}
	if err := h.d.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if len(waits) < 2 || waits[0] != 20*time.Second {
		t.Fatalf("waits = %v, want first sleep of 20s", waits)
	}
	if h.docker.count("stop") != 1 {
		t.Fatalf("stops = %d", h.docker.count("stop"))
	}
}

func TestDrainRunReturnsPromptlyOnCancelWhileIdle(t *testing.T) {
	h := newDrainHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel (goroutine leak)")
	}
}

func TestDrainCancelMidStopLeavesRecordPending(t *testing.T) {
	h := newDrainHarness(t)
	h.arm(1, 30)
	h.advance(time.Minute)
	h.docker.stopBlock = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for h.docker.count("stop") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stop never called")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly while a stop was in flight")
	}
	if rec, ok := h.get(1); !ok || rec.Phase != state.DrainPending || rec.Attempts != 0 {
		t.Fatalf("a cancelled stop must not be recorded as a failure: %+v", rec)
	}
}

func TestDrainRunRejectsInvalidConfiguration(t *testing.T) {
	h := newDrainHarness(t)
	for name, mutate := range map[string]func(*Drainer){
		"nil store":       func(d *Drainer) { d.Store = nil },
		"nil docker":      func(d *Drainer) { d.Docker = nil },
		"no route dir":    func(d *Drainer) { d.RouteDir = "" },
		"fractional":      func(d *Drainer) { d.StopGrace = 1500 * time.Millisecond },
		"grace too large": func(d *Drainer) { d.StopGrace = 301 * time.Second },
	} {
		d := h.newDrainer()
		mutate(d)
		if err := d.Run(context.Background()); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestDrainerValidateIsExported(t *testing.T) {
	h := newDrainHarness(t)
	if err := h.newDrainer().Validate(); err != nil {
		t.Fatal(err)
	}
	d := h.newDrainer()
	d.StopGrace = 500 * time.Millisecond
	if err := d.Validate(); err == nil {
		t.Fatal("invalid grace accepted")
	}
}

func TestDrainerDefaults(t *testing.T) {
	d := &Drainer{}
	if d.stopGrace() != 30*time.Second || d.waitTimeout() != time.Minute || d.maxAttempts() != 5 || d.pollInterval() != 30*time.Second {
		t.Fatal("defaults")
	}
	wants := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, want := range wants {
		if got := d.backoff(i + 1); got != want {
			t.Fatalf("backoff(%d) = %s, want %s", i+1, got, want)
		}
	}
}

// The drainer must never be able to remove or kill: its Docker seam exposes
// only inspect, label lookup, stop and wait.
func TestDrainDockerSeamHasNoRemoveOrKill(t *testing.T) {
	typ := reflect.TypeOf((*DrainDocker)(nil)).Elem()
	want := map[string]bool{"InspectContainer": true, "ListContainersByLabels": true, "StopContainer": true, "WaitStopped": true}
	if typ.NumMethod() != len(want) {
		t.Fatalf("seam has %d methods, want %d", typ.NumMethod(), len(want))
	}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !want[name] || strings.Contains(strings.ToLower(name), "remove") || strings.Contains(strings.ToLower(name), "kill") {
			t.Fatalf("unexpected seam method %q", name)
		}
	}
}

func TestNewExecutorRejectsNilDrains(t *testing.T) {
	orig := probeBuildxFunc
	probeBuildxFunc = func(context.Context) error { return nil }
	defer func() { probeBuildxFunc = orig }()
	act := testActivationConfig()
	act.Drains = nil
	_, err := NewExecutor(context.Background(), Config{
		ControlPlane: &fakeControlPlane{}, Docker: &fakeDocker{}, CheckoutRunner: fakeCheckoutRunner{},
		ServerID: "server1", Network: "env-network", Activation: act,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete activation configuration") {
		t.Fatalf("err = %v", err)
	}
}
