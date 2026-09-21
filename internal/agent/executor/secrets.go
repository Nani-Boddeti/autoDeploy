package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/runtime/docker"
)

// errMaterializeMismatch means the control plane's materialized secret set
// (the frozen bindings it decided this attempt gets, per
// Client.MaterializeSecrets's own doc) did not contain a value for a
// binding Execute expected, or returned one under the wrong delivery scope.
// Both are treated as a local/authorization inconsistency, never as "assume
// the value is safe to use anyway."
var errMaterializeMismatch = errors.New("agent executor: materialized secret set does not match frozen bindings")

// controlPlaneSecrets is the narrow control-plane capability this file
// needs. *agentclient.Client satisfies it structurally; tests use a fake.
type controlPlaneSecrets interface {
	MaterializeSecrets(ctx context.Context, attemptID, fence int64) ([]agentclient.MaterializedSecret, error)
}

// secretMaterializer fetches the frozen secret set for one attempt/fence at
// most once -- materialization is a fenced authorization decision the
// control plane makes per attempt, not a cache to repopulate on demand --
// and serves both the build path (docker.BuildSecretMaterializer, called by
// internal/runtime/docker.Build) and the runtime container-env path from
// that single fetch. Both accessors independently re-check DeliveryScope
// against what they asked for: a runtime-scoped value can never satisfy a
// build request and a build-scoped value can never satisfy a runtime
// request, even if the caller's own upstream filtering (docker.
// BuildScopedBindings for build, runtimeScoped for runtime) were ever
// bypassed or had a bug.
type secretMaterializer struct {
	client    controlPlaneSecrets
	attemptID int64
	fence     int64

	once sync.Once
	all  []agentclient.MaterializedSecret
	err  error
}

func newSecretMaterializer(client controlPlaneSecrets, attemptID, fence int64) *secretMaterializer {
	return &secretMaterializer{client: client, attemptID: attemptID, fence: fence}
}

func (m *secretMaterializer) fetch(ctx context.Context) ([]agentclient.MaterializedSecret, error) {
	m.once.Do(func() {
		m.all, m.err = m.client.MaterializeSecrets(ctx, m.attemptID, m.fence)
	})
	return m.all, m.err
}

// MaterializeBuildSecrets implements docker.BuildSecretMaterializer. bindings
// must already be filtered to build scope by docker.BuildScopedBindings
// before Execute ever calls Build -- see toScopedBindings's caller in
// executor.go -- and this method independently refuses to return a value
// whose own DeliveryScope is not "build".
func (m *secretMaterializer) MaterializeBuildSecrets(ctx context.Context, bindings []docker.BuildSecretBinding) ([]docker.BuildSecretValue, error) {
	all, err := m.fetch(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]agentclient.MaterializedSecret, len(all))
	for _, s := range all {
		byName[s.VariableName] = s
	}
	out := make([]docker.BuildSecretValue, 0, len(bindings))
	for _, b := range bindings {
		s, ok := byName[b.VariableName]
		if !ok || s.DeliveryScope != docker.DeliveryScopeBuild {
			return nil, fmt.Errorf("%w: no build-scoped materialized value for %q", errMaterializeMismatch, b.VariableName)
		}
		out = append(out, docker.BuildSecretValue{VariableName: b.VariableName, Value: []byte(s.Value)})
	}
	return out, nil
}

// runtimeSecretEnv resolves every runtime-scoped secret binding to its
// plaintext value -- the container-env mirror of MaterializeBuildSecrets's
// build-scope filter. It fetches nothing if there is nothing runtime-scoped
// to resolve.
func (m *secretMaterializer) runtimeSecretEnv(ctx context.Context, bindings []agentclient.SecretBinding) ([]docker.ContainerEnv, error) {
	var needed []agentclient.SecretBinding
	for _, b := range bindings {
		if b.DeliveryScope == docker.DeliveryScopeRuntime {
			needed = append(needed, b)
		}
	}
	if len(needed) == 0 {
		return nil, nil
	}
	all, err := m.fetch(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]agentclient.MaterializedSecret, len(all))
	for _, s := range all {
		byName[s.VariableName] = s
	}
	out := make([]docker.ContainerEnv, 0, len(needed))
	for _, b := range needed {
		s, ok := byName[b.VariableName]
		if !ok || s.DeliveryScope != docker.DeliveryScopeRuntime {
			return nil, fmt.Errorf("%w: no runtime-scoped materialized value for %q", errMaterializeMismatch, b.VariableName)
		}
		out = append(out, docker.ContainerEnv{VariableName: b.VariableName, Value: s.Value})
	}
	return out, nil
}

// toScopedBindings adapts the Lease's own typed secret bindings to
// docker.ScopedSecretBinding, the input docker.BuildScopedBindings filters.
// This is a pure field reshuffle, not a scope decision -- the actual
// build/runtime split happens exactly once, inside docker.
// BuildScopedBindings itself, per that function's own doc ("the single
// choke point ADR 0003 requires").
func toScopedBindings(bindings []agentclient.SecretBinding) []docker.ScopedSecretBinding {
	out := make([]docker.ScopedSecretBinding, len(bindings))
	for i, b := range bindings {
		out[i] = docker.ScopedSecretBinding{VariableName: b.VariableName, DeliveryScope: b.DeliveryScope, SecretVersionID: b.SecretVersionID}
	}
	return out
}

// literalRuntimeEnv extracts the runtime-scoped literal_nonsecret bindings
// from the lease as container env entries directly -- these carry their
// value on the lease itself (ADR 0003 classification) and need no
// materialization call. Build-scoped literal bindings are not delivered
// here or anywhere else in this slice: internal/runtime/docker's Step 5
// build path has no build-arg/env passthrough (deliberately -- see
// build.go, "no --build-arg call site in the package"), so a build-scoped
// literal binding has no delivery mechanism yet. That gap is called out in
// the Step 6b report rather than silently worked around.
func literalRuntimeEnv(bindings []agentclient.LiteralBinding) []docker.ContainerEnv {
	var out []docker.ContainerEnv
	for _, b := range bindings {
		if b.DeliveryScope == docker.DeliveryScopeRuntime {
			out = append(out, docker.ContainerEnv{VariableName: b.VariableName, Value: b.Value})
		}
	}
	return out
}
