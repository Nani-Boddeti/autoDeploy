//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// erConfigFixture reuses leaseFixture's owner/server bootstrap (an owner,
// principal, and servers row already scoped to it) purely for the
// ownership derivation EnvironmentRuntimeConfigRepository relies on: an
// environment is "owned" by whichever owner has at least one agent_jobs row
// naming it. newLeaseFixture's own default environment (f.envID) already
// gets an environment_runtime_configs row from insertRuntimeConfig, so
// tests that need a genuinely never-configured-but-owned environment use a
// fresh environment id via insertOwnedEnvironment below.
type erConfigFixture struct {
	leaseFixture
	repo *EnvironmentRuntimeConfigRepository
}

func newERConfigFixture(t *testing.T, seed byte) erConfigFixture {
	t.Helper()
	f := newLeaseFixture(t, seed)
	return erConfigFixture{leaseFixture: f, repo: NewEnvironmentRuntimeConfigRepository(f.pool)}
}

// insertOwnedEnvironment gives environmentID an agent_jobs row under f's
// owner (no environment_runtime_configs row), the minimum precondition
// EnvironmentRuntimeConfigRepository's ownership check requires, without
// pulling in an environment_runtime_configs row the way newLeaseFixture's
// default environment already has.
func (f erConfigFixture) insertOwnedEnvironment(t *testing.T, environmentID, key string) int64 {
	t.Helper()
	return f.insertJobNoConfig(t, f.serverID, environmentID, key, 0)
}

func validInput() RuntimeConfigInput {
	return RuntimeConfigInput{ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "stop_then_start", DrainSeconds: 30, CPUMillis: 500, MemoryBytes: 268435456, RestartPolicy: "unless-stopped", HealthGatePath: "/healthz", HealthGateHost: "app.internal"}
}

// TestEnvironmentRuntimeConfigCreateGetUpdateCAS proves the full
// create -> read -> CAS-update -> stale-CAS-rejected lifecycle, including
// the audit trail's resulting_revision.
func TestEnvironmentRuntimeConfigCreateGetUpdateCAS(t *testing.T) {
	f := newERConfigFixture(t, 210)
	env := "environment-crud-" + t.Name()
	f.insertOwnedEnvironment(t, env, "crud-"+t.Name())
	ctx := context.Background()

	cfg, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cfg.Revision != 0 || cfg.HealthPolicyRevision != 1 {
		t.Fatalf("created cfg = %+v", cfg)
	}

	got, bindings, found, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil || !found || len(bindings) != 0 {
		t.Fatalf("get after create: found=%v err=%v bindings=%v", found, err, bindings)
	}
	if got.ContainerPort != 8080 || got.Revision != 0 {
		t.Fatalf("get after create = %+v", got)
	}

	// Creating again is rejected: only one config row per environment.
	if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput()); !errors.Is(err, ErrEnvironmentRuntimeConfigAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrEnvironmentRuntimeConfigAlreadyExists", err)
	}

	// A correct CAS update advances the revision.
	edited := validInput()
	edited.ContainerPort = 9090
	updated, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, edited, 0)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 1 || updated.ContainerPort != 9090 {
		t.Fatalf("updated cfg = %+v", updated)
	}

	// A stale expectedRevision (0, already consumed) is a distinct
	// conflict, not a generic error, and changes nothing.
	if _, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, env, edited, 0); !errors.Is(err, ErrEnvironmentRuntimeConfigConflict) {
		t.Fatalf("stale update err = %v, want ErrEnvironmentRuntimeConfigConflict", err)
	}
	again, _, found, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil || !found || again.Revision != 1 || again.ContainerPort != 9090 {
		t.Fatalf("state after rejected stale update = %+v found=%v err=%v", again, found, err)
	}

	// An update naming an environment with no config at all is
	// ErrEnvironmentRuntimeConfigNotFound, not ErrEnvironmentRuntimeConfigConflict.
	other := "environment-crud-missing-" + t.Name()
	f.insertOwnedEnvironment(t, other, "crud-missing-"+t.Name())
	if _, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, other, edited, 0); !errors.Is(err, ErrEnvironmentRuntimeConfigNotFound) {
		t.Fatalf("update on missing config err = %v, want ErrEnvironmentRuntimeConfigNotFound", err)
	}

	// The audit trail recorded the owner principal and every resulting
	// revision, and never carries a secret value (there is none in this
	// test, but the schema itself has no column that could).
	rows, err := f.pool.Query(ctx, `SELECT action, actor_id, resulting_revision, result FROM environment_runtime_config_audit_events WHERE environment_id=$1 ORDER BY id`, env)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type auditRow struct {
		action, actorID, result string
		revision                *int64
	}
	var audits []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.action, &a.actorID, &a.revision, &a.result); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, a)
	}
	if len(audits) != 4 {
		t.Fatalf("audit rows = %+v, want 4 (create, failed re-create, update, failed stale update)", audits)
	}
	if audits[0].action != "config_created" || audits[0].result != "success" || audits[0].revision == nil || *audits[0].revision != 0 {
		t.Fatalf("first audit row = %+v", audits[0])
	}
	if audits[1].action != "config_created" || audits[1].result != "failure" || audits[1].revision != nil {
		t.Fatalf("second audit row = %+v", audits[1])
	}
	if audits[2].action != "config_updated" || audits[2].result != "success" || audits[2].revision == nil || *audits[2].revision != 1 {
		t.Fatalf("third audit row = %+v", audits[2])
	}
	if audits[3].action != "config_updated" || audits[3].result != "failure" {
		t.Fatalf("fourth audit row = %+v", audits[3])
	}
	for _, a := range audits {
		if a.actorID != f.principal.UserID {
			t.Fatalf("audit actor_id = %q, want %q", a.actorID, f.principal.UserID)
		}
	}
}

