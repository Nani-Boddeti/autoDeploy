package agentclient_test

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"reflect"
	"testing"

	agentclient "autodeploy/internal/agent/client"
	agenthttp "autodeploy/internal/agent/http"
	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
	"autodeploy/internal/server"
)

type contractActivator struct{ result queue.ActivationResult }

func (f contractActivator) Activate(context.Context, string, [32]byte, int64, int64, [32]byte) (queue.ActivationResult, error) {
	return f.result, nil
}

func (f contractActivator) ActivateWithPredecessorHealth(context.Context, string, [32]byte, int64, int64, [32]byte) (queue.ActivationResult, error) {
	return f.result, nil
}

// Cross-side wire contract (22-D2): the real control-plane handler's
// predecessor_health block must decode, through the real agent client, to
// exactly the values the activator returned. Renaming a JSON tag on either
// side makes the target nil (fail-closed) and this test fail.
func TestPredecessorHealthWireContract(t *testing.T) {
	for _, tcpPort := range []int{0, 9000} {
		fake := contractActivator{result: queue.ActivationResult{
			OperationID: "7", RouteRevision: 3, Hostname: "app.example.com", EntrypointName: "websecure",
			CertResolverName: "letsencrypt-production", BackendRuntimeInstanceID: "43",
			RouteDigest:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			PredecessorReleaseID: "rel-1", PredecessorRuntimeInstanceID: "42",
			PredecessorHealth: &queue.PredecessorHealth{
				ContainerPort: 8081, ContainerScheme: "https", HealthPolicyRevision: 1,
				HealthGatePath: "/ready", HealthGateHost: "old.example.com",
				HealthPolicy: healthgate.Policy{SuccessThreshold: 2, IntervalSeconds: 5, TimeoutSeconds: 4, DeadlineSeconds: 50, TCPPort: tcpPort},
			},
		}}
		srv := httptest.NewTLSServer(agenthttp.Handler{Activator: fake})
		token, err := server.NewCredentialToken(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		c, err := agentclient.NewClient(srv.URL, "server_test", token, srv.Client(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var img [32]byte
		resp, err := c.Activate(context.Background(), 43, 3, img, true)
		srv.Close()
		if err != nil {
			t.Fatalf("tcp_port=%d: %v", tcpPort, err)
		}
		ph := fake.result.PredecessorHealth
		want := &agentclient.PredecessorHealthTarget{
			Host: ph.HealthGateHost, Path: ph.HealthGatePath, Port: ph.ContainerPort,
			Scheme: ph.ContainerScheme, PolicyRevision: ph.HealthPolicyRevision, Policy: ph.HealthPolicy,
		}
		if !reflect.DeepEqual(resp.PredecessorHealth, want) {
			t.Fatalf("tcp_port=%d: decoded %+v, want %+v", tcpPort, resp.PredecessorHealth, want)
		}
	}
}
