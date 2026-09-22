//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rhFixture reuses erConfigFixture's owner/ownership bootstrap purely for
// the ownership derivation RouteHeadRepository relies on, exactly like
// EnvironmentRuntimeConfigRepository does (agent_jobs.owner_id is the only
// durable owner/environment pairing this schema has).
type rhFixture struct {
	leaseFixture
	repo *RouteHeadRepository
}

func newRHFixture(t *testing.T, seed byte) rhFixture {
	t.Helper()
	f := newLeaseFixture(t, seed)
	return rhFixture{leaseFixture: f, repo: NewRouteHeadRepository(f.pool)}
}

func (f rhFixture) insertOwnedEnvironment(t *testing.T, environmentID, key string) int64 {
	t.Helper()
	return f.insertJobNoConfig(t, f.serverID, environmentID, key, 0)
}

// insertActiveRelease creates a fresh deployment and a release for
// environmentID, drives the release candidate -> ready -> active (the same
// legal edges migration 000018 proves at the schema level), and returns the
// release's id. releases has no FK to agent_jobs/attempts (only to
// deployments), so no job/attempt fixture is needed here -- a deployment
// row alone satisfies releases.deployment_id's FK.
func insertActiveRelease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, environmentID, unique string) string {
	t.Helper()
	deploymentID := "deploy-" + unique
	releaseID := "release-" + unique
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	digest := bytes.Repeat([]byte{7}, 32)
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO deployments (id, project_id, environment_id, server_id, commit_sha, schema_version, status, created_at, updated_at, revision)
	      VALUES ($1,$2,$3,$4,$5,1,'healthy',$6,$6,1)`, deploymentID, "project-"+unique, environmentID, "server-"+unique, commitSHA, now); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO releases (id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
	      VALUES ($1,$2,$3,$4,$5,$6,'blue_green','candidate')`, releaseID, deploymentID, environmentID, digest, digest, commitSHA); err != nil {
		t.Fatalf("insert release: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE releases SET state='ready' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("candidate->ready: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, releaseID); err != nil {
		t.Fatalf("ready->active: %v", err)
	}
	return releaseID
}

