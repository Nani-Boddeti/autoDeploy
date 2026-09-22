package postgres

import "testing"

// TestComputeConfigDigestChangesWithHealthGatePathAndHost proves Slice 10
// Step 0's digest-widening requirement: computeConfigDigest's inputs must
// include HealthGatePath/HealthGateHost, so a release's config_digest
// changes when either one changes -- otherwise an administrator could edit
// the health gate configuration after attempt 1 without the deployment's
// recorded config_digest ever reflecting that change, defeating
// ensureRelease's own corrupt-state mismatch check. This is a pure unit
// test (no database) precisely because computeConfigDigest is a pure
// function of its inputs.
func TestComputeConfigDigestChangesWithHealthGatePathAndHost(t *testing.T) {
	base := frozenExecutionSnapshot{
		ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "blue_green",
		DrainSeconds: 30, HealthPolicyRevision: 1, CPUMillis: 500, MemoryBytes: 268435456,
		RestartPolicy: "unless-stopped", HealthGatePath: "/healthz", HealthGateHost: "app.internal",
		ConfigRevision: 0,
	}
	baseDigest := computeConfigDigest(base, nil, nil)

	diffPath := base
	diffPath.HealthGatePath = "/other"
	if computeConfigDigest(diffPath, nil, nil) == baseDigest {
		t.Fatal("digest unchanged after HealthGatePath changed, want a different digest")
	}

	diffHost := base
	diffHost.HealthGateHost = "other.internal"
	if computeConfigDigest(diffHost, nil, nil) == baseDigest {
		t.Fatal("digest unchanged after HealthGateHost changed, want a different digest")
	}

	// Identical inputs still produce an identical digest (sanity check that
	// the two failures above are about the specific field, not general
	// nondeterminism).
	if computeConfigDigest(base, nil, nil) != baseDigest {
		t.Fatal("digest is not deterministic for identical inputs")
	}
}
