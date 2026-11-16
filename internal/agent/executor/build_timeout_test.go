package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

func TestBuildTimeoutLogsReasonAndPassesLimit(t *testing.T) {
	cp := &fakeControlPlane{secrets: fullSecrets()}
	var gotTimeout time.Duration
	build := func(_ context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		gotTimeout = req.Timeout
		return docker.BuildResult{}, fmt.Errorf("%w: killed", docker.ErrBuildTimeout)
	}
	sender := &captureSender{}
	e := shippingExecutor(t, cp, build, sender)
	e.buildTimeout = 7 * time.Minute
	out := e.Execute(context.Background(), testLease())
	if out.Succeeded || out.FailureClass != queue.FailureClassPermanentConfiguration {
		t.Fatalf("outcome = %+v, want a permanent_configuration failure", out)
	}
	if gotTimeout != 7*time.Minute {
		t.Fatalf("build got Timeout %s, want 7m0s", gotTimeout)
	}
	if phase := sender.text("phase"); !strings.Contains(phase, "build exceeded its time limit of 7m0s") {
		t.Fatalf("phase stream lacks the timeout reason: %q", phase)
	}
}

func TestBuildInterruptedIsTransient(t *testing.T) {
	cp := &fakeControlPlane{secrets: fullSecrets()}
	build := func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
		return docker.BuildResult{}, fmt.Errorf("%w: %w", docker.ErrBuildInterrupted, context.Canceled)
	}
	e := shippingExecutor(t, cp, build, &captureSender{})
	out := e.Execute(context.Background(), testLease())
	if out.Succeeded || out.FailureClass != queue.FailureClassTransient {
		t.Fatalf("outcome = %+v, want a transient failure", out)
	}
}
