package executor

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/logship"
	"autodeploy/internal/runtime/docker"
)

type captureSender struct {
	mu     sync.Mutex
	chunks []agentclient.LogChunk
}

func (c *captureSender) AppendLogs(_ context.Context, req agentclient.AppendLogsRequest) (agentclient.AppendLogsResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunks = append(c.chunks, req.Chunks...)
	return agentclient.AppendLogsResult{Accepted: len(req.Chunks)}, nil
}

func (c *captureSender) text(stream string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var sb strings.Builder
	for _, ch := range c.chunks {
		if ch.Stream == stream {
			sb.WriteString(ch.Body + "\n")
		}
	}
	return sb.String()
}

// TestExecuteShipsRedactedLogsAndKeepsLocalOutput proves the additive wiring:
// the local writer still sees the (already build-redacted) bytes, the shipper
// additionally masks the RUNTIME secret the build redactor does not know, and
// phase/checkout/health lines are shipped.
func TestExecuteShipsRedactedLogsAndKeepsLocalOutput(t *testing.T) {
	lease := testLease()
	cp := &fakeControlPlane{secrets: []agentclient.MaterializedSecret{
		{VariableName: "BUILD_SECRET", DeliveryScope: docker.DeliveryScopeBuild, Value: "build-secret-value"},
		{VariableName: "RUNTIME_SECRET", DeliveryScope: docker.DeliveryScopeRuntime, Value: "runtime-secret-value"},
	}}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	build := func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		// Like docker.Build: materialize the build-scoped set before output.
		if _, err := req.Materializer.MaterializeBuildSecrets(ctx, req.SecretBindings); err != nil {
			return docker.BuildResult{}, err
		}
		io.WriteString(req.Stdout, "step 1 runtime-secret-value done\n")
		io.WriteString(req.Stderr, "warn\n")
		return docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}, nil
	}
	sender := &captureSender{}
	factory, err := logship.NewFactory(logship.Config{Sender: sender, Logger: log.New(io.Discard, "", 0), BatchInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var local bytes.Buffer
	var lmu sync.Mutex
	e := newTestExecutor(cp, co, dk, build)
	e.logs = factory
	e.buildOutput = lockedBuf{&lmu, &local}

	if out := e.Execute(context.Background(), lease); !out.HandedOff {
		t.Fatalf("not handed off: %+v", out)
	}
	lmu.Lock()
	gotLocal := local.String()
	lmu.Unlock()
	if !strings.Contains(gotLocal, "step 1 runtime-secret-value done") || !strings.Contains(gotLocal, "warn") {
		t.Fatalf("local output changed: %q", gotLocal)
	}
	buildText := sender.text("build")
	if strings.Contains(buildText, "runtime-secret-value") || !strings.Contains(buildText, "step 1 [REDACTED] done") || !strings.Contains(buildText, "warn") {
		t.Fatalf("shipped build text = %q", buildText)
	}
	phase := sender.text("phase")
	for _, want := range []string{"phase: fetching", "checkout: complete", "phase: building", "build: complete", "phase: starting", "phase: health_checking"} {
		if !strings.Contains(phase, want) {
			t.Fatalf("phase stream missing %q: %q", want, phase)
		}
	}
	if !strings.Contains(sender.text("health"), "health gate: passed") {
		t.Fatalf("health stream = %q", sender.text("health"))
	}
	if !strings.Contains(sender.text("activation"), "activation: authorized") {
		t.Fatalf("activation stream = %q", sender.text("activation"))
	}
}

type lockedBuf struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func okBuildResult() docker.BuildResult {
	return docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}
}

func shippingExecutor(t *testing.T, cp *fakeControlPlane, build func(context.Context, docker.BuildRequest) (docker.BuildResult, error), sender logship.Sender) *Executor {
	t.Helper()
	factory, err := logship.NewFactory(logship.Config{Sender: sender, Logger: log.New(io.Discard, "", 0), BatchInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	e := newTestExecutor(cp, &fakeCheckout{result: happyCheckoutResult()}, dk, build)
	e.logs = factory
	return e
}

func TestMaterializeCallCountsUnchangedByLogShipping(t *testing.T) {
	materializingBuild := func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		if len(req.SecretBindings) > 0 {
			if _, err := req.Materializer.MaterializeBuildSecrets(ctx, req.SecretBindings); err != nil {
				return docker.BuildResult{}, err
			}
		}
		return okBuildResult(), nil
	}
	t.Run("validation failure makes no call", func(t *testing.T) {
		cp := &fakeControlPlane{secrets: fullSecrets()}
		e := shippingExecutor(t, cp, materializingBuild, &captureSender{})
		lease := testLease()
		lease.HealthGatePath = ""
		e.Execute(context.Background(), lease)
		if cp.secretsN != 0 {
			t.Fatalf("materialize calls = %d, want 0", cp.secretsN)
		}
	})
	t.Run("build-scoped bindings make one call", func(t *testing.T) {
		cp := &fakeControlPlane{secrets: fullSecrets()}
		e := shippingExecutor(t, cp, materializingBuild, &captureSender{})
		e.Execute(context.Background(), testLease())
		if cp.secretsN != 1 {
			t.Fatalf("materialize calls = %d, want 1", cp.secretsN)
		}
	})
	t.Run("runtime-only makes one call, after the build", func(t *testing.T) {
		cp := &fakeControlPlane{secrets: fullSecrets()}
		callsAtBuild := -1
		build := func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
			callsAtBuild = cp.secretsN
			return okBuildResult(), nil
		}
		sender := &captureSender{}
		e := shippingExecutor(t, cp, build, sender)
		lease := testLease()
		lease.SecretBindings = lease.SecretBindings[1:] // runtime only
		e.Execute(context.Background(), lease)
		if callsAtBuild != 0 || cp.secretsN != 1 {
			t.Fatalf("calls at build=%d total=%d, want 0 and 1", callsAtBuild, cp.secretsN)
		}
	})
}

func TestBuildScopedSecretPrintedByBuildIsRedactedInShippedLogs(t *testing.T) {
	cp := &fakeControlPlane{secrets: fullSecrets()}
	build := func(ctx context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		if _, err := req.Materializer.MaterializeBuildSecrets(ctx, req.SecretBindings); err != nil {
			return docker.BuildResult{}, err
		}
		io.WriteString(req.Stdout, "leak build-secret-value and b\x00u\x00ild-secret-value\n")
		return okBuildResult(), nil
	}
	sender := &captureSender{}
	e := shippingExecutor(t, cp, build, sender)
	e.Execute(context.Background(), testLease())
	got := sender.text("build")
	if strings.Contains(got, "build-secret-value") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("shipped build text = %q", got)
	}
}
