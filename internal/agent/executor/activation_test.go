package executor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
	"autodeploy/internal/runtime/dockerid"
	"autodeploy/internal/runtime/traefik"
)

type reportCall struct {
	operationID string
	result      agentclient.ActivationResult
}

// actFake implements every activation seam in memory.
type actFake struct {
	mu    sync.Mutex
	lease agentclient.Lease
	resp  agentclient.ActivateResponse

	activateErrs []error // one per call; exhausted -> success
	activateN    int
	onActivated  func()

	reports    []reportCall
	reportErr  error
	reportErrs []error // per call; exhausted -> reportErr

	saved       []state.PendingActivation
	saveErrOn   map[int]error // 1-based call number
	saveCalls   int
	saveErrFrom int // when >0, every call number >= this fails

	stored    int64
	loadErr   error
	recordErr error
	recorded  []int64

	verifyErr error
	verifyN   int
	probeErr  error
	probeN    int

	writes    []traefik.EnvironmentRoute
	writeErr  error
	disk      string // current on-disk digest
	diskAfter string // when set, overrides what is on disk after a write
	readErr   error

	kicks int

	// ordering violations observed inside the write seam.
	wroteBeforePersist bool
	wroteBeforeRecord  bool
	probeCtxErr        error
}

func newActFake(lease agentclient.Lease) *actFake {
	resp := agentclient.ActivateResponse{
		OperationID: "9001", RouteRevision: 5, Hostname: "app.example.com",
		EntrypointName: traefik.DefaultEntrypointName, CertResolverName: traefik.DefaultCertResolverName,
		BackendRuntimeInstanceID: dockerid.AttemptRuntimeInstanceID(lease.AttemptID),
	}
	f := &actFake{lease: lease, resp: resp}
	_, digest, err := traefik.Render(f.route(resp))
	if err != nil {
		panic(err)
	}
	f.resp.RouteDigest, _ = parseRouteDigestString(digest)
	return f
}

func (f *actFake) route(resp agentclient.ActivateResponse) traefik.EnvironmentRoute {
	return traefik.EnvironmentRoute{
		EnvironmentID: f.lease.EnvironmentID, RouteRevision: uint64(resp.RouteRevision), Hostname: resp.Hostname,
		EntrypointName: resp.EntrypointName, CertResolverName: resp.CertResolverName,
		BackendRuntimeInstanceID: resp.BackendRuntimeInstanceID, BackendPort: f.lease.ContainerPort,
		BackendScheme: f.lease.ContainerScheme, ReleaseID: f.lease.ReleaseID,
		FenceID: strconv.FormatInt(f.lease.Fence, 10),
	}
}

func (f *actFake) Activate(context.Context, int64, int64, [32]byte) (agentclient.ActivateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activateN++
	if f.activateN <= len(f.activateErrs) && f.activateErrs[f.activateN-1] != nil {
		return agentclient.ActivateResponse{}, f.activateErrs[f.activateN-1]
	}
	if f.onActivated != nil {
		f.onActivated()
	}
	return f.resp, nil
}

func (f *actFake) ReportActivationResult(_ context.Context, _, _ int64, op string, r agentclient.ActivationResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, reportCall{op, r})
	if n := len(f.reports); n <= len(f.reportErrs) {
		return f.reportErrs[n-1]
	}
	return f.reportErr
}

func (f *actFake) SaveActivation(p state.PendingActivation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls++
	n := f.saveCalls
	if err := f.saveErrOn[n]; err != nil {
		return err
	}
	if f.saveErrFrom > 0 && n >= f.saveErrFrom {
		return errors.New("disk full")
	}
	f.saved = append(f.saved, p)
	return nil
}

func (f *actFake) LoadRouteRevision(string) (int64, error) { return f.stored, f.loadErr }
func (f *actFake) RecordRouteRevision(_ string, rev int64) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, rev)
	return nil
}

func (f *actFake) Verify(context.Context, traefik.EnvironmentRoute) (traefik.VerifyResult, error) {
	f.verifyN++
	return traefik.VerifyResult{}, f.verifyErr
}
func (f *actFake) Kick() { f.kicks++ }

