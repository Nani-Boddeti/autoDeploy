package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

type pruneCall struct {
	cfg        docker.BuilderConfig
	keep       int64
	ctxErr     error
	hadTimeout bool
}

// pruneRecorder returns a pruneCache fake that records each call and returns err.
func pruneRecorder(calls *[]pruneCall, err error) func(context.Context, docker.BuilderConfig, int64) error {
	return func(ctx context.Context, cfg docker.BuilderConfig, keep int64) error {
		*calls = append(*calls, pruneCall{cfg: cfg, keep: keep, ctxErr: ctx.Err()})
		return err
	}
}

func cleanupExecutor(t *testing.T, build func(context.Context, docker.BuildRequest) (docker.BuildResult, error), sender *captureSender, prune func(context.Context, docker.BuilderConfig, int64) error) *Executor {
	t.Helper()
	e := shippingExecutor(t, &fakeControlPlane{secrets: fullSecrets()}, build, sender)
	e.builder = docker.BuilderConfig{ConfigDir: "/var/lib/autodeploy/agent/buildx", MemoryBytes: 2 << 30, CPUs: 2}
	e.cacheKeep = 7 << 30
	e.pruneCache = prune
	return e
}

func TestBuildCacheIsPrunedAfterSuccessFailureAndTimeout(t *testing.T) {
	cases := map[string]struct {
		build       func(context.Context, docker.BuildRequest) (docker.BuildResult, error)
		wantSuccess bool
		wantClass   queue.FailureClass
	}{
		"success": {build: func(context.Context, docker.BuildRequest) (docker.BuildResult, error) { return okBuildResult(), nil }, wantSuccess: true},
		"failure": {build: func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
			return docker.BuildResult{}, fmt.Errorf("%w: exit 1", docker.ErrBuildFailed)
		}, wantClass: queue.FailureClassPermanentConfiguration},
		"timeout": {build: func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
			return docker.BuildResult{}, fmt.Errorf("%w: killed", docker.ErrBuildTimeout)
		}, wantClass: queue.FailureClassPermanentConfiguration},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls []pruneCall
			e := cleanupExecutor(t, tc.build, &captureSender{}, pruneRecorder(&calls, nil))
			out := e.Execute(context.Background(), testLease())
			if len(calls) != 1 {
				t.Fatalf("prune called %d times, want exactly 1", len(calls))
			}
			if calls[0].keep != 7<<30 || calls[0].cfg.ConfigDir != e.builder.ConfigDir || calls[0].cfg.MemoryBytes != e.builder.MemoryBytes || calls[0].cfg.CPUs != e.builder.CPUs {
				t.Fatalf("prune got %+v, want the executor's builder and keep", calls[0])
			}
			if tc.wantSuccess {
				return
			}
			if out.Succeeded || out.FailureClass != tc.wantClass {
				t.Fatalf("outcome = %+v, want failure %s", out, tc.wantClass)
			}
		})
	}
}

func TestBuildCachePruneRunsAfterCallerCancelWithLiveContext(t *testing.T) {
	var calls []pruneCall
	ctx, cancel := context.WithCancel(context.Background())
	build := func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
		cancel()
		return docker.BuildResult{}, fmt.Errorf("%w: %w", docker.ErrBuildInterrupted, context.Canceled)
	}
	e := cleanupExecutor(t, build, &captureSender{}, pruneRecorder(&calls, nil))
	out := e.Execute(ctx, testLease())
	if len(calls) != 1 {
		t.Fatalf("prune called %d times after a cancelled build, want 1", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Fatalf("prune context was already dead (%v): a cancelled job must still clean up", calls[0].ctxErr)
	}
	if out.Succeeded || out.FailureClass != queue.FailureClassTransient {
		t.Fatalf("outcome = %+v, want the unchanged transient failure", out)
	}
}

func TestBuildCachePruneFailureIsLoggedAndNeverChangesOutcome(t *testing.T) {
	const leak = "dial unix /var/run/secret-socket: boom"
	for name, tc := range map[string]struct {
		build       func(context.Context, docker.BuildRequest) (docker.BuildResult, error)
		wantSuccess bool
	}{
		"success": {build: func(context.Context, docker.BuildRequest) (docker.BuildResult, error) { return okBuildResult(), nil }, wantSuccess: true},
		"failure": {build: func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
			return docker.BuildResult{}, fmt.Errorf("%w: exit 1", docker.ErrBuildFailed)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var calls []pruneCall
			sender := &captureSender{}
			e := cleanupExecutor(t, tc.build, sender, pruneRecorder(&calls, errors.New(leak)))
			out := e.Execute(context.Background(), testLease())
			// A successful run ends handed off to activation, not Succeeded.
			if tc.wantSuccess != out.HandedOff || out.Succeeded || (tc.wantSuccess && out.FailureClass != "") {
				t.Fatalf("outcome = %+v, want the unchanged result (handed off=%v) despite the prune error", out, tc.wantSuccess)
			}
			phase := sender.text("phase")
			if !strings.Contains(phase, "build: cache cleanup failed") {
				t.Fatalf("phase log lacks the cleanup failure line: %q", phase)
			}
			if strings.Contains(phase, "secret-socket") || strings.Contains(phase, "boom") {
				t.Fatalf("phase log leaked the raw error text: %q", phase)
			}
		})
	}
}

func TestNewExecutorWiresBuildCacheKeepWithDefault(t *testing.T) {
	orig := probeBuildxFunc
	probeBuildxFunc = func(context.Context, docker.BuilderConfig) error { return nil }
	defer func() { probeBuildxFunc = orig }()
	for name, tc := range map[string]struct{ in, want int64 }{
		"explicit": {in: 9 << 30, want: 9 << 30},
		"zero":     {in: 0, want: docker.DefaultBuildCacheKeepBytes},
	} {
		e, err := NewExecutor(context.Background(), Config{
			ControlPlane: &fakeControlPlane{}, Docker: &fakeDocker{}, CheckoutRunner: fakeCheckoutRunner{},
			ServerID: "server1", Network: "env-network", Activation: testActivationConfig(),
			BuildCacheKeep: tc.in,
		})
		if err != nil {
			t.Fatalf("%s: NewExecutor: %v", name, err)
		}
		if e.cacheKeep != tc.want || e.pruneCache == nil {
			t.Fatalf("%s: cacheKeep = %d (pruneCache nil=%v), want %d and a real pruner", name, e.cacheKeep, e.pruneCache == nil, tc.want)
		}
	}
}

func TestBuildCachePruneSkippedWhenBuildNeverStarted(t *testing.T) {
	var calls []pruneCall
	sender := &captureSender{}
	build := func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
		return docker.BuildResult{}, docker.MarkBuildNotStarted(fmt.Errorf("%w: bad request", docker.ErrInvalidBuildRequest))
	}
	e := cleanupExecutor(t, build, sender, pruneRecorder(&calls, errors.New("must not be called")))
	out := e.Execute(context.Background(), testLease())
	if len(calls) != 0 {
		t.Fatalf("prune called %d times for a build that never started, want 0", len(calls))
	}
	if out.Succeeded || out.FailureClass != queue.FailureClassPermanentConfiguration {
		t.Fatalf("outcome = %+v, want the unchanged permanent failure", out)
	}
}