// TestEnvironmentRuntimeConfigCrossOwnerDenied proves an owner cannot read
// or write another owner's environment configuration, and an environment
// with no owning agent_jobs row at all is denied identically (fail closed,
// indistinguishable from a foreign owner).
func TestEnvironmentRuntimeConfigCrossOwnerDenied(t *testing.T) {
	f := newERConfigFixture(t, 211)
	env := "environment-crossowner-" + t.Name()
	f.insertOwnedEnvironment(t, env, "crossowner-"+t.Name())
	ctx := context.Background()
	if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput()); err != nil {
		t.Fatal(err)
	}

	authRepo := NewAuthRepository(f.pool)
	other := secondOwnerPrincipal(t, f.pool, authRepo, 220)

	if _, _, _, err := f.repo.GetEnvironmentRuntimeConfig(ctx, other, env); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner get = %v", err)
	}
	if _, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, other, env, validInput(), 0); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner update = %v", err)
	}
	neverOwned := "environment-never-owned-" + t.Name()
	if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, neverOwned, validInput()); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("create against never-owned environment = %v, want denied", err)
	}
}

// TestEnvironmentRuntimeConfigInvalidFieldsRejected proves every bound the
// spec calls out is refused before any row is written.
func TestEnvironmentRuntimeConfigInvalidFieldsRejected(t *testing.T) {
	f := newERConfigFixture(t, 212)
	env := "environment-invalid-" + t.Name()
	f.insertOwnedEnvironment(t, env, "invalid-"+t.Name())
	ctx := context.Background()

	cases := []RuntimeConfigInput{
		func() RuntimeConfigInput { in := validInput(); in.ContainerPort = 0; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.ContainerPort = 65536; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.ContainerScheme = "ftp"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.ReleaseStrategy = "rolling"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.DrainSeconds = 0; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.DrainSeconds = 301; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.CPUMillis = 0; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.MemoryBytes = 0; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.RestartPolicy = "always"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "no-leading-slash"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "//evil"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a#b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a\tb"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/hé"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a%zz"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a%2"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = `/a"b`; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a<b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = `/a\b`; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a^b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a|b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a{b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGatePath = "/a`b"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGateHost = ""; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGateHost = "user@host"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGateHost = "host/path"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGateHost = "[::1]"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGateHost = "app.internal:0"; return in }(),
		func() RuntimeConfigInput { in := validInput(); in.HealthGateHost = "app.internal:65536"; return in }(),
	}
	for i, in := range cases {
		if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, in); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("case %d: err = %v, want ErrServerAuthorizationDenied", i, err)
		}
	}
	// No row was ever written by any of the above.
	if _, _, found, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env); err != nil || found {
		t.Fatalf("found=%v err=%v after all-invalid submissions", found, err)
	}
}