func (f *actFake) runner() *activationRunner {
	a := &activationRunner{
		cfg: ActivationConfig{
			Client: f, Observer: f, Store: f, Revisions: f, Kicker: f,
			RouteDir: "/route-dir", ProbeAddress: "127.0.0.1:443", ProbePath: "/", ProbeTimeout: time.Second,
		},
		write: func(_ string, r traefik.EnvironmentRoute) (traefik.WriteResult, error) {
			if len(f.saved) == 0 {
				f.wroteBeforePersist = true
			}
			if len(f.recorded) == 0 {
				f.wroteBeforeRecord = true
			}
			if f.writeErr != nil {
				return traefik.WriteResult{}, f.writeErr
			}
			f.writes = append(f.writes, r)
			_, d, _ := traefik.Render(r)
			f.disk = d
			if f.diskAfter != "" {
				f.disk = f.diskAfter
			}
			return traefik.WriteResult{Digest: f.disk}, nil
		},
		readDigest: func(string, string) (string, bool, error) {
			if f.readErr != nil {
				return "", false, f.readErr
			}
			return f.disk, f.disk != "", nil
		},
		probe: func(ctx context.Context, _ traefik.ProbeConfig) (traefik.ProbeResult, error) {
			f.probeN++
			f.probeCtxErr = ctx.Err()
			return traefik.ProbeResult{Attempts: 2, LastStatus: 502, Reason: "status 502"}, f.probeErr
		},
		activateAttempts: 3, activateBackoff: time.Millisecond, saveBackoff: time.Millisecond, reportAttempts: 5, reportBackoff: time.Millisecond, verifyInterval: time.Millisecond,
		timeout: 5 * time.Second, verifyTimeout: 50 * time.Millisecond, logger: testLogger(),
	}
	return a
}

func (f *actFake) executor() *Executor {
	return &Executor{activation: f.runner(), logger: testLogger()}
}

var testImageDigest = "sha256:" + ones(64)

func (f *actFake) last(t *testing.T) state.PendingActivation {
	t.Helper()
	if len(f.saved) == 0 {
		t.Fatal("no activation record persisted")
	}
	return f.saved[len(f.saved)-1]
}

// assertHandedOffFailure checks a result_ready failure whose summary holds
// every substring, a handed-off outcome, a kick and no direct report.
func assertHandedOffFailure(t *testing.T, f *actFake, out agentclient.Outcome, wantInSummary ...string) state.PendingActivation {
	t.Helper()
	if !out.HandedOff || out.Succeeded || out.AuthorizationUnknown {
		t.Fatalf("outcome = %+v, want handed off", out)
	}
	rec := f.last(t)
	if rec.Phase != state.PhaseResultReady || rec.Succeeded {
		t.Fatalf("record = %+v, want result_ready failure", rec)
	}
	for _, w := range wantInSummary {
		if !strings.Contains(rec.FailureSummary, w) {
			t.Fatalf("summary %q lacks %q", rec.FailureSummary, w)
		}
	}
	if len(rec.FailureSummary) > 2048 || !utf8.ValidString(rec.FailureSummary) {
		t.Fatalf("summary not bounded/valid: %d bytes", len(rec.FailureSummary))
	}
	if f.kicks != 1 || len(f.reports) != 0 {
		t.Fatalf("kicks=%d direct reports=%d, want 1 and 0", f.kicks, len(f.reports))
	}
	return rec
}

func TestActivateSuccess(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	out := f.executor().activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || out.Succeeded || out.FailureClass != "" || out.OperationID != "9001" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(f.saved) != 2 || f.saved[0].Phase != state.PhaseAuthorized {
		t.Fatalf("saved = %+v, want authorized then result_ready", f.saved)
	}
	rec := f.last(t)
	if rec.Phase != state.PhaseResultReady || !rec.Succeeded || rec.ObservedRouteDigest != f.resp.RouteDigest {
		t.Fatalf("record = %+v", rec)
	}
	if f.wroteBeforePersist || f.wroteBeforeRecord || len(f.writes) != 1 {
		t.Fatalf("ordering: beforePersist=%v beforeRecord=%v writes=%d", f.wroteBeforePersist, f.wroteBeforeRecord, len(f.writes))
	}
	if f.kicks != 1 || len(f.reports) != 0 {
		t.Fatalf("kicks=%d reports=%d", f.kicks, len(f.reports))
	}
	if rec.RouteRevision != 5 || rec.OperationID != "9001" || rec.Fence != lease.Fence || rec.EnvironmentID != lease.EnvironmentID {
		t.Fatalf("record fields = %+v", rec)
	}
}

