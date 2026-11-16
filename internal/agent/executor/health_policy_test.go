package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/runtime/docker"
)

func TestSTSWindowExtraGrowsWithDeadline(t *testing.T) {
	cases := []struct {
		deadlineSeconds int
		want            time.Duration
	}{
		{0, stsExtraTimeout},  // absent policy = defaults
		{60, stsExtraTimeout}, // default
		{10, stsExtraTimeout}, // shorter never shrinks
		{300, stsExtraTimeout + 240*time.Second},
		{600, stsExtraTimeout + 540*time.Second},
	}
	for _, c := range cases {
		if got := stsWindowExtra(healthgate.Policy{DeadlineSeconds: c.deadlineSeconds}); got != c.want {
			t.Fatalf("deadline %ds: extra = %s, want %s", c.deadlineSeconds, got, c.want)
		}
	}
}

func TestSTSTunedDeadlineNotCutOffByWindow(t *testing.T) {
	for _, tc := range []struct {
		deadline int
		minLeft  time.Duration
		maxLeft  time.Duration
	}{
		{0, 0, 11 * time.Minute},                  // default: unchanged window (rig: 151s + 8m)
		{600, 600*time.Second + 4*time.Minute, 0}, // tuned: gate deadline plus the old pre-gate budget
	} {
		r := newSTSRig(t, true)
		r.lease.HealthPolicy = healthgate.Policy{DeadlineSeconds: tc.deadline}
		var left time.Duration
		r.dk.onGate = func(ctx context.Context) {
			if d, ok := ctx.Deadline(); ok {
				left = time.Until(d)
			}
		}
		out := r.e.Execute(context.Background(), r.lease)
		if !out.HandedOff {
			t.Fatalf("deadline %d: outcome = %+v", tc.deadline, out)
		}
		if left < tc.minLeft || (tc.maxLeft > 0 && left > tc.maxLeft) {
			t.Fatalf("deadline %d: window left at gate = %s (min %s max %s)", tc.deadline, left, tc.minLeft, tc.maxLeft)
		}
	}
}

func TestSTSPassesFrozenPolicyToGate(t *testing.T) {
	r := newSTSRig(t, true)
	want := healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: 8080}
	r.lease.HealthPolicy = want
	r.e.Execute(context.Background(), r.lease)
	if r.dk.gotPolicy != want {
		t.Fatalf("policy = %+v, want %+v", r.dk.gotPolicy, want)
	}
}

// The post-gate remaining-window rule is not weakened by a long deadline.
func TestSTSTunedPolicyStillChecksWindowAfterGate(t *testing.T) {
	r := newSTSRig(t, true)
	r.lease.HealthPolicy = healthgate.Policy{DeadlineSeconds: 600}
	rel, rec := r.directRelease()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary := rel.beforeSwitch(ctx, rec)
	if !strings.Contains(summary, "authorized window too short") || len(r.act.writes) != 0 {
		t.Fatalf("summary = %q writes=%d", summary, len(r.act.writes))
	}
}

func TestBlueGreenPassesFrozenPolicyToGate(t *testing.T) {
	want := healthgate.Policy{SuccessThreshold: 2, IntervalSeconds: 3, TimeoutSeconds: 2, DeadlineSeconds: 90, TCPPort: 5432}
	lease := testLease()
	lease.HealthPolicy = want
	cp := &fakeControlPlane{secrets: fullSecrets()}
	co := &fakeCheckout{result: happyCheckoutResult()}
	dk := &fakeDocker{ensureResult: testInspect("networkabc123"), gateResult: docker.HealthGateResult{Success: true}}
	build := fakeBuildFunc(docker.BuildResult{ImageDigest: "sha256:" + ones(64), ConfigDigest: "sha256:" + ones(64)}, nil, nil)
	e := newTestExecutor(cp, co, dk, build)
	e.Execute(context.Background(), lease)
	if dk.gotPolicy != want {
		t.Fatalf("policy = %+v, want %+v", dk.gotPolicy, want)
	}
}

func TestRollbackPassesFrozenPolicyToGate(t *testing.T) {
	want := healthgate.Policy{SuccessThreshold: 2, IntervalSeconds: 3, TimeoutSeconds: 2, DeadlineSeconds: 90, TCPPort: 5432}
	for _, strategy := range []string{"blue_green", "stop_then_start"} {
		r := newRollbackRig(t, strategy, true, false)
		r.lease.HealthPolicy = want
		r.e.Execute(context.Background(), r.lease)
		if r.dk.gotPolicy != want {
			t.Fatalf("%s: policy = %+v, want %+v", strategy, r.dk.gotPolicy, want)
		}
	}
}
