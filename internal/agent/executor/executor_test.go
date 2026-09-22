package executor

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	agentcheckout "autodeploy/internal/agent/checkout"
	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/deployment"
	"autodeploy/internal/gitcheckout"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

// --- fakes: one per seam (checkout, materializer+progress via ControlPlane,
// docker client, build func) -- reusing the struct-of-closures fake pattern
// internal/runtime/docker's own tests (run_test.go, health_test.go) use. ---

type fakeControlPlane struct {
	secrets    []agentclient.MaterializedSecret
	secretsErr error
	secretsN   int

	progressCalls []deployment.Status
	progressErrOn map[deployment.Status]error
}

func (f *fakeControlPlane) CheckoutBegin(context.Context, int64, int64) (agentcheckout.Reservation, error) {
	return agentcheckout.Reservation{}, errors.New("unused in these tests: checkout goes through the CheckoutSeam fake")
}
func (f *fakeControlPlane) CheckoutStart(context.Context, int64, int64, string) (agentcheckout.Credential, error) {
	return agentcheckout.Credential{}, errors.New("unused")
}
func (f *fakeControlPlane) CheckoutClose(context.Context, int64, int64, string) error { return nil }

func (f *fakeControlPlane) MaterializeSecrets(ctx context.Context, attemptID, fence int64) ([]agentclient.MaterializedSecret, error) {
	f.secretsN++
	return f.secrets, f.secretsErr
}

func (f *fakeControlPlane) ReportProgress(ctx context.Context, attemptID, fence int64, phase deployment.Status) error {
	f.progressCalls = append(f.progressCalls, phase)
	if f.progressErrOn != nil {
		if err, ok := f.progressErrOn[phase]; ok {
			return err
		}
	}
	return nil
}

type fakeCheckout struct {
	result    CheckoutResult
	err       error
	block     bool
	closeN    int
	closeErr  error
	checkoutN int
}

func (f *fakeCheckout) Checkout(ctx context.Context, attemptID, fence int64) (CheckoutResult, error) {
	f.checkoutN++
	if f.block {
		<-ctx.Done()
		return CheckoutResult{}, ctx.Err()
	}
	if f.err != nil {
		return CheckoutResult{}, f.err
	}
	r := f.result
	r.Close = func(context.Context) error {
		f.closeN++
		return f.closeErr
	}
	return r, nil
}

type fakeDocker struct {
	probeErr error

	ensureResult docker.ContainerInspect
	ensureErr    error
	ensureN      int
	lastRunReq   docker.RunContainerRequest

	gateResult docker.HealthGateResult
	gateErr    error
	gateN      int
	blockGate  bool
	lastTarget docker.HealthGateTarget
	lastNetID  string
}

func (f *fakeDocker) Probe(ctx context.Context) (docker.VersionInfo, error) {
	return docker.VersionInfo{}, f.probeErr
}

func (f *fakeDocker) EnsureReleaseRuntimeContainer(ctx context.Context, req docker.RunContainerRequest, networkNameOrID string) (docker.ContainerInspect, error) {
	f.ensureN++
	f.lastRunReq = req
	return f.ensureResult, f.ensureErr
}

func (f *fakeDocker) RunHealthGate(ctx context.Context, containerID, networkID string, healthPolicyRevision int, target docker.HealthGateTarget) (docker.HealthGateResult, error) {
	f.gateN++
	f.lastTarget = target
	f.lastNetID = networkID
	if f.blockGate {
		<-ctx.Done()
		return docker.HealthGateResult{}, ctx.Err()
	}
	return f.gateResult, f.gateErr
}

func fakeBuildFunc(result docker.BuildResult, err error, captured *docker.BuildRequest) func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
	return func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		if captured != nil {
			*captured = req
		}
		return result, err
	}
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func testInspect(networkID string) docker.ContainerInspect {
	inspect := docker.ContainerInspect{ID: "container1"}
	inspect.NetworkSettings.Networks = map[string]struct {
		NetworkID string `json:"NetworkID"`
		IPAddress string `json:"IPAddress"`
	}{
		"env-net": {NetworkID: networkID, IPAddress: "10.0.0.5"},
	}
	return inspect
}

