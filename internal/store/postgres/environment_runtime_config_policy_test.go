//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/healthgate"
)

func tunedPolicy() healthgate.Policy {
	return healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: 8081}
}

// TestEnvironmentRuntimeConfigHealthPolicyRoundTripAndCAS proves 22-1a's
// five policy fields survive create -> get -> CAS update -> get, that a
// stale update changes nothing, and that clearing the TCP port stores NULL.
func TestEnvironmentRuntimeConfigHealthPolicyRoundTripAndCAS(t *testing.T) {
	f := newERConfigFixture(t, 220)
	env := "environment-policy-" + t.Name()
	f.insertOwnedEnvironment(t, env, "policy-"+t.Name())
	ctx := context.Background()

	in := validInput()
	in.HealthPolicy = tunedPolicy()
	created, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.HealthPolicy != tunedPolicy() || created.HealthPolicyRevision != 1 {
		t.Fatalf("created policy = %+v rev=%d", created.HealthPolicy, created.HealthPolicyRevision)
	}
	got, _, found, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil || !found || got.HealthPolicy != tunedPolicy() {
		t.Fatalf("get after create = %+v found=%v err=%v", got.HealthPolicy, found, err)
	}

	edited := validInput()
	edited.HealthPolicy = healthgate.Policy{SuccessThreshold: 2, IntervalSeconds: 10, TimeoutSeconds: 10, DeadlineSeconds: 30}
	updated, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, edited, 0)
	if err != nil || updated.Revision != 1 || updated.HealthPolicy != edited.HealthPolicy {
		t.Fatalf("update = %+v err=%v", updated, err)
	}
	var tcpIsNull bool
	if err := f.pool.QueryRow(ctx, `SELECT health_tcp_port IS NULL FROM environment_runtime_configs WHERE environment_id=$1`, env).Scan(&tcpIsNull); err != nil || !tcpIsNull {
		t.Fatalf("health_tcp_port IS NULL = %v err=%v, want true after clearing", tcpIsNull, err)
	}

	stale := validInput()
	stale.HealthPolicy = tunedPolicy()
	if _, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, stale, 0); !errors.Is(err, ErrEnvironmentRuntimeConfigConflict) {
		t.Fatalf("stale update err = %v, want conflict", err)
	}
	again, _, _, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil || again.Revision != 1 || again.HealthPolicy != edited.HealthPolicy {
		t.Fatalf("state after stale update = %+v err=%v", again, err)
	}
}

// TestEnvironmentRuntimeConfigZeroPolicyMeansDefaults documents and tests
// the compatibility rule for callers that never set HealthPolicy: the zero
// value is normalized to ADR 0005's defaults (never "0 seconds"), on create
// and on update, and what is stored and returned is the defaulted value.
func TestEnvironmentRuntimeConfigZeroPolicyMeansDefaults(t *testing.T) {
	f := newERConfigFixture(t, 221)
	env := "environment-policy-zero-" + t.Name()
	f.insertOwnedEnvironment(t, env, "policy-zero-"+t.Name())
	ctx := context.Background()

	created, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput())
	if err != nil || created.HealthPolicy != healthgate.DefaultPolicy() {
		t.Fatalf("create with zero policy = %+v err=%v, want defaults", created.HealthPolicy, err)
	}
	var s, i, to, d int
	var tcpIsNull bool
	q := `SELECT health_success_threshold, health_interval_seconds, health_timeout_seconds, health_deadline_seconds, health_tcp_port IS NULL FROM environment_runtime_configs WHERE environment_id=$1`
	if err := f.pool.QueryRow(ctx, q, env).Scan(&s, &i, &to, &d, &tcpIsNull); err != nil || s != 3 || i != 2 || to != 2 || d != 60 || !tcpIsNull {
		t.Fatalf("stored = %d/%d/%d/%d null=%v err=%v", s, i, to, d, tcpIsNull, err)
	}

	// Update from a tuned policy back to the zero value restores defaults.
	tuned := validInput()
	tuned.HealthPolicy = tunedPolicy()
	if _, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, tuned, 0); err != nil {
		t.Fatal(err)
	}
	reset, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput(), 1)
	if err != nil || reset.HealthPolicy != healthgate.DefaultPolicy() {
		t.Fatalf("update with zero policy = %+v err=%v, want defaults", reset.HealthPolicy, err)
	}
}