func TestActivateNilKickerIsSafe(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	e := f.executor()
	e.activation.cfg.Kicker = nil
	if out := e.activate(context.Background(), lease, testImageDigest); !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestActivateDefiniteRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want queue.FailureClass
	}{
		{"stale_lease", agentclient.ErrStaleLease, queue.FailureClassCorruptState},
		{agentclient.ConflictCandidateNotReady, &agentclient.ConflictError{Code: agentclient.ConflictCandidateNotReady}, queue.FailureClassTransient},
		{agentclient.ConflictNoRouteHead, &agentclient.ConflictError{Code: agentclient.ConflictNoRouteHead}, queue.FailureClassPermanentConfiguration},
		{agentclient.ConflictActivationOpen, &agentclient.ConflictError{Code: agentclient.ConflictActivationOpen}, queue.FailureClassCorruptState},
		{agentclient.ConflictEnvironmentBlocked, &agentclient.ConflictError{Code: agentclient.ConflictEnvironmentBlocked}, queue.FailureClassCorruptState},
		{agentclient.ConflictCorruptState, &agentclient.ConflictError{Code: agentclient.ConflictCorruptState}, queue.FailureClassCorruptState},
		{"unknown code", &agentclient.ConflictError{Code: "something_new"}, queue.FailureClassCorruptState},
		{"empty code", &agentclient.ConflictError{}, queue.FailureClassCorruptState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lease := testLease()
			f := newActFake(lease)
			f.activateErrs = []error{tc.err}
			out := f.executor().activate(context.Background(), lease, testImageDigest)
			if out.Succeeded || out.HandedOff || out.AuthorizationUnknown || out.FailureClass != tc.want {
				t.Fatalf("outcome = %+v, want failed %s (Complete path)", out, tc.want)
			}
			if f.activateN != 1 {
				t.Fatalf("Activate called %d times, a 409 is definite and must not be replayed", f.activateN)
			}
			assertUntouched(t, f)
		})
	}
}

func assertUntouched(t *testing.T, f *actFake) {
	t.Helper()
	if len(f.writes) != 0 || len(f.saved) != 0 || len(f.reports) != 0 || len(f.recorded) != 0 || f.kicks != 0 {
		t.Fatalf("host/state touched: writes=%d saved=%d reports=%d recorded=%d kicks=%d", len(f.writes), len(f.saved), len(f.reports), len(f.recorded), f.kicks)
	}
}

func TestActivateUnknownRetriesThenNothingWritten(t *testing.T) {
	for _, unknown := range []error{agentclient.ErrServiceUnavailable, agentclient.ErrTransportFailure, agentclient.ErrMalformedResponse, errors.New("decode")} {
		t.Run(unknown.Error(), func(t *testing.T) {
			lease := testLease()
			f := newActFake(lease)
			f.activateErrs = []error{unknown, unknown, unknown, unknown}
			out := f.executor().activate(context.Background(), lease, testImageDigest)
			if !out.AuthorizationUnknown || out.Succeeded || out.HandedOff {
				t.Fatalf("outcome = %+v, want authorization unknown", out)
			}
			if f.activateN != 3 {
				t.Fatalf("Activate called %d times, want 3 (activateAttempts)", f.activateN)
			}
			assertUntouched(t, f)
		})
	}
}

func TestActivateUnknownThenReplaySucceeds(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.activateErrs = []error{agentclient.ErrTransportFailure, agentclient.ErrServiceUnavailable}
	out := f.executor().activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || f.activateN != 3 || !f.last(t).Succeeded {
		t.Fatalf("outcome=%+v activateN=%d", out, f.activateN)
	}
}