func testLease() agentclient.Lease {
	return agentclient.Lease{
		JobID: 1, AttemptID: 42, Fence: 7,
		ProjectID: "proj1", EnvironmentID: "env1", DeploymentID: "dep1",
		RepositoryID: 1, Ref: "refs/heads/main", CommitSHA: "abc1230000000000000000000000000000000f",
		ContainerPort: 8080, ContainerScheme: "http", HealthPolicyRevision: 1,
		CPUMillis: 500, MemoryBytes: 256 << 20, RestartPolicy: docker.RestartPolicyUnlessStopped,
		HealthGatePath: "/healthz", HealthGateHost: "configured.internal:9443",
		ReleaseID: "rel1",
		SecretBindings: []agentclient.SecretBinding{
			{VariableName: "BUILD_SECRET", DeliveryScope: docker.DeliveryScopeBuild, SecretVersionID: "sv1"},
			{VariableName: "RUNTIME_SECRET", DeliveryScope: docker.DeliveryScopeRuntime, SecretVersionID: "sv2"},
		},
		LiteralBindings: []agentclient.LiteralBinding{
			{VariableName: "RUNTIME_LITERAL", DeliveryScope: docker.DeliveryScopeRuntime, Value: "literal-value"},
		},
	}
}

func newTestExecutor(cp *fakeControlPlane, co *fakeCheckout, dk *fakeDocker, build func(context.Context, docker.BuildRequest) (docker.BuildResult, error)) *Executor {
	return &Executor{
		checkout:     co,
		docker:       dk,
		build:        build,
		controlPlane: cp,
		serverID:     "server1",
		network:      "env-network",
		buildOutput:  io.Discard,
		logger:       testLogger(),
	}
}

func happyCheckoutResult() CheckoutResult {
	return CheckoutResult{
		ContextDir: "/tmp/does-not-matter",
		Binding:    githubapp.CheckoutBinding{OwnerID: "owner1"},
	}
}

// --- Happy path ---

func TestExecuteHappyPathReachesHealthChecking(t *testing.T) {
	lease := testLease()
	cp := &fakeControlPlane{secrets: []agentclient.MaterializedSecret{
		{VariableName: "BUILD_SECRET", DeliveryScope: docker.DeliveryScopeBuild, Value: "build-secret-value"},
		{VariableName: "RUNTIME_SECRET", DeliveryScope: docker.DeliveryScopeRuntime, Value: "runtime-secret-value"},
	}}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{
		ensureResult: testInspect("networkabc123"),
		gateResult:   docker.HealthGateResult{Success: true},
	}
	var buildReq docker.BuildRequest
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, &buildReq)

	e := newTestExecutor(cp, co, dk, build)
	outcome := e.Execute(context.Background(), lease)

	if !outcome.Succeeded {
		t.Fatalf("Execute did not succeed: %+v", outcome)
	}
	wantPhases := []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking}
	if len(cp.progressCalls) != len(wantPhases) {
		t.Fatalf("progress calls = %v, want exactly %v (never progressing past health_checking)", cp.progressCalls, wantPhases)
	}
	for i, phase := range wantPhases {
		if cp.progressCalls[i] != phase {
			t.Fatalf("progress call %d = %q, want %q", i, cp.progressCalls[i], phase)
		}
	}
	if co.closeN != 1 {
		t.Fatalf("checkout Close called %d times, want exactly 1", co.closeN)
	}
	if dk.ensureN != 1 || dk.gateN != 1 {
		t.Fatalf("ensureN=%d gateN=%d, want 1 and 1", dk.ensureN, dk.gateN)
	}

	// Build-scoped secret reaches the build, never the container env.
	if len(buildReq.SecretBindings) != 1 || buildReq.SecretBindings[0].VariableName != "BUILD_SECRET" {
		t.Fatalf("build secret bindings = %+v, want exactly BUILD_SECRET", buildReq.SecretBindings)
	}
	for _, env := range dk.lastRunReq.Env {
		if env.VariableName == "BUILD_SECRET" {
			t.Fatalf("build-scoped secret BUILD_SECRET leaked into container env: %+v", dk.lastRunReq.Env)
		}
	}
	// Runtime-scoped secret and literal both reach the container env.
	foundSecret, foundLiteral := false, false
	for _, env := range dk.lastRunReq.Env {
		if env.VariableName == "RUNTIME_SECRET" {
			foundSecret = true
			if env.Value != "runtime-secret-value" {
				t.Fatalf("RUNTIME_SECRET value = %q, want materialized value", env.Value)
			}
		}
		if env.VariableName == "RUNTIME_LITERAL" && env.Value == "literal-value" {
			foundLiteral = true
		}
	}
	if !foundSecret || !foundLiteral {
		t.Fatalf("container env = %+v, want RUNTIME_SECRET and RUNTIME_LITERAL", dk.lastRunReq.Env)
	}

	if outcome.OperationID == "" {
		t.Fatal("OperationID empty, want a deterministic non-empty value")
	}
}

