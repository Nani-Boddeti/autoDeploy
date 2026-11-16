package postgres

import (
	"fmt"
	"testing"

	"autodeploy/internal/healthgate"
)

// Golden digests computed from computeConfigDigest BEFORE migration 000028
// widened the health policy (step 22-1a). A default policy (including the
// zero value) must reproduce them byte for byte: ensureRelease compares
// digests and real release rows exist.
const (
	goldenDefaultNoBindings   = "d02cc44eb72b55734e32f34baa4a47963a42fe0f424fb81893b5831fd9c01853"
	goldenDefaultFullBindings = "7b8ce6ddb66b306f9f4e61d71b146eb9dfa4e53e826c9500d7a78deafac95c57"
	goldenDefaultSecretsOnly  = "a8d979da0c40c97788b802ef83a58b910a75c9c328a2c6c28adabfa091dfa9e2"
)

func goldenSnapshotA() frozenExecutionSnapshot {
	return frozenExecutionSnapshot{ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "blue_green", DrainSeconds: 30, HealthPolicyRevision: 1, CPUMillis: 500, MemoryBytes: 268435456, RestartPolicy: "unless-stopped", HealthGatePath: "/healthz", HealthGateHost: "app.internal", ConfigRevision: 0}
}

func goldenSnapshotB() frozenExecutionSnapshot {
	return frozenExecutionSnapshot{ContainerPort: 3000, ContainerScheme: "https", ReleaseStrategy: "stop_then_start", DrainSeconds: 0, HealthPolicyRevision: 1, CPUMillis: 1000, MemoryBytes: 536870912, RestartPolicy: "always", HealthGatePath: "/", HealthGateHost: "web.example.com:8443", ConfigRevision: 7}
}

func goldenBindings() ([]frozenBinding, []frozenLiteral) {
	return []frozenBinding{{VariableName: "Z_KEY", DeliveryScope: "runtime", SecretVersionID: "sv-2"}, {VariableName: "A_KEY", DeliveryScope: "runtime", SecretVersionID: "sv-1"}},
		[]frozenLiteral{{VariableName: "MODE", DeliveryScope: "runtime", LiteralValue: "prod"}}
}

func TestComputeConfigDigestDefaultPolicyMatchesPreChangeGolden(t *testing.T) {
	bind, lit := goldenBindings()
	for _, withExplicitDefaults := range []bool{false, true} {
		a, b := goldenSnapshotA(), goldenSnapshotB()
		if withExplicitDefaults {
			a.HealthPolicy, b.HealthPolicy = healthgate.DefaultPolicy(), healthgate.DefaultPolicy()
		}
		got := map[string][32]byte{
			goldenDefaultNoBindings:   computeConfigDigest(a, nil, nil),
			goldenDefaultFullBindings: computeConfigDigest(b, bind, lit),
			goldenDefaultSecretsOnly:  computeConfigDigest(a, bind, nil),
		}
		for want, digest := range got {
			if hex := fmt.Sprintf("%x", digest); hex != want {
				t.Fatalf("explicitDefaults=%v: digest = %s, want golden %s", withExplicitDefaults, hex, want)
			}
		}
	}
}

func TestComputeConfigDigestEveryNonDefaultPolicyFieldChangesDigest(t *testing.T) {
	base := goldenSnapshotA()
	baseDigest := computeConfigDigest(base, nil, nil)
	variants := map[string]healthgate.Policy{
		"threshold": {SuccessThreshold: 4, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60},
		"interval":  {SuccessThreshold: 3, IntervalSeconds: 3, TimeoutSeconds: 2, DeadlineSeconds: 60},
		"timeout":   {SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 1, DeadlineSeconds: 60},
		"deadline":  {SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 61},
		"tcp port":  {SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 80},
	}
	seen := map[[32]byte]string{baseDigest: "default"}
	for name, p := range variants {
		s := base
		s.HealthPolicy = p
		d := computeConfigDigest(s, nil, nil)
		if prev, dup := seen[d]; dup {
			t.Fatalf("%s digest collides with %s", name, prev)
		}
		seen[d] = name
		if computeConfigDigest(s, nil, nil) != d {
			t.Fatalf("%s digest not deterministic", name)
		}
	}
	// A different TCP port value differs from another port, and from none.
	s80, s81 := base, base
	s80.HealthPolicy = healthgate.Policy{SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 80}
	s81.HealthPolicy = healthgate.Policy{SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 81}
	if computeConfigDigest(s80, nil, nil) == computeConfigDigest(s81, nil, nil) {
		t.Fatal("tcp port 80 and 81 must produce different digests")
	}
}

func TestComputeConfigDigestPolicyPartsKeepBindingOrderStable(t *testing.T) {
	s := goldenSnapshotA()
	s.HealthPolicy = healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 3, TimeoutSeconds: 3, DeadlineSeconds: 90, TCPPort: 8080}
	bind, lit := goldenBindings()
	reversed := []frozenBinding{bind[1], bind[0]}
	if computeConfigDigest(s, bind, lit) != computeConfigDigest(s, reversed, lit) {
		t.Fatal("digest must not depend on binding order for a non-default policy")
	}
	// Policy parts are positioned apart from bindings/literals: moving the
	// same bytes between sections must not collide.
	if computeConfigDigest(s, bind, nil) == computeConfigDigest(s, nil, nil) {
		t.Fatal("bindings must still affect the digest for a non-default policy")
	}
}
