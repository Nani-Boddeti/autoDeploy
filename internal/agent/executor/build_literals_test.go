package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

func TestExecuteDeliversBuildScopedLiteralsToBuild(t *testing.T) {
	lease := testLease()
	lease.LiteralBindings = []agentclient.LiteralBinding{
		{VariableName: "RUNTIME_LITERAL", DeliveryScope: docker.DeliveryScopeRuntime, Value: "rt"},
		{VariableName: "ZED_BUILD", DeliveryScope: docker.DeliveryScopeBuild, Value: "z"},
		{VariableName: "ALPHA_BUILD", DeliveryScope: docker.DeliveryScopeBuild, Value: "a"},
	}
	var req docker.BuildRequest
	e := newTestExecutor(&fakeControlPlane{secrets: fullSecrets()}, &fakeCheckout{result: happyCheckoutResult()},
		&fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}},
		fakeBuildFunc(okBuildResult(), nil, &req))
	e.Execute(context.Background(), lease)

	got := map[string]string{}
	for _, l := range req.Literals {
		got[l.VariableName] = l.Value
	}
	if len(req.Literals) != 2 || got["ALPHA_BUILD"] != "a" || got["ZED_BUILD"] != "z" {
		t.Fatalf("build literals = %+v, want exactly the two build-scoped ones", req.Literals)
	}
	if _, ok := got["RUNTIME_LITERAL"]; ok {
		t.Fatalf("runtime literal reached the build: %+v", req.Literals)
	}
}

func TestExecuteBuildLiteralRefusalIsPermanentConfigurationWithPhaseLine(t *testing.T) {
	lease := testLease()
	lease.LiteralBindings = []agentclient.LiteralBinding{{VariableName: "BUILDKIT_X", DeliveryScope: docker.DeliveryScopeBuild, Value: "sentinel-literal"}}
	refuse := func(context.Context, docker.BuildRequest) (docker.BuildResult, error) {
		return docker.BuildResult{}, fmt.Errorf("%w: variable name %q", docker.ErrInvalidBuildLiteral, "BUILDKIT_X")
	}
	sender := &captureSender{}
	e := shippingExecutor(t, &fakeControlPlane{secrets: fullSecrets()}, refuse, sender)
	out := e.Execute(context.Background(), lease)
	if out.Succeeded || out.FailureClass != queue.FailureClassPermanentConfiguration {
		t.Fatalf("outcome = %+v, want permanent_configuration", out)
	}
	phase := sender.text("phase")
	if !strings.Contains(phase, "build: a build-scoped variable binding was refused") || !strings.Contains(phase, "BUILDKIT_X") {
		t.Fatalf("phase log lacks the refusal line naming the variable: %q", phase)
	}
	if strings.Contains(phase, "sentinel-literal") {
		t.Fatalf("phase log leaked the literal value: %q", phase)
	}
}