// TestExecuteHealthGateTargetUsesConfiguredPathAndHostNeverSynthesized
// proves Slice 10 Step 0's fix: the probe's Path and Host header must come
// exactly from the lease's frozen, administrator-configured values, never a
// synthesized ip:port authority or a hard-coded "/" default -- the gap
// independent review flagged before migration 000017 existed.
func TestExecuteHealthGateTargetUsesConfiguredPathAndHostNeverSynthesized(t *testing.T) {
	lease := testLease()
	cp := &fakeControlPlane{secrets: fullSecrets()}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{
		ensureResult: testInspect("networkabc123"),
		gateResult:   docker.HealthGateResult{Success: true},
	}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)

	outcome := e.Execute(context.Background(), lease)
	if !outcome.Succeeded {
		t.Fatalf("Execute did not succeed: %+v", outcome)
	}

	if dk.lastTarget.Host != lease.HealthGateHost {
		t.Fatalf("Host = %q, want the lease's configured HealthGateHost %q, never a synthesized ip:port authority", dk.lastTarget.Host, lease.HealthGateHost)
	}
	if dk.lastTarget.Path != lease.HealthGatePath {
		t.Fatalf("Path = %q, want the lease's configured HealthGatePath %q, never a hard-coded default", dk.lastTarget.Path, lease.HealthGatePath)
	}
	if dk.lastNetID != "networkabc123" {
		t.Fatalf("networkID passed to RunHealthGate = %q, want the resolved network ID", dk.lastNetID)
	}
}

// TestExecuteEmptyHealthGatePathFailsClosedBeforeAnyContainerWork and
// TestExecuteEmptyHealthGateHostFailsClosedBeforeAnyContainerWork prove
// Slice 10 Step 0's fail-closed guard: a lease with an empty configured
// path or Host (which migration 000017's NOT NULL/CHECK constraints should
// make unreachable for a genuinely valid configuration) is rejected as
// FailureClassCorruptState before checkout, build, or any docker call --
// proving this is a protocol-level defect classification, not an
// application failure, and that nothing downstream ever runs.
func TestExecuteEmptyHealthGatePathFailsClosedBeforeAnyContainerWork(t *testing.T) {
	lease := testLease()
	lease.HealthGatePath = ""
	assertFailsClosedBeforeContainerWork(t, lease)
}

func TestExecuteEmptyHealthGateHostFailsClosedBeforeAnyContainerWork(t *testing.T) {
	lease := testLease()
	lease.HealthGateHost = ""
	assertFailsClosedBeforeContainerWork(t, lease)
}

func assertFailsClosedBeforeContainerWork(t *testing.T, lease agentclient.Lease) {
	t.Helper()
	cp := &fakeControlPlane{secrets: fullSecrets()}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{
		ensureResult: testInspect("networkabc123"),
		gateResult:   docker.HealthGateResult{Success: true},
	}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)

	outcome := e.Execute(context.Background(), lease)
	if outcome.Succeeded {
		t.Fatal("Execute succeeded with an empty configured health gate path/host, want failure")
	}
	if outcome.FailureClass != queue.FailureClassCorruptState {
		t.Fatalf("FailureClass = %q, want %q", outcome.FailureClass, queue.FailureClassCorruptState)
	}
	if co.checkoutN != 0 {
		t.Fatalf("Checkout called %d times, want 0 -- must fail before any container work", co.checkoutN)
	}
	if dk.ensureN != 0 || dk.gateN != 0 {
		t.Fatalf("ensureN=%d gateN=%d, want 0 and 0 -- must fail before any container work", dk.ensureN, dk.gateN)
	}
	if len(cp.progressCalls) != 0 {
		t.Fatalf("progress calls = %v, want none -- must fail before reporting any phase", cp.progressCalls)
	}
}