func TestActivateUnknownThenLater409IsStillUnknown(t *testing.T) {
	for _, code := range []string{agentclient.ConflictActivationOpen, agentclient.ConflictCandidateNotReady} {
		t.Run(code, func(t *testing.T) {
			lease := testLease()
			f := newActFake(lease)
			f.activateErrs = []error{agentclient.ErrTransportFailure, &agentclient.ConflictError{Code: code}}
			out := f.executor().activate(context.Background(), lease, testImageDigest)
			if !out.AuthorizationUnknown || out.FailureClass != "" || out.Succeeded || out.HandedOff || f.activateN != 2 {
				t.Fatalf("outcome=%+v activateN=%d", out, f.activateN)
			}
			assertUntouched(t, f)
		})
	}
	t.Run("stale_lease", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.activateErrs = []error{agentclient.ErrServiceUnavailable, agentclient.ErrStaleLease}
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		if !out.AuthorizationUnknown {
			t.Fatalf("outcome=%+v", out)
		}
		assertUntouched(t, f)
	})
}

func TestActivateBlockedInFlightUntilCancelIsUnknown(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	e := f.executor()
	e.activation.cfg.Client = blockingClient{f}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	out := e.activate(ctx, lease, testImageDigest)
	if !out.AuthorizationUnknown || out.Succeeded || out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	assertUntouched(t, f)
}

type blockingClient struct{ *actFake }

func (blockingClient) Activate(ctx context.Context, _, _ int64, _ [32]byte) (agentclient.ActivateResponse, error) {
	<-ctx.Done()
	return agentclient.ActivateResponse{}, ctx.Err()
}

func TestActivateCtxCancelledDuringRetryIsUnknown(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.activateErrs = []error{agentclient.ErrTransportFailure, agentclient.ErrTransportFailure, agentclient.ErrTransportFailure}
	ctx, cancel := context.WithCancel(context.Background())
	e := f.executor()
	e.activation.activateBackoff = time.Hour
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	out := e.activate(ctx, lease, testImageDigest)
	if !out.AuthorizationUnknown {
		t.Fatalf("outcome = %+v, want unknown", out)
	}
	assertUntouched(t, f)
}

func TestActivateCtxAlreadyCancelledNeverCallsActivate(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := f.executor().activate(ctx, lease, testImageDigest)
	if out.FailureClass != queue.FailureClassTransient || f.activateN != 0 {
		t.Fatalf("outcome=%+v activateN=%d", out, f.activateN)
	}
}

func TestActivateMalformedImageDigestIsPreAuthorizationFailure(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	out := f.executor().activate(context.Background(), lease, "sha256:zz")
	if out.FailureClass != queue.FailureClassCorruptState || f.activateN != 0 {
		t.Fatalf("outcome=%+v activateN=%d", out, f.activateN)
	}
}

func TestActivatePersistFailureReportsDirectlyAndWritesNothing(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrOn = map[int]error{1: errors.New("disk full")}
	out := f.executor().activate(context.Background(), lease, testImageDigest)
	if !out.AuthorizationUnknown || out.Succeeded || out.HandedOff {
		t.Fatalf("outcome = %+v, want no-Complete outcome", out)
	}
	if len(f.writes) != 0 || len(f.recorded) != 0 || f.kicks != 0 {
		t.Fatalf("route touched: writes=%d recorded=%d kicks=%d", len(f.writes), len(f.recorded), f.kicks)
	}
	if len(f.reports) != 1 || f.reports[0].operationID != "9001" {
		t.Fatalf("direct reports = %+v, want exactly one for operation 9001", f.reports)
	}
}

func TestActivatePersistFailureDirectReportFailureStillNoComplete(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrOn = map[int]error{1: errors.New("disk full")}
	f.reportErr = agentclient.ErrTransportFailure
	out := f.executor().activate(context.Background(), lease, testImageDigest)
	if !out.AuthorizationUnknown || len(f.writes) != 0 {
		t.Fatalf("outcome=%+v writes=%d", out, len(f.writes))
	}
}

func TestActivateResultSaveRetriesThenSucceeds(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrOn = map[int]error{2: errors.New("busy"), 3: errors.New("busy")}
	out := f.executor().activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || len(f.reports) != 0 || f.kicks != 1 {
		t.Fatalf("outcome=%+v reports=%d kicks=%d", out, len(f.reports), f.kicks)
	}
	if rec := f.last(t); rec.Phase != state.PhaseResultReady || !rec.Succeeded {
		t.Fatalf("record = %+v", rec)
	}
}