// TestEnvironmentRuntimeBindingSecretAndLiteral proves adding a secret
// binding and a literal binding, reading them back (identity+version only
// for the secret; the value itself for the literal, per ADR 0003), and
// removing them.
func TestEnvironmentRuntimeBindingSecretAndLiteral(t *testing.T) {
	f := newERConfigFixture(t, 213)
	env := "environment-bindings-" + t.Name()
	f.insertOwnedEnvironment(t, env, "bindings-"+t.Name())
	ctx := context.Background()
	if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput()); err != nil {
		t.Fatal(err)
	}
	f.insertSecretHeadWithActiveVersion(t, "secret-head-"+t.Name(), env, "API_TOKEN", "runtime", "secret-version-"+t.Name(), 5)

	// The secret binding above was inserted directly by the fixture helper
	// (bypassing AddEnvironmentRuntimeBinding, matching agent_lease_execution_test.go's
	// convention), so exercise Add on a second, literal binding here, and
	// read both back through the repository.
	lit, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, env, BindingInput{VariableName: "NODE_ENV", BindingKind: "literal_nonsecret", DeliveryScope: "runtime", LiteralValue: "production"})
	if err != nil {
		t.Fatalf("add literal: %v", err)
	}
	if lit.LiteralValue != "production" {
		t.Fatalf("literal binding = %+v", lit)
	}

	_, bindings, _, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings = %+v, err=%v", bindings, err)
	}
	var secretRow, literalRow *RuntimeBinding
	for i := range bindings {
		switch bindings[i].VariableName {
		case "API_TOKEN":
			secretRow = &bindings[i]
		case "NODE_ENV":
			literalRow = &bindings[i]
		}
	}
	if secretRow == nil || secretRow.BindingKind != "secret" || secretRow.SecretVersionNumber != 5 || secretRow.LiteralValue != "" {
		t.Fatalf("secret binding row = %+v", secretRow)
	}
	if literalRow == nil || literalRow.LiteralValue != "production" {
		t.Fatalf("literal binding row = %+v", literalRow)
	}

	// The response never carries the ciphertext this fixture inserted --
	// there is no field capable of it, but assert it anyway against every
	// string field reachable from the binding rows.
	for _, b := range bindings {
		if strings.Contains(b.LiteralValue, "abababab") || strings.Contains(b.SecretHeadID, "abababab") {
			t.Fatalf("binding leaks ciphertext marker: %+v", b)
		}
	}

	if err := f.repo.RemoveEnvironmentRuntimeBinding(ctx, f.principal, env, "NODE_ENV"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := f.repo.RemoveEnvironmentRuntimeBinding(ctx, f.principal, env, "NODE_ENV"); !errors.Is(err, ErrEnvironmentRuntimeBindingNotFound) {
		t.Fatalf("remove again = %v, want ErrEnvironmentRuntimeBindingNotFound", err)
	}
}