// TestExecuteRuntimeSecretNeverReachesBuild isolates the build-scope
// invariant so the break-the-guard pass can target it directly: even if the
// materializer were asked for a runtime-scoped binding disguised as a build
// binding, MaterializeBuildSecrets refuses it.
func TestExecuteRuntimeSecretNeverReachesBuild(t *testing.T) {
	cp := &fakeControlPlane{secrets: []agentclient.MaterializedSecret{
		{VariableName: "RUNTIME_SECRET", DeliveryScope: docker.DeliveryScopeRuntime, Value: "v"},
	}}
	m := newSecretMaterializer(cp, 1, 1)
	_, err := m.MaterializeBuildSecrets(context.Background(), []docker.BuildSecretBinding{{VariableName: "RUNTIME_SECRET", SecretVersionID: "sv"}})
	if err == nil {
		t.Fatal("want rejection: a runtime-scoped materialized value must never satisfy a build request")
	}
}

// TestExecuteBuildSecretNeverReachesContainerEnv is the mirror image of
// TestExecuteRuntimeSecretNeverReachesBuild. It targets runtimeSecretEnv's
// OWN inner re-check (secrets.go, "s.DeliveryScope != docker.
// DeliveryScopeRuntime") specifically, not the outer per-binding filter one
// line above it: the binding itself claims DeliveryScope=="runtime" (so it
// passes the outer filter and the inner check is actually reached), but the
// control plane's materialized value for that same variable name comes
// back scoped "build" -- exactly the shape a mismatched/corrupted
// materialization response would have. An earlier version of this test
// gave the outer filter a build-scoped binding directly, which excludes it
// before the inner check ever runs; deleting the inner check entirely left
// that version green (see the break-the-guard pass in the Step 6b/6b-fix
// report) -- the same "test proves less than its name claims" trap Steps
// 4/5/6a's own reviews found twice before.
func TestExecuteBuildSecretNeverReachesContainerEnv(t *testing.T) {
	cp := &fakeControlPlane{secrets: []agentclient.MaterializedSecret{
		{VariableName: "MISSCOPED", DeliveryScope: docker.DeliveryScopeBuild, Value: "v"},
	}}
	m := newSecretMaterializer(cp, 1, 1)
	_, err := m.runtimeSecretEnv(context.Background(), []agentclient.SecretBinding{
		{VariableName: "MISSCOPED", DeliveryScope: docker.DeliveryScopeRuntime, SecretVersionID: "sv"},
	})
	if err == nil {
		t.Fatal("want rejection: a build-scoped materialized value must never satisfy a runtime-env request, even under a binding that itself claims runtime scope")
	}
	if !errors.Is(err, errMaterializeMismatch) {
		t.Fatalf("err = %v, want errMaterializeMismatch", err)
	}
}

// fullSecrets satisfies every secret binding testLease() carries, so a
// subtest that must get past the "starting" phase's containerEnv
// resolution to exercise a later failure point does not fail early (and
// silently pass for the wrong reason -- exactly the "delivered test proved
// less than its name claimed" trap Steps 4/5/6a's own review passes found).
func fullSecrets() []agentclient.MaterializedSecret {
	return []agentclient.MaterializedSecret{
		{VariableName: "BUILD_SECRET", DeliveryScope: docker.DeliveryScopeBuild, Value: "build-secret-value"},
		{VariableName: "RUNTIME_SECRET", DeliveryScope: docker.DeliveryScopeRuntime, Value: "runtime-secret-value"},
	}
}

// --- Failure classification ---

