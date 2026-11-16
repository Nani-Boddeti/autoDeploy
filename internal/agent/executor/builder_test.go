package executor

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/runtime/docker"
)

func TestNewExecutorEnsuresBuilderAndPassesConfigDirToBuilds(t *testing.T) {
	var probed docker.BuilderConfig
	orig := probeBuildxFunc
	probeBuildxFunc = func(_ context.Context, b docker.BuilderConfig) error { probed = b; return nil }
	defer func() { probeBuildxFunc = orig }()

	want := docker.BuilderConfig{ConfigDir: "/var/lib/autodeploy/agent/buildx", MemoryBytes: 2 << 30, CPUs: 2}
	e, err := NewExecutor(context.Background(), Config{
		ControlPlane:   &fakeControlPlane{},
		Docker:         &fakeDocker{},
		CheckoutRunner: fakeCheckoutRunner{},
		ServerID:       "server1",
		Network:        "env-network",
		Activation:     testActivationConfig(),
		Builder:        want,
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	if probed.ConfigDir != want.ConfigDir || probed.MemoryBytes != want.MemoryBytes || probed.CPUs != want.CPUs {
		t.Fatalf("probe got %+v, want %+v", probed, want)
	}
	if e.buildxConfig != want.ConfigDir {
		t.Fatalf("executor buildxConfig = %q, want %q", e.buildxConfig, want.ConfigDir)
	}
}

func TestNewExecutorFailsClosedWhenBuilderUnavailable(t *testing.T) {
	orig := probeBuildxFunc
	probeBuildxFunc = func(context.Context, docker.BuilderConfig) error { return docker.ErrBuilderUnavailable }
	defer func() { probeBuildxFunc = orig }()
	_, err := NewExecutor(context.Background(), Config{
		ControlPlane:   &fakeControlPlane{},
		Docker:         &fakeDocker{},
		CheckoutRunner: fakeCheckoutRunner{},
		ServerID:       "server1",
		Network:        "env-network",
		Activation:     testActivationConfig(),
	})
	if !errors.Is(err, docker.ErrBuilderUnavailable) {
		t.Fatalf("err = %v, want ErrBuilderUnavailable", err)
	}
}

func TestBuildRequestCarriesBuildxConfigDir(t *testing.T) {
	cp := &fakeControlPlane{secrets: fullSecrets()}
	var got string
	build := func(_ context.Context, req docker.BuildRequest) (docker.BuildResult, error) {
		got = req.BuildxConfigDir
		return docker.BuildResult{}, errors.New("stop here")
	}
	e := shippingExecutor(t, cp, build, &captureSender{})
	e.buildxConfig = "/var/lib/autodeploy/agent/buildx"
	e.Execute(context.Background(), testLease())
	if got != "/var/lib/autodeploy/agent/buildx" {
		t.Fatalf("build got BuildxConfigDir %q", got)
	}
}