// TestEnvironmentRuntimeConfigHealthPolicyOutOfBoundsRejectedBeforeSQL proves
// every bound and both cross rules are refused by Go validation with the
// repository's existing typed error, and that no row was written.
func TestEnvironmentRuntimeConfigHealthPolicyOutOfBoundsRejectedBeforeSQL(t *testing.T) {
	f := newERConfigFixture(t, 222)
	env := "environment-policy-bad-" + t.Name()
	f.insertOwnedEnvironment(t, env, "policy-bad-"+t.Name())
	ctx := context.Background()

	d := healthgate.DefaultPolicy()
	mut := func(fn func(*healthgate.Policy)) healthgate.Policy { p := d; fn(&p); return p }
	bad := map[string]healthgate.Policy{
		"threshold 11":           mut(func(p *healthgate.Policy) { p.SuccessThreshold = 11; p.DeadlineSeconds = 600 }),
		"threshold negative":     mut(func(p *healthgate.Policy) { p.SuccessThreshold = -1 }),
		"interval 31":            mut(func(p *healthgate.Policy) { p.IntervalSeconds = 31 }),
		"interval negative":      mut(func(p *healthgate.Policy) { p.IntervalSeconds = -1 }),
		"timeout 31":             mut(func(p *healthgate.Policy) { p.IntervalSeconds = 30; p.TimeoutSeconds = 31 }),
		"timeout above interval": mut(func(p *healthgate.Policy) { p.TimeoutSeconds = 3 }),
		"deadline 9":             mut(func(p *healthgate.Policy) { p.DeadlineSeconds = 9 }),
		"deadline 601":           mut(func(p *healthgate.Policy) { p.DeadlineSeconds = 601 }),
		"deadline below product": mut(func(p *healthgate.Policy) {
			p.SuccessThreshold = 10
			p.IntervalSeconds = 10
			p.TimeoutSeconds = 10
			p.DeadlineSeconds = 99
		}),
		"tcp port 65536":    mut(func(p *healthgate.Policy) { p.TCPPort = 65536 }),
		"tcp port negative": mut(func(p *healthgate.Policy) { p.TCPPort = -1 }),
	}
	for name, p := range bad {
		in := validInput()
		in.HealthPolicy = p
		if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, in); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Errorf("%s: create err = %v, want ErrServerAuthorizationDenied", name, err)
		}
	}
	if _, _, found, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env); err != nil || found {
		t.Fatalf("found=%v err=%v after all-invalid creates", found, err)
	}

	// Edges are accepted (create once, then update through the rest).
	edges := []healthgate.Policy{
		{SuccessThreshold: 1, IntervalSeconds: 1, TimeoutSeconds: 1, DeadlineSeconds: 10, TCPPort: 1},
		{SuccessThreshold: 10, IntervalSeconds: 30, TimeoutSeconds: 30, DeadlineSeconds: 600, TCPPort: 65535},
		{SuccessThreshold: 3, IntervalSeconds: 5, TimeoutSeconds: 5, DeadlineSeconds: 15},
	}
	var revision int64
	for i, p := range edges {
		in := validInput()
		in.HealthPolicy = p
		var err error
		if i == 0 {
			_, err = f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, in)
		} else {
			var cfg RuntimeConfig
			cfg, err = f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, in, revision)
			revision = cfg.Revision
		}
		if err != nil {
			t.Fatalf("edge %+v rejected: %v", p, err)
		}
	}
	for name, p := range bad {
		in := validInput()
		in.HealthPolicy = p
		if _, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, in, revision); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Errorf("%s: update err = %v, want ErrServerAuthorizationDenied", name, err)
		}
	}
	got, _, _, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil || got.Revision != revision || got.HealthPolicy != edges[2].WithDefaults() {
		t.Fatalf("state after rejected updates = %+v err=%v", got, err)
	}
}