func TestActivateResultSaveFailsDirectlyReportsRealSuccess(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrFrom = 2
	out := f.executor().activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || len(f.reports) != 1 || f.kicks != 1 {
		t.Fatalf("outcome=%+v reports=%d kicks=%d", out, len(f.reports), f.kicks)
	}
	want := agentclient.ActivationSucceeded(f.resp.RouteDigest)
	if f.reports[0].result != want {
		t.Fatalf("direct report = %+v, want a success with the on-disk digest", f.reports[0].result)
	}
	if f.last(t).Phase != state.PhaseAuthorized {
		t.Fatal("authorized record must remain")
	}
}

func TestActivateResultSaveFailsDirectlyReportsRealFailure(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrFrom = 2
	f.probeErr = traefik.ErrProbeTimeout
	f.executor().activate(context.Background(), lease, testImageDigest)
	if len(f.reports) != 1 {
		t.Fatalf("reports=%d", len(f.reports))
	}
	if f.reports[0].result == agentclient.ActivationSucceeded(f.resp.RouteDigest) {
		t.Fatal("failure reported as success")
	}
}

func TestActivateDirectReportRetriesTransientThenSucceeds(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrFrom = 2
	f.reportErrs = []error{agentclient.ErrTransportFailure, &agentclient.ConflictError{Code: "corrupt_state"}, nil}
	f.executor().activate(context.Background(), lease, testImageDigest)
	if len(f.reports) != 3 {
		t.Fatalf("reports=%d, want 3 (transient, non-terminal 409, then 200)", len(f.reports))
	}
}

func TestActivateDirectReportStopsOnTerminalConflict(t *testing.T) {
	for _, code := range []string{agentclient.ConflictLateReportRecorded, agentclient.ConflictRouteMoved, agentclient.ConflictIntegrity} {
		lease := testLease()
		f := newActFake(lease)
		f.saveErrFrom = 2
		f.reportErrs = []error{&agentclient.ConflictError{Code: code}}
		f.executor().activate(context.Background(), lease, testImageDigest)
		if len(f.reports) != 1 {
			t.Fatalf("%s: reports=%d, want 1", code, len(f.reports))
		}
	}
}

func TestActivateSaveAndDirectReportBothFailLeavesAuthorizedAndLogs(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	f.saveErrFrom = 2
	f.reportErr = agentclient.ErrTransportFailure
	var buf strings.Builder
	e := f.executor()
	e.activation.logger = log.New(&buf, "", 0)
	out := e.activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff || len(f.reports) != 5 {
		t.Fatalf("outcome=%+v reports=%d", out, len(f.reports))
	}
	if f.last(t).Phase != state.PhaseAuthorized {
		t.Fatal("authorized record must remain")
	}
	if !strings.Contains(buf.String(), "stays on disk for startup recovery") {
		t.Fatalf("no loud log: %q", buf.String())
	}
}

func TestActivateDirectReportStillAttemptedWhenDetachedCtxExpired(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	e := f.executor()
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()
	rec := state.PendingActivation{AttemptID: lease.AttemptID, Fence: lease.Fence, OperationID: "9001"}
	e.activation.reportDirect(expired, rec, agentclient.ActivationSucceeded(f.resp.RouteDigest))
	if len(f.reports) != 1 {
		t.Fatalf("reports=%d, want the report attempted on its own deadline", len(f.reports))
	}
}