func TestExecuteFailurePointsMapToExpectedFailureClass(t *testing.T) {
	t.Run("checkout unsafe source", func(t *testing.T) {
		lease := testLease()
		cp := &fakeControlPlane{}
		co := &fakeCheckout{err: errUnsafeSourceForTest}
		e := newTestExecutor(cp, co, &fakeDocker{}, fakeBuildFunc(docker.BuildResult{}, nil, nil))
		outcome := e.Execute(context.Background(), lease)
		if outcome.Succeeded || outcome.FailureClass != queue.FailureClassPermanentConfiguration {
			t.Fatalf("outcome = %+v, want permanent_configuration", outcome)
		}
	})

	t.Run("materialize authorization failure", func(t *testing.T) {
		lease := testLease()
		cp := &fakeControlPlane{secretsErr: agentclient.ErrUnauthorized}
		co := &fakeCheckout{result: happyCheckoutResult()}
		dk := &fakeDocker{}
		build := fakeBuildFunc(docker.BuildResult{}, nil, nil)
		e := newTestExecutor(cp, co, dk, build)
		outcome := e.Execute(context.Background(), lease)
		if outcome.Succeeded || outcome.FailureClass != queue.FailureClassAuthorization {
			t.Fatalf("outcome = %+v, want authorization", outcome)
		}
		if dk.ensureN != 0 {
			t.Fatalf("EnsureReleaseRuntimeContainer called %d times, want 0 -- materialization must fail before run", dk.ensureN)
		}
	})

	t.Run("build failure", func(t *testing.T) {
		lease := testLease()
		cp := &fakeControlPlane{}
		co := &fakeCheckout{result: happyCheckoutResult()}
		dk := &fakeDocker{}
		var buildCalled bool
		build := func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
			buildCalled = true
			return docker.BuildResult{}, docker.ErrBuildFailed
		}
		e := newTestExecutor(cp, co, dk, build)
		outcome := e.Execute(context.Background(), lease)
		if outcome.Succeeded || outcome.FailureClass != queue.FailureClassPermanentConfiguration {
			t.Fatalf("outcome = %+v, want permanent_configuration", outcome)
		}
		if !buildCalled {
			t.Fatal("build was never called")
		}
		if dk.ensureN != 0 {
			t.Fatalf("EnsureReleaseRuntimeContainer called %d times, want 0 -- build must fail before run", dk.ensureN)
		}
	})

	t.Run("ensure container inconsistent state", func(t *testing.T) {
		lease := testLease()
		cp := &fakeControlPlane{secrets: fullSecrets()}
		co := &fakeCheckout{result: happyCheckoutResult()}
		dk := &fakeDocker{ensureErr: docker.ErrInconsistentLocalState}
		build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
		e := newTestExecutor(cp, co, dk, build)
		outcome := e.Execute(context.Background(), lease)
		if outcome.Succeeded || outcome.FailureClass != queue.FailureClassCorruptState {
			t.Fatalf("outcome = %+v, want corrupt_state", outcome)
		}
		if dk.ensureN != 1 {
			t.Fatalf("EnsureReleaseRuntimeContainer called %d times, want exactly 1 -- outcome must come from ensureErr, not an earlier failure", dk.ensureN)
		}
	})

	t.Run("health gate deadline exceeded", func(t *testing.T) {
		lease := testLease()
		cp := &fakeControlPlane{secrets: fullSecrets()}
		co := &fakeCheckout{result: happyCheckoutResult()}
		dk := &fakeDocker{
			ensureResult: testInspect("networkabc123"),
			gateResult:   docker.HealthGateResult{Success: false, Reason: docker.HealthGateFailureDeadlineExceeded},
		}
		build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
		e := newTestExecutor(cp, co, dk, build)
		outcome := e.Execute(context.Background(), lease)
		if outcome.Succeeded || outcome.FailureClass != queue.FailureClassPermanentConfiguration {
			t.Fatalf("outcome = %+v, want permanent_configuration", outcome)
		}
		if dk.gateN != 1 {
			t.Fatalf("RunHealthGate called %d times, want exactly 1 -- outcome must come from the gate result, not an earlier failure", dk.gateN)
		}
	})

	t.Run("health gate OOM killed is transient", func(t *testing.T) {
		lease := testLease()
		cp := &fakeControlPlane{secrets: fullSecrets()}
		co := &fakeCheckout{result: happyCheckoutResult()}
		dk := &fakeDocker{
			ensureResult: testInspect("networkabc123"),
			gateResult:   docker.HealthGateResult{Success: false, Reason: docker.HealthGateFailureContainerNotRunning, OOMKilled: true},
		}
		build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
		e := newTestExecutor(cp, co, dk, build)
		outcome := e.Execute(context.Background(), lease)
		if outcome.Succeeded || outcome.FailureClass != queue.FailureClassTransient {
			t.Fatalf("outcome = %+v, want transient", outcome)
		}
		if dk.gateN != 1 {
			t.Fatalf("RunHealthGate called %d times, want exactly 1 -- outcome must come from the gate result, not an earlier failure", dk.gateN)
		}
	})
}

// --- Progress idempotency: never rely on the control plane's own
// transition rules to absorb a repeated phase. ---