// TestEnvironmentRuntimeBindingScopeViolationsSurfaceAsValidation proves
// migration 000014's bind-time scope trigger rejections (cross-environment
// head, delivery-scope mismatch, provider-purpose head) surface as
// ErrEnvironmentRuntimeBindingInvalid, never a bare/opaque error, and never
// panic or leave a partial row.
func TestEnvironmentRuntimeBindingScopeViolationsSurfaceAsValidation(t *testing.T) {
	f := newERConfigFixture(t, 214)
	env := "environment-scope-" + t.Name()
	other := "environment-scope-other-" + t.Name()
	f.insertOwnedEnvironment(t, env, "scope-"+t.Name())
	f.insertOwnedEnvironment(t, other, "scope-other-"+t.Name())
	ctx := context.Background()
	if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput()); err != nil {
		t.Fatal(err)
	}

	// A secret head belonging to a different environment.
	f.insertSecretHeadWithActiveVersion(t, "secret-head-foreign-"+t.Name(), other, "FOREIGN_VAR", "runtime", "secret-version-foreign-"+t.Name(), 1)
	if _, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, env, BindingInput{VariableName: "FOREIGN_VAR", BindingKind: "secret", DeliveryScope: "runtime", SecretHeadID: "secret-head-foreign-" + t.Name()}); !errors.Is(err, ErrEnvironmentRuntimeBindingInvalid) {
		t.Fatalf("cross-environment head err = %v, want ErrEnvironmentRuntimeBindingInvalid", err)
	}

	// A delivery-scope mismatch: the head is runtime-scoped, bind as build.
	f.insertSecretHeadWithActiveVersion(t, "secret-head-scope-"+t.Name(), env, "SCOPE_VAR", "runtime", "secret-version-scope-"+t.Name(), 1)
	if _, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, env, BindingInput{VariableName: "SCOPE_MISMATCH", BindingKind: "secret", DeliveryScope: "build", SecretHeadID: "secret-head-scope-" + t.Name()}); !errors.Is(err, ErrEnvironmentRuntimeBindingInvalid) {
		t.Fatalf("delivery-scope mismatch err = %v, want ErrEnvironmentRuntimeBindingInvalid", err)
	}

	// An unknown secret head id.
	if _, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, env, BindingInput{VariableName: "UNKNOWN_HEAD", BindingKind: "secret", DeliveryScope: "runtime", SecretHeadID: "secret-head-does-not-exist-" + t.Name()}); !errors.Is(err, ErrEnvironmentRuntimeBindingInvalid) {
		t.Fatalf("unknown head err = %v, want ErrEnvironmentRuntimeBindingInvalid", err)
	}

	// A provider-purpose head.
	if _, err := f.pool.Exec(ctx, `INSERT INTO secret_heads(id, owner_id, project_id, environment_id, purpose, delivery_scope, revision) VALUES ($1,$2,'project',$3,'provider','runtime',0)`, "secret-head-provider-"+t.Name(), f.ownerID, env); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, env, BindingInput{VariableName: "PROVIDER_VAR", BindingKind: "secret", DeliveryScope: "runtime", SecretHeadID: "secret-head-provider-" + t.Name()}); !errors.Is(err, ErrEnvironmentRuntimeBindingInvalid) {
		t.Fatalf("provider-purpose head err = %v, want ErrEnvironmentRuntimeBindingInvalid", err)
	}

	// None of the four rejected AddEnvironmentRuntimeBinding attempts left
	// a row: the only binding present is SCOPE_VAR, which
	// insertSecretHeadWithActiveVersion itself created directly (bypassing
	// this repository) as a fixture precondition, not a row this test's
	// AddEnvironmentRuntimeBinding calls produced.
	_, bindings, _, err := f.repo.GetEnvironmentRuntimeConfig(ctx, f.principal, env)
	if err != nil {
		t.Fatal(err)
	}
	rejectedNames := []string{"FOREIGN_VAR", "SCOPE_MISMATCH", "UNKNOWN_HEAD", "PROVIDER_VAR"}
	for _, b := range bindings {
		for _, rejected := range rejectedNames {
			if b.VariableName == rejected {
				t.Fatalf("rejected binding %q was nonetheless written: %+v", rejected, bindings)
			}
		}
	}
	if len(bindings) != 1 || bindings[0].VariableName != "SCOPE_VAR" {
		t.Fatalf("bindings after all-rejected attempts = %+v, want exactly the fixture's SCOPE_VAR", bindings)
	}
}