func TestActivatePostAuthorizationFailures(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.resp.RouteDigest[0] ^= 0xff
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "render", "authorized route digest: sha256:"+fmt.Sprintf("%x", f.resp.RouteDigest[:]), "on-disk route digest: absent")
		assertNoWrite(t, f)
	})
	t.Run("wrong entrypoint", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.resp.EntrypointName = "web"
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "entrypoint-check")
		assertNoWrite(t, f)
	})
	t.Run("wrong cert resolver", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.resp.CertResolverName = "other"
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "entrypoint-check")
		assertNoWrite(t, f)
	})
	t.Run("revision regression", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.stored = 9
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "revision-guard", "below the stored revision 9")
		assertNoWrite(t, f)
		if len(f.recorded) != 0 {
			t.Fatalf("regressed revision recorded: %v", f.recorded)
		}
	})
	t.Run("revision equal is allowed (replay)", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.stored = 5
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		if !out.HandedOff || !f.last(t).Succeeded {
			t.Fatalf("outcome=%+v record=%+v", out, f.last(t))
		}
	})
	t.Run("revision load error", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.loadErr = errors.New("corrupt revision file")
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "revision-guard", "corrupt revision file")
		assertNoWrite(t, f)
	})
	t.Run("revision record error", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.recordErr = errors.New("read-only fs")
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "revision-guard", "read-only fs")
		assertNoWrite(t, f)
	})
	t.Run("write error", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.writeErr = errors.New("rename failed")
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "route-write", "rename failed", "route not rolled back")
		if f.verifyN != 0 || f.probeN != 0 {
			t.Fatalf("verify=%d probe=%d after a failed write", f.verifyN, f.probeN)
		}
	})
	t.Run("observer failure never rolls back", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.verifyErr = traefik.ErrWrongBackend
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "observer-verify", "loaded service backend", "on-disk route digest: sha256:")
		if len(f.writes) != 1 || f.probeN != 0 {
			t.Fatalf("writes=%d probe=%d: route must stay written, probe skipped", len(f.writes), f.probeN)
		}
	})
	t.Run("observer not loaded polls then fails", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.verifyErr = traefik.ErrRouterNotLoaded
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "observer-verify")
		if f.verifyN < 2 {
			t.Fatalf("verify polled %d times, want repeated polling while not loaded", f.verifyN)
		}
	})
	t.Run("probe failure never rolls back", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.probeErr = traefik.ErrProbeTimeout
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "data-plane-probe", "attempts=2", "last_status=502", "on-disk route digest: sha256:")
		if len(f.writes) != 1 {
			t.Fatalf("writes=%d, want exactly the one forward write (no rollback)", len(f.writes))
		}
	})
	t.Run("on-disk digest differs", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		f.diskAfter = "sha256:" + zeros(64)
		out := f.executor().activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "on-disk-check", "on-disk route digest: sha256:"+zeros(64))
		if len(f.writes) != 1 {
			t.Fatalf("writes=%d", len(f.writes))
		}
	})
	t.Run("on-disk unreadable after probe", func(t *testing.T) {
		lease := testLease()
		f := newActFake(lease)
		e := f.executor()
		real := e.activation.readDigest
		reads := 0
		e.activation.readDigest = func(d, id string) (string, bool, error) {
			reads++
			if reads >= 1 {
				return "", false, errors.New("EIO")
			}
			return real(d, id)
		}
		out := e.activate(context.Background(), lease, testImageDigest)
		assertHandedOffFailure(t, f, out, "on-disk-check", "on-disk route digest: unreadable")
	})
}

func assertNoWrite(t *testing.T, f *actFake) {
	t.Helper()
	if len(f.writes) != 0 || f.verifyN != 0 || f.probeN != 0 {
		t.Fatalf("route work happened: writes=%d verify=%d probe=%d", len(f.writes), f.verifyN, f.probeN)
	}
}

func TestActivateCancelAfterActivateStillCompletes(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	ctx, cancel := context.WithCancel(context.Background())
	f.onActivated = cancel // lease cancelled the instant Activate succeeds
	out := f.executor().activate(ctx, lease, testImageDigest)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	rec := f.last(t)
	if rec.Phase != state.PhaseResultReady || !rec.Succeeded || len(f.writes) != 1 {
		t.Fatalf("record=%+v writes=%d: sequence must finish despite cancellation", rec, len(f.writes))
	}
	if f.probeCtxErr != nil {
		t.Fatalf("probe ran under a cancelled context: %v", f.probeCtxErr)
	}
}

func TestActivateDetachedDeadlineBoundsTheSequence(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	e := f.executor()
	e.activation.timeout = 30 * time.Millisecond
	e.activation.probe = func(ctx context.Context, _ traefik.ProbeConfig) (traefik.ProbeResult, error) {
		<-ctx.Done()
		return traefik.ProbeResult{}, ctx.Err()
	}
	done := make(chan agentclient.Outcome, 1)
	go func() { done <- e.activate(context.Background(), lease, testImageDigest) }()
	select {
	case out := <-done:
		assertHandedOffFailure(t, f, out, "data-plane-probe")
	case <-time.After(3 * time.Second):
		t.Fatal("activate did not return: detached work has no deadline")
	}
}