func TestPhaseTrackerSkipsAlreadyReportedStatus(t *testing.T) {
	cp := &fakeControlPlane{}
	reporter := newProgressReporter(cp)
	tracker := &phaseTracker{}
	ctx := context.Background()

	if err := tracker.advance(ctx, reporter, 1, 1, deployment.StatusFetching); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := tracker.advance(ctx, reporter, 1, 1, deployment.StatusFetching); err != nil {
		t.Fatalf("advance (repeat): %v", err)
	}
	if len(cp.progressCalls) != 1 {
		t.Fatalf("ReportProgress called %d times for a repeated phase, want exactly 1 (the tracker must skip locally, not rely on the server absorbing a duplicate)", len(cp.progressCalls))
	}

	if err := tracker.advance(ctx, reporter, 1, 1, deployment.StatusBuilding); err != nil {
		t.Fatalf("advance (new phase): %v", err)
	}
	if len(cp.progressCalls) != 2 {
		t.Fatalf("ReportProgress called %d times after a genuinely new phase, want 2", len(cp.progressCalls))
	}
}

// --- Context cancellation: Execute must return promptly, never stall the
// lease loop that awaits it. ---

func TestExecuteReturnsPromptlyOnContextCancellation(t *testing.T) {
	lease := testLease()
	cp := &fakeControlPlane{}
	co := &fakeCheckout{block: true}
	dk := &fakeDocker{}
	e := newTestExecutor(cp, co, dk, fakeBuildFunc(docker.BuildResult{}, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan agentclient.Outcome, 1)
	go func() { done <- e.Execute(ctx, lease) }()

	select {
	case outcome := <-done:
		if outcome.Succeeded {
			t.Fatal("cancelled Execute reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return promptly after context cancellation")
	}
}

// TestExecuteReturnsPromptlyOnContextCancellationDuringHealthGate covers the
// other blocking point explicitly (checkout being cancelled is not the same
// code path as the health gate being cancelled).
func TestExecuteReturnsPromptlyOnContextCancellationDuringHealthGate(t *testing.T) {
	lease := testLease()
	cp := &fakeControlPlane{}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), blockGate: true}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + zeros(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan agentclient.Outcome, 1)
	go func() { done <- e.Execute(ctx, lease) }()

	select {
	case outcome := <-done:
		if outcome.Succeeded {
			t.Fatal("cancelled Execute reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return promptly after context cancellation during the health gate")
	}
}

// --- NewExecutor's startup capability probe fails closed. ---

func TestNewExecutorFailsClosedWhenEngineProbeFails(t *testing.T) {
	cfg := Config{
		ControlPlane:   &fakeControlPlane{},
		Docker:         &fakeDocker{probeErr: errors.New("engine unreachable")},
		CheckoutRunner: fakeCheckoutRunner{},
		ServerID:       "server1",
		Network:        "env-network",
	}
	if _, err := NewExecutor(context.Background(), cfg); err == nil {
		t.Fatal("want NewExecutor to fail closed when the engine probe fails")
	}
}

func TestNewExecutorFailsClosedWhenBuildxProbeFails(t *testing.T) {
	orig := probeBuildxFunc
	probeBuildxFunc = func(context.Context) error { return errors.New("buildx missing") }
	defer func() { probeBuildxFunc = orig }()

	cfg := Config{
		ControlPlane:   &fakeControlPlane{},
		Docker:         &fakeDocker{},
		CheckoutRunner: fakeCheckoutRunner{},
		ServerID:       "server1",
		Network:        "env-network",
	}
	if _, err := NewExecutor(context.Background(), cfg); err == nil {
		t.Fatal("want NewExecutor to fail closed when the buildx probe fails")
	}
}

func TestNewExecutorSucceedsWhenBothProbesPass(t *testing.T) {
	orig := probeBuildxFunc
	probeBuildxFunc = func(context.Context) error { return nil }
	defer func() { probeBuildxFunc = orig }()

	cfg := Config{
		ControlPlane:   &fakeControlPlane{},
		Docker:         &fakeDocker{},
		CheckoutRunner: fakeCheckoutRunner{},
		ServerID:       "server1",
		Network:        "env-network",
	}
	e, err := NewExecutor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	if e == nil {
		t.Fatal("NewExecutor returned nil Executor with no error")
	}
}

type fakeCheckoutRunner struct{}

func (fakeCheckoutRunner) Checkout(context.Context, gitcheckout.Lease) (*gitcheckout.Result, error) {
	return nil, errors.New("unused")
}

var errUnsafeSourceForTest = gitcheckout.ErrUnsafeSource

func zeros(n int) string { return repeat('0', n) }
func ones(n int) string  { return repeat('1', n) }
func repeat(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}