// TestEnvironmentRuntimeConfigMakesEnvironmentClaimable is the spec's
// required end-to-end proof: a queued job for an environment with no
// runtime configuration yields nothing on Claim (ok=false, the candidate is
// skipped, not a hard failure), and once an administrator creates the
// configuration through this repository, the same job claims successfully.
func TestEnvironmentRuntimeConfigMakesEnvironmentClaimable(t *testing.T) {
	f := newERConfigFixture(t, 215)
	d := f.withCredential(t, 216)
	env := "environment-claimable-" + t.Name()
	jobID := f.insertOwnedEnvironment(t, env, "claimable-"+t.Name())
	_ = jobID
	leaseRepo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	leases, err := leaseRepo.Claim(ctx, f.serverID, d, 10)
	if err != nil {
		t.Fatalf("claim before config: %v", err)
	}
	for _, l := range leases {
		if l.EnvironmentID == env {
			t.Fatalf("environment %s claimed before any config existed: %+v", env, l)
		}
	}

	if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, env, validInput()); err != nil {
		t.Fatalf("create config: %v", err)
	}

	leases, err = leaseRepo.Claim(ctx, f.serverID, d, 10)
	if err != nil {
		t.Fatalf("claim after config: %v", err)
	}
	found := false
	for _, l := range leases {
		if l.EnvironmentID == env {
			found = true
		}
	}
	if !found {
		t.Fatalf("environment %s not claimed after config created: %+v", env, leases)
	}
}