func TestExecuteFailsClosedWithoutActivationConfig(t *testing.T) {
	cp := &fakeControlPlane{}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{}
	e := newTestExecutor(cp, co, dk, fakeBuildFunc(zeroBuild(), nil, nil))
	e.activation = nil
	out := e.Execute(context.Background(), testLease())
	if out.Succeeded || out.HandedOff || out.FailureClass != queue.FailureClassPermanentConfiguration {
		t.Fatalf("outcome = %+v", out)
	}
	if co.checkoutN != 0 || dk.ensureN != 0 || len(cp.progressCalls) != 0 {
		t.Fatal("work started without activation config")
	}
}

func TestNewExecutorRejectsIncompleteActivationConfig(t *testing.T) {
	if _, err := newActivationRunner(ActivationConfig{}, testLogger()); err == nil {
		t.Fatal("empty activation config accepted")
	}
	f := newActFake(testLease())
	cfg := f.runner().cfg
	if _, err := newActivationRunner(cfg, testLogger()); err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = cfg.ProbeTimeout + time.Second
	if _, err := newActivationRunner(cfg, testLogger()); err == nil {
		t.Fatal("explicit timeout without margin accepted")
	}
	cfg.Timeout = cfg.ProbeTimeout + defaultVerifyTimeout + minTimeoutMargin
	if _, err := newActivationRunner(cfg, testLogger()); err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 0
	cfg.ProbeTimeout = 0
	if _, err := newActivationRunner(cfg, testLogger()); err == nil {
		t.Fatal("zero probe timeout accepted")
	}
}

func TestSummaryHelperBoundsOnRuneBoundary(t *testing.T) {
	reason := strings.Repeat("é", 3000) + "\xff\xfe"
	s := buildFailureSummary("route-write", reason, "sha256:"+zeros(64), "sha256:"+ones(64))
	if len(s) > 2048 || !utf8.ValidString(s) || s == "" {
		t.Fatalf("len=%d valid=%v", len(s), utf8.ValidString(s))
	}
	if !strings.Contains(s, "authorized route digest: sha256:"+ones(64)) {
		t.Fatal("digest evidence lost to truncation")
	}
	for max := 1; max < 12; max++ {
		out := truncateUTF8("a€b€c€", max)
		if len(out) > max || !utf8.ValidString(out) {
			t.Fatalf("max=%d out=%q", max, out)
		}
	}
	if got := truncateUTF8("a\xffb", 10); got != "a?b" {
		t.Fatalf("invalid byte not replaced: %q", got)
	}
}

func TestActivateWithRealStoresAndRouteDir(t *testing.T) {
	lease := testLease()
	f := newActFake(lease)
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acts, err := store.Activations()
	if err != nil {
		t.Fatal(err)
	}
	revs, err := store.RouteRevisions()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := f.executor()
	e.activation.cfg.Store, e.activation.cfg.Revisions, e.activation.cfg.RouteDir = acts, revs, dir
	e.activation.write, e.activation.readDigest = traefik.Write, traefik.ReadDigest

	out := e.activate(context.Background(), lease, testImageDigest)
	if !out.HandedOff {
		t.Fatalf("outcome = %+v", out)
	}
	rec, ok, err := acts.LoadActivation(lease.AttemptID)
	if err != nil || !ok || rec.Phase != state.PhaseResultReady || !rec.Succeeded || rec.ObservedRouteDigest != f.resp.RouteDigest {
		t.Fatalf("rec=%+v ok=%v err=%v", rec, ok, err)
	}
	if rev, err := revs.LoadRouteRevision(lease.EnvironmentID); err != nil || rev != 5 {
		t.Fatalf("revision=%d err=%v", rev, err)
	}
	d, exists, err := traefik.ReadDigest(dir, lease.EnvironmentID)
	if err != nil || !exists || d != routeDigestString(f.resp.RouteDigest) {
		t.Fatalf("on disk %q exists=%v err=%v", d, exists, err)
	}
}

func zeroBuild() (r docker.BuildResult) { return r }