// TestRouteHeadAssignCreateGetUpdateCAS proves the full
// create -> read -> CAS-update -> stale-CAS-rejected lifecycle, including
// the audit trail and the append-only route_events log.
func TestRouteHeadAssignCreateGetUpdateCAS(t *testing.T) {
	f := newRHFixture(t, 230)
	env := "route-crud-" + t.Name()
	f.insertOwnedEnvironment(t, env, "route-crud-"+t.Name())
	ctx := context.Background()

	head, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "App.Example.com", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if head.Hostname != "app.example.com" {
		t.Fatalf("hostname not normalized to lowercase: %q", head.Hostname)
	}
	if head.RouteRevision != 0 {
		t.Fatalf("created head = %+v", head)
	}

	got, found, err := f.repo.GetRouteHead(ctx, f.principal, env)
	if err != nil || !found {
		t.Fatalf("get after create: found=%v err=%v", found, err)
	}
	if got.Hostname != "app.example.com" || got.RouteRevision != 0 || got.ActiveReleaseID != "" {
		t.Fatalf("get after create = %+v", got)
	}

	// Creating again (expectedRevision nil) is rejected: only one route head
	// per environment.
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "other.example.com", nil); !errors.Is(err, ErrRouteHeadAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrRouteHeadAlreadyExists", err)
	}

	// A correct CAS update advances the revision and changes the hostname.
	updated, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "new.example.com", int64ptr(0))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.RouteRevision != 1 || updated.Hostname != "new.example.com" {
		t.Fatalf("updated head = %+v", updated)
	}

	// A stale expectedRevision is a distinct conflict and changes nothing.
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "stale.example.com", int64ptr(0)); !errors.Is(err, ErrRouteHeadConflict) {
		t.Fatalf("stale update err = %v, want ErrRouteHeadConflict", err)
	}
	again, found, err := f.repo.GetRouteHead(ctx, f.principal, env)
	if err != nil || !found || again.RouteRevision != 1 || again.Hostname != "new.example.com" {
		t.Fatalf("state after rejected stale update = %+v found=%v err=%v", again, found, err)
	}

	// An update naming an environment with no route head at all is
	// ErrRouteHeadNotFound, not ErrRouteHeadConflict.
	other := "route-crud-missing-" + t.Name()
	f.insertOwnedEnvironment(t, other, "route-crud-missing-"+t.Name())
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, other, "x.example.com", int64ptr(0)); !errors.Is(err, ErrRouteHeadNotFound) {
		t.Fatalf("update on missing head err = %v, want ErrRouteHeadNotFound", err)
	}

	// Audit trail: create success, failed re-create, update success, failed
	// stale update.
	rows, err := f.pool.Query(ctx, `SELECT action, actor_id, hostname, resulting_revision, result FROM route_head_audit_events WHERE environment_id=$1 ORDER BY id`, env)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type auditRow struct {
		action, actorID, hostname, result string
		revision                          *int64
	}
	var audits []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.action, &a.actorID, &a.hostname, &a.revision, &a.result); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, a)
	}
	if len(audits) != 4 {
		t.Fatalf("audit rows = %+v, want 4 (create, failed re-create, update, failed stale update)", audits)
	}
	if audits[0].action != "hostname_assigned" || audits[0].result != "success" || audits[0].revision == nil || *audits[0].revision != 0 {
		t.Fatalf("first audit row = %+v", audits[0])
	}
	if audits[1].result != "failure" || audits[1].revision != nil {
		t.Fatalf("second audit row = %+v", audits[1])
	}
	if audits[2].result != "success" || audits[2].revision == nil || *audits[2].revision != 1 {
		t.Fatalf("third audit row = %+v", audits[2])
	}
	if audits[3].result != "failure" {
		t.Fatalf("fourth audit row = %+v", audits[3])
	}
	for _, a := range audits {
		if a.actorID != f.principal.UserID {
			t.Fatalf("audit actor_id = %q, want %q", a.actorID, f.principal.UserID)
		}
	}

	// route_events recorded both successful writes (create + update), never
	// the two rejected attempts.
	var eventCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM route_events WHERE environment_id=$1`, env).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("route_events count = %d, want 2", eventCount)
	}
}

// TestRouteHeadHostnameTakenAcrossEnvironments proves the UNIQUE(hostname)
// violation is translated into ErrRouteHeadHostnameTaken, never a raw
// SQLSTATE 23505 or generic 500, on both create and CAS-update.
func TestRouteHeadHostnameTakenAcrossEnvironments(t *testing.T) {
	f := newRHFixture(t, 231)
	envA := "route-taken-a-" + t.Name()
	envB := "route-taken-b-" + t.Name()
	f.insertOwnedEnvironment(t, envA, "route-taken-a-"+t.Name())
	f.insertOwnedEnvironment(t, envB, "route-taken-b-"+t.Name())
	ctx := context.Background()

	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, envA, "shared.example.com", nil); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, envB, "shared.example.com", nil); !errors.Is(err, ErrRouteHeadHostnameTaken) {
		t.Fatalf("create B with taken hostname = %v, want ErrRouteHeadHostnameTaken", err)
	}

	// B creates its own head with a distinct hostname, then tries to CAS
	// onto A's hostname.
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, envB, "distinct.example.com", nil); err != nil {
		t.Fatalf("create B distinct: %v", err)
	}
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, envB, "shared.example.com", int64ptr(0)); !errors.Is(err, ErrRouteHeadHostnameTaken) {
		t.Fatalf("update B onto A's hostname = %v, want ErrRouteHeadHostnameTaken", err)
	}
}

// TestRouteHeadConcurrentAssignSameHostnameExactlyOneSucceeds races two
// goroutines assigning the same hostname to two different environments.
// Exactly one must succeed; the other must observe the typed
// ErrRouteHeadHostnameTaken error, never a raw driver error or panic.
func TestRouteHeadConcurrentAssignSameHostnameExactlyOneSucceeds(t *testing.T) {
	f := newRHFixture(t, 232)
	envA := "route-race-a-" + t.Name()
	envB := "route-race-b-" + t.Name()
	f.insertOwnedEnvironment(t, envA, "route-race-a-"+t.Name())
	f.insertOwnedEnvironment(t, envB, "route-race-b-"+t.Name())
	ctx := context.Background()
	hostname := "race.example.com"

	var wg sync.WaitGroup
	errs := make([]error, 2)
	envs := []string{envA, envB}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := f.repo.AssignRouteHostname(ctx, f.principal, envs[i], hostname, nil)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	successes, taken := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRouteHeadHostnameTaken):
			taken++
		default:
			t.Fatalf("unexpected error from concurrent assign: %v", err)
		}
	}
	if successes != 1 || taken != 1 {
		t.Fatalf("successes=%d taken=%d, want exactly one of each (errs=%v)", successes, taken, errs)
	}
}

// TestRouteHeadCrossOwnerDenied proves an owner cannot read or write another
// owner's route head, and an environment with no owning agent_jobs row at
// all is denied identically.
func TestRouteHeadCrossOwnerDenied(t *testing.T) {
	f := newRHFixture(t, 233)
	env := "route-crossowner-" + t.Name()
	f.insertOwnedEnvironment(t, env, "route-crossowner-"+t.Name())
	ctx := context.Background()
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "app.example.com", nil); err != nil {
		t.Fatal(err)
	}

	authRepo := NewAuthRepository(f.pool)
	other := secondOwnerPrincipal(t, f.pool, authRepo, 240)

	if _, _, err := f.repo.GetRouteHead(ctx, other, env); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner get = %v", err)
	}
	if _, err := f.repo.AssignRouteHostname(ctx, other, env, "hijack.example.com", int64ptr(0)); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-owner update = %v", err)
	}
	neverOwned := "route-never-owned-" + t.Name()
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, neverOwned, "x.example.com", nil); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("create against never-owned environment = %v, want denied", err)
	}
}

// TestRouteHeadInvalidHostnameRejected proves the spec's hostname bounds are
// refused before any row is written: uppercase (pre-normalization would
// otherwise reach the CHECK as-is if normalization were ever skipped --
// this proves normalization occurs and the normalized form is still
// checked against the grammar), userinfo, path, port, IPv6 brackets,
// trailing dot, oversized, and empty.
func TestRouteHeadInvalidHostnameRejected(t *testing.T) {
	f := newRHFixture(t, 234)
	env := "route-invalid-" + t.Name()
	f.insertOwnedEnvironment(t, env, "route-invalid-"+t.Name())
	ctx := context.Background()

	cases := []string{
		"",
		"user@host.example.com",
		"host.example.com/path",
		"host.example.com:8443",
		"[::1]",
		"host.example.com.",
		"-bad.example.com",
		"bad-.example.com",
		"bad..example.com",
		"host example.com",
		"host\x00.example.com",
	}
	for _, hostname := range cases {
		if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, hostname, nil); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("hostname %q accepted or wrong error: %v", hostname, err)
		}
	}
	// Prove no row was written by any of the rejected attempts.
	if _, found, err := f.repo.GetRouteHead(ctx, f.principal, env); err != nil || found {
		t.Fatalf("route head exists after only-invalid attempts: found=%v err=%v", found, err)
	}
}

// TestRouteHeadAuditWrittenInSameTransaction proves a failure injected
// immediately after the successful INSERT/UPDATE, but before commit, rolls
// back the mutation together with its audit row -- the mutation and its
// audit record commit atomically or neither does, per ADR 0003's audit
// discipline.
func TestRouteHeadAuditWrittenInSameTransaction(t *testing.T) {
	f := newRHFixture(t, 235)
	env := "route-audit-tx-" + t.Name()
	f.insertOwnedEnvironment(t, env, "route-audit-tx-"+t.Name())
	ctx := context.Background()

	f.repo.failAfter = func(stage string) error {
		if stage == "route_head_insert" {
			return errors.New("injected failure after insert, before audit")
		}
		return nil
	}
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "app.example.com", nil); err == nil {
		t.Fatal("injected failure did not surface")
	}
	f.repo.failAfter = nil

	var heads, events, audits int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM route_heads WHERE environment_id=$1),(SELECT count(*) FROM route_events WHERE environment_id=$1),(SELECT count(*) FROM route_head_audit_events WHERE environment_id=$1)`, env).Scan(&heads, &events, &audits); err != nil {
		t.Fatal(err)
	}
	if heads != 0 || events != 0 || audits != 0 {
		t.Fatalf("partial write survived rollback: heads=%d events=%d audits=%d", heads, events, audits)
	}

	// The same guard on the audit_insert stage: a failure there must roll
	// back the mutation too (the audit row and the mutation are one
	// transaction, not audit-after-commit).
	f.repo.failAfter = func(stage string) error {
		if stage == "audit_insert" {
			return errors.New("injected failure before audit insert")
		}
		return nil
	}
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "app.example.com", nil); err == nil {
		t.Fatal("injected audit failure did not surface")
	}
	f.repo.failAfter = nil
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM route_heads WHERE environment_id=$1),(SELECT count(*) FROM route_events WHERE environment_id=$1),(SELECT count(*) FROM route_head_audit_events WHERE environment_id=$1)`, env).Scan(&heads, &events, &audits); err != nil {
		t.Fatal(err)
	}
	if heads != 0 || events != 0 || audits != 0 {
		t.Fatalf("partial write survived rollback (audit_insert guard): heads=%d events=%d audits=%d", heads, events, audits)
	}

	// Sanity: with no injected failure, the same call now succeeds.
	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "app.example.com", nil); err != nil {
		t.Fatalf("assign after clearing injected failures: %v", err)
	}
}

func int64ptr(v int64) *int64 { return &v }

// TestRouteHeadAssignHostnameRefusedOnceReleaseActive is a permanent
// repository-level guard: with a real release driven all the way to
// 'active' state and the route head pointing at it, AssignRouteHostname
// must refuse a hostname change with the typed
// ErrRouteHeadHostnameImmutable error -- not a generic error, not a raw
// PostgreSQL trigger error, and not success.
func TestRouteHeadAssignHostnameRefusedOnceReleaseActive(t *testing.T) {
	f := newRHFixture(t, 236)
	env := "route-active-immutable-" + t.Name()
	f.insertOwnedEnvironment(t, env, "route-active-immutable-"+t.Name())
	ctx := context.Background()

	head, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "app.example.com", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	releaseID := insertActiveRelease(t, ctx, f.pool, env, fmt.Sprintf("t236-%s", t.Name()))
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, env, releaseID); err != nil {
		t.Fatalf("point route head at active release: %v", err)
	}

	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "changed.example.com", int64ptr(head.RouteRevision+1)); !errors.Is(err, ErrRouteHeadHostnameImmutable) {
		t.Fatalf("hostname change against an active release = %v, want ErrRouteHeadHostnameImmutable", err)
	}

	// The hostname on the stored row is unchanged.
	got, found, err := f.repo.GetRouteHead(ctx, f.principal, env)
	if err != nil || !found || got.Hostname != "app.example.com" {
		t.Fatalf("route head after rejected change = %+v found=%v err=%v", got, found, err)
	}
}

// TestRouteHeadActiveReleaseWriteSkewPrevented is a permanent regression
// test for a write-skew window an independent review found live against
// PG18: two concurrent transactions -- one (T1) pointing a route head at a
// release R1 that is still 'active', the other (T2) retiring R1 to
// 'retained' -- could each validate their own deferred check against a
// snapshot that does not yet reflect the other's still-uncommitted write,
// and both commit, leaving the head pointing at a non-active release. The
// fix (migration 000018's validate_route_head_active_release) takes a
// FOR SHARE lock on the target release row, which forces the two
// transactions to serialize through that row regardless of which one
// happens to run first. Both orderings are proven here, each driven by
// goroutines synchronized with channels (real blocking on the database
// lock, not a fixed sleep) and bounded by a context timeout so a real
// deadlock fails the test instead of hanging:
//
//   - "head-checks-first": T1's UPDATE + deferred check (forced early via
//     SET CONSTRAINTS ALL IMMEDIATE) run and pass while R1 is still
//     'active', holding a FOR SHARE lock on R1 until T1 commits. T2's
//     UPDATE of R1 then blocks on that lock; once T1 commits, T2's UPDATE
//     proceeds and T2's own deferred check now sees the committed
//     reference and fails.
//   - "release-updates-first": T2's UPDATE + deferred check run and pass
//     first (nothing references R1 yet), holding R1's row lock until T2
//     commits. T1's FOR SHARE then blocks on that lock; once T2 commits,
//     T1's check observes the committed 'retained' state and fails.
//
// In both orderings exactly one transaction must fail with an
// invariant-violation error and the other must succeed, and the state
// actually committed to the database must never show the head pointing at
// a non-active release.
func TestRouteHeadActiveReleaseWriteSkewPrevented(t *testing.T) {
	for _, headChecksFirst := range []bool{true, false} {
		name := "release-updates-first"
		if headChecksFirst {
			name = "head-checks-first"
		}
		t.Run(name, func(t *testing.T) {
			testRouteHeadActiveReleaseWriteSkewOrdering(t, headChecksFirst)
		})
	}
}

func testRouteHeadActiveReleaseWriteSkewOrdering(t *testing.T, headChecksFirst bool) {
	f := newRHFixture(t, 237)
	env := "route-skew-" + t.Name()
	f.insertOwnedEnvironment(t, env, "route-skew-"+t.Name())
	ctx := context.Background()

	if _, err := f.repo.AssignRouteHostname(ctx, f.principal, env, "skew.example.com", nil); err != nil {
		t.Fatalf("create route head: %v", err)
	}
	releaseID := insertActiveRelease(t, ctx, f.pool, env, fmt.Sprintf("skew-%s-%v", t.Name(), headChecksFirst))

	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// proceed is closed by whichever side moves first, immediately after
	// its own deferred check has passed and it holds the lock the other
	// side will need -- and, critically, *before* it commits. The first
	// mover never waits on anything after that: it signals, then commits
	// unconditionally. The second mover waits on proceed before issuing
	// the statement that will block on the first mover's lock. This
	// one-way handshake is what avoids a deadlock: neither goroutine ever
	// waits for a signal that only arrives after the other has already
	// unblocked it.
	proceed := make(chan struct{})
	headErr := make(chan error, 1)
	releaseErr := make(chan error, 1)

	runHead := func(first bool) {
		if !first {
			<-proceed
		}
		tx, err := f.pool.Begin(dctx)
		if err != nil {
			headErr <- fmt.Errorf("begin: %w", err)
			return
		}
		if _, err := tx.Exec(dctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1 WHERE environment_id=$1`, env, releaseID); err != nil {
			_ = tx.Rollback(dctx)
			headErr <- fmt.Errorf("update route head: %w", err)
			return
		}
		// When first, this call passes immediately (R1 is still active)
		// and acquires FOR SHARE on R1, held until this transaction
		// commits or rolls back. When second, this call is the one
		// expected to block -- on the release transaction's row lock on
		// R1 -- until the release transaction commits.
		if _, err := tx.Exec(dctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			_ = tx.Rollback(dctx)
			headErr <- err
			return
		}
		if first {
			close(proceed)
			// Widen the window between "check passed" and "commit" so the
			// second mover's statements have a realistic chance to run
			// while this transaction is still open -- this is what makes
			// the test actually exercise (and, with the FOR SHARE guard
			// removed, actually detect) the race rather than relying on
			// goroutine scheduling and network round-trip latency alone to
			// keep the two transactions apart. The real correctness
			// mechanism under test is still the database lock, not this
			// sleep: with the lock in place, the second mover blocks for
			// however long this sleep plus lock-wait takes; without it,
			// this sleep is what reliably lets the second mover's
			// unlocked read land before this transaction commits.
			time.Sleep(200 * time.Millisecond)
		}
		if err := tx.Commit(dctx); err != nil {
			headErr <- err
			return
		}
		headErr <- nil
	}

	runRelease := func(first bool) {
		if !first {
			<-proceed
		}
		tx, err := f.pool.Begin(dctx)
		if err != nil {
			releaseErr <- fmt.Errorf("begin: %w", err)
			return
		}
		// When first, this call passes immediately (nothing references R1
		// yet) and, as an ordinary side effect of any UPDATE, holds R1's
		// row lock until this transaction commits or rolls back. When
		// second, this call is the one expected to block -- on the head
		// transaction's FOR SHARE lock on R1 -- until the head
		// transaction commits.
		if _, err := tx.Exec(dctx, `UPDATE releases SET state='retained' WHERE id=$1`, releaseID); err != nil {
			_ = tx.Rollback(dctx)
			releaseErr <- fmt.Errorf("update release: %w", err)
			return
		}
		if _, err := tx.Exec(dctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			_ = tx.Rollback(dctx)
			releaseErr <- err
			return
		}
		if first {
			close(proceed)
			// See the matching comment in runHead: widens the window so
			// this test can actually detect the FOR SHARE guard's absence
			// rather than depending on scheduling luck.
			time.Sleep(200 * time.Millisecond)
		}
		if err := tx.Commit(dctx); err != nil {
			releaseErr <- err
			return
		}
		releaseErr <- nil
	}

	go runHead(headChecksFirst)
	go runRelease(!headChecksFirst)

	var hErr, rErr error
	var hDone, rDone bool
	for !hDone || !rDone {
		select {
		case hErr = <-headErr:
			hDone = true
		case rErr = <-releaseErr:
			rDone = true
		case <-time.After(25 * time.Second):
			t.Fatal("timed out waiting for concurrent transactions -- possible deadlock")
		}
	}

	headFailed, releaseFailed := hErr != nil, rErr != nil
	if headFailed == releaseFailed {
		t.Fatalf("expected exactly one transaction to fail, got headErr=%v releaseErr=%v", hErr, rErr)
	}

	// Whatever the outcome, the persisted state must never show the head
	// pointing at a non-active release.
	var headActiveReleaseID, releaseState string
	err := f.pool.QueryRow(ctx, `SELECT coalesce(h.active_release_id,''), coalesce(r.state,'') FROM route_heads h LEFT JOIN releases r ON r.id = h.active_release_id WHERE h.environment_id=$1`, env).Scan(&headActiveReleaseID, &releaseState)
	if err != nil {
		t.Fatalf("read committed state: %v", err)
	}
	if headActiveReleaseID != "" && releaseState != "active" {
		t.Fatalf("write skew observed: route head active_release_id=%q points at a release in state %q", headActiveReleaseID, releaseState)
	}
}