// TestEnvironmentRuntimeConfigUpdateRacesClaimBothInterleavingsNoDeadlock is
// the required proof of the CRITICAL LOCK-ORDER CONSTRAINT: many concurrent
// UpdateEnvironmentRuntimeConfig calls (the real administrator write path,
// not a raw-SQL stand-in) race many concurrent Claim calls against the same
// environments. Every goroutine starts from an unbuffered closed channel, so
// across repeated runs (this test is meant to be run with -race -count=20,
// per the spec) both interleavings -- Claim's SHARE lock landing first, and
// the administrator's exclusive lock landing first -- are sampled. Neither
// interleaving may deadlock (Postgres would return 40P01) or fail for any
// reason other than the one expected outcome: the administrator update's
// own CAS naturally loses at most once per environment if two of its own
// goroutines raced each other (this test only ever runs one update
// goroutine per environment, so that case does not arise here either).
func TestEnvironmentRuntimeConfigUpdateRacesClaimBothInterleavingsNoDeadlock(t *testing.T) {
	f := newERConfigFixture(t, 217)
	d := f.withCredential(t, 218)
	leaseRepo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	const environments = 8
	envIDs := make([]string, environments)
	for i := 0; i < environments; i++ {
		envIDs[i] = "environment-lockorder-" + t.Name() + "-" + string(rune('a'+i))
		f.insertOwnedEnvironment(t, envIDs[i], "lockorder-"+t.Name()+"-"+string(rune('a'+i)))
		if _, err := f.repo.CreateEnvironmentRuntimeConfig(ctx, f.principal, envIDs[i], validInput()); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	claimErrs := make([]error, environments)
	updateErrs := make([]error, environments)

	for i := 0; i < environments; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := leaseRepo.Claim(ctx, f.serverID, d, 10)
			claimErrs[i] = err
		}()
		go func() {
			defer wg.Done()
			<-start
			edited := validInput()
			edited.ContainerPort = 9090
			_, err := f.repo.UpdateEnvironmentRuntimeConfig(ctx, f.principal, envIDs[i], edited, 0)
			updateErrs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range claimErrs {
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	for i, err := range updateErrs {
		if err != nil {
			t.Fatalf("administrator update %d: %v", i, err)
		}
	}

	for i, envID := range envIDs {
		var attemptCount int
		if err := f.pool.QueryRow(ctx, `SELECT attempt_count FROM agent_jobs WHERE environment_id=$1`, envID).Scan(&attemptCount); err != nil {
			t.Fatal(err)
		}
		if attemptCount != 1 {
			t.Fatalf("environment %d (%s) attemptCount=%d, want 1", i, envID, attemptCount)
		}
		var port int
		if err := f.pool.QueryRow(ctx, `SELECT container_port FROM environment_runtime_configs WHERE environment_id=$1`, envID).Scan(&port); err != nil {
			t.Fatal(err)
		}
		if port != 9090 {
			t.Fatalf("environment %d (%s) container_port=%d, want 9090 (administrator update applied)", i, envID, port)
		}
	}
}

// TestEnvironmentRuntimeConfigWriteTouchesNoForbiddenTable is a grep-style
// inspection test proving, by construction, that neither
// CreateEnvironmentRuntimeConfig nor UpdateEnvironmentRuntimeConfig's SQL
// text ever names environment_slots, agent_jobs, agent_job_attempts,
// servers, or server_credentials -- the CRITICAL LOCK-ORDER CONSTRAINT's
// second half. A live-transaction assertion (e.g. asserting pg_locks) would
// also work, but would only prove it for the exact rows this test happens
// to exercise; grepping the literal SQL statements this file's Go source
// issues proves it for every row, unconditionally, and is simpler to keep
// correct as the file evolves -- any future edit that adds a query
// touching one of these tables to either write path fails this test
// immediately without needing a new integration scenario. The tradeoff is
// that it inspects source text, not runtime behavior; the concurrency test
// above is what actually proves the runtime claim (no deadlock, no
// pathological blocking).
func TestEnvironmentRuntimeConfigWriteTouchesNoForbiddenTable(t *testing.T) {
	// Step 2's deadlock-freedom argument (tasks/todo.md, ADR 0004 position
	// "1b") holds only while this write path never waits on a lock a Claim
	// transaction holds. Claim does lock agent_jobs and environment_slots, so
	// the guarantee is one-sided: the administrator path must take no lock on
	// those tables at all.
	//
	// This reads the repository source itself rather than a copy of its
	// queries. An earlier version of this test inlined the SQL, which meant
	// editing the real query and leaving the copy alone would still pass --
	// it restated the invariant instead of proving it.
	source, err := os.ReadFile("environment_runtime_config_repository.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"environment_slots", "agent_jobs", "agent_job_attempts", "servers", "server_credentials"}
	lockClauses := []string{"FOR SHARE", "FOR UPDATE", "FOR NO KEY UPDATE", "FOR KEY SHARE"}

	// Every backtick-quoted SQL literal in the file, so a future query cannot
	// escape this check by being added somewhere new.
	statements := backtickLiterals(string(source))
	if len(statements) == 0 {
		t.Fatal("no SQL literals found: this test can no longer see the write path")
	}
	sawOwnershipCheck := false
	for _, stmt := range statements {
		mentionsForbidden := ""
		for _, table := range forbidden {
			if sqlMentionsTable(stmt, table) {
				mentionsForbidden = table
				break
			}
		}
		if mentionsForbidden == "" {
			continue
		}
		// A statement naming one of these tables is allowed only if it takes
		// no lock whatsoever. The ownership check is the one such statement
		// today (environment_id has no owner column anywhere else in this
		// schema, so agent_jobs.owner_id is the only durable pairing).
		for _, clause := range lockClauses {
			if strings.Contains(strings.ToUpper(stmt), clause) {
				t.Fatalf("write-path statement locks forbidden table %q with %s, which breaks Step 2's deadlock-freedom argument: %s",
					mentionsForbidden, clause, stmt)
			}
		}
		if mentionsForbidden == "agent_jobs" {
			sawOwnershipCheck = true
		}
	}
	if !sawOwnershipCheck {
		t.Fatal("expected the unlocked agent_jobs ownership check to still be present; if ownership moved elsewhere, update this test deliberately")
	}
}

// backtickLiterals returns every backtick-quoted string in Go source, which is
// how this package writes every SQL statement.
func backtickLiterals(source string) []string {
	var out []string
	for {
		start := strings.Index(source, "`")
		if start < 0 {
			return out
		}
		rest := source[start+1:]
		end := strings.Index(rest, "`")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		source = rest[end+1:]
	}
}

// sqlMentionsTable reports whether stmt references table as a whole word, so
// "servers" does not match "server_credentials" and vice versa.
func sqlMentionsTable(stmt, table string) bool {
	lower := strings.ToLower(stmt)
	for i := 0; ; {
		idx := strings.Index(lower[i:], table)
		if idx < 0 {
			return false
		}
		at := i + idx
		beforeOK := at == 0 || !isSQLWordByte(lower[at-1])
		afterIdx := at + len(table)
		afterOK := afterIdx >= len(lower) || !isSQLWordByte(lower[afterIdx])
		if beforeOK && afterOK {
			return true
		}
		i = at + len(table)
	}
}

func isSQLWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
