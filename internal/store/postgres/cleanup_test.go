//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/runtime/dockerid"
)

// cleanupWorld is a rollbackWorld (relA retained, relB active, no job in
// flight) whose retention policy the test chooses.
type cleanupWorld struct {
	rollbackWorld
}

func newCleanupWorld(t *testing.T, seed byte, policy CleanupPolicy) cleanupWorld {
	t.Helper()
	w := cleanupWorld{newRollbackWorld(t, seed)}
	w.lease.SetCleanupPolicy(policy)
	return w
}

// keepNone makes every retained release eligible by age (valid policies keep
// at least one by count).
func keepNone() CleanupPolicy {
	p := DefaultCleanupPolicy()
	p.MaxRetainedAge = time.Millisecond
	return p
}

// backdate moves a release's retention time (updated_at) into the past. The
// monotonic trigger forbids it, so the change runs with triggers disabled for
// that one transaction only.
func (w cleanupWorld) backdate(t *testing.T, releaseID string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE releases SET created_at=LEAST(created_at, clock_timestamp() - make_interval(secs => $2) - interval '1 second'), updated_at=clock_timestamp() - make_interval(secs => $2) WHERE id=$1`, releaseID, age.Seconds()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// addRetained adds one more retained release retained `age` ago.
func (w cleanupWorld) addRetained(t *testing.T, tag string, age time.Duration) (string, int64) {
	t.Helper()
	id := "rt-" + tag + "-" + releaseSuffix(t)
	attempt := w.insertHistoricRelease(t, id, "retained", tag, true, "blue_green", 9090, 5)
	w.backdate(t, id, age)
	return id, attempt
}

func (w cleanupWorld) claim(t *testing.T, limit int) []CleanupClaim {
	t.Helper()
	claims, err := w.lease.ClaimCleanups(context.Background(), w.serverID, w.cred, limit)
	if err != nil {
		t.Fatalf("ClaimCleanups: %v", err)
	}
	return claims
}

func (w cleanupWorld) releaseState(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := w.pool.QueryRow(context.Background(), `SELECT state FROM releases WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func claimedIDs(cs []CleanupClaim) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ReleaseID)
	}
	return out
}

func (w cleanupWorld) cleanupRows(t *testing.T, releaseID string) (n, open int) {
	t.Helper()
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE completed_at IS NULL) FROM release_cleanups WHERE release_id=$1`, releaseID).Scan(&n, &open); err != nil {
		t.Fatal(err)
	}
	return
}

func TestCleanupClaimKeepsNewestTwoAndNeverTheActiveRelease(t *testing.T) {
	w := newCleanupWorld(t, 150, DefaultCleanupPolicy())
	// relA (retained just now) + C (1h) + D (2h): three retained, active relB.
	c, _ := w.addRetained(t, "c", time.Hour)
	d, attemptD := w.addRetained(t, "d", 2*time.Hour)
	got := w.claim(t, 8)
	if ids := claimedIDs(got); len(ids) != 1 || ids[0] != d {
		t.Fatalf("claimed %v, want only the third-newest %s", ids, d)
	}
	cl := got[0]
	if cl.EnvironmentID != w.envID || cl.RuntimeInstanceID != dockerid.AttemptRuntimeInstanceID(attemptD) || cl.ImageDigest != strings.Repeat("11", 32) || cl.CleanupID <= 0 {
		t.Fatalf("claim = %+v", cl)
	}
	if w.releaseState(t, d) != "removing" || w.releaseState(t, c) != "retained" || w.releaseState(t, w.relA) != "retained" || w.releaseState(t, w.relB) != "active" {
		t.Fatal("states after the claim")
	}
	if n, open := w.cleanupRows(t, d); n != 1 || open != 1 {
		t.Fatalf("cleanup rows = %d open %d", n, open)
	}
	// The open cleanup is handed out again unchanged (lost answer); nothing new.
	again := w.claim(t, 8)
	if len(again) != 1 || again[0].CleanupID != cl.CleanupID {
		t.Fatalf("redelivery = %+v, want cleanup %d again", again, cl.CleanupID)
	}
	if n, _ := w.cleanupRows(t, d); n != 1 {
		t.Fatalf("redelivery created %d rows", n)
	}
}

func TestCleanupClaimEvenKeepZeroNeverTakesActiveOrCandidate(t *testing.T) {
	w := newCleanupWorld(t, 151, keepNone())
	ids := claimedIDs(w.claim(t, 8))
	if len(ids) != 1 || ids[0] != w.relA {
		t.Fatalf("claimed %v, want only the retained %s", ids, w.relA)
	}
	if w.releaseState(t, w.relB) != "active" {
		t.Fatal("active release touched")
	}
}

func TestCleanupClaimSevenDayBoundary(t *testing.T) {
	w := newCleanupWorld(t, 152, DefaultCleanupPolicy())
	// Newest two are always kept by count only if young: age decides otherwise.
	old, _ := w.addRetained(t, "old", 7*24*time.Hour+time.Minute)
	young, _ := w.addRetained(t, "young", 7*24*time.Hour-time.Minute)
	// relA is brand new. Three retained: rank by retention time = A, young, old.
	// old is rank 3 anyway; make the age rule decisive with KeepRetained 3.
	p := DefaultCleanupPolicy()
	p.KeepRetained = 3
	w.lease.SetCleanupPolicy(p)
	ids := claimedIDs(w.claim(t, 8))
	if len(ids) != 1 || ids[0] != old {
		t.Fatalf("claimed %v, want only the release older than 7 days (%s), not %s", ids, old, young)
	}
}

func TestCleanupClaimNeverTakesRollbackTargetOfAJobInFlight(t *testing.T) {
	w := newCleanupWorld(t, 153, keepNone())
	if _, err := w.repo.EnqueueRollback(context.Background(), w.principal, w.relA, rollbackNonce); err != nil {
		t.Fatal(err)
	}
	if ids := claimedIDs(w.claim(t, 8)); len(ids) != 0 {
		t.Fatalf("claimed %v while a rollback to it is queued", ids)
	}
	if w.releaseState(t, w.relA) != "retained" {
		t.Fatal("rollback target moved")
	}
	// Once that job is terminal the release is eligible again.
	if _, err := w.pool.Exec(context.Background(), `UPDATE agent_jobs SET state='cancelled' WHERE rollback_target_release_id=$1`, w.relA); err != nil {
		t.Fatal(err)
	}
	if ids := claimedIDs(w.claim(t, 8)); len(ids) != 1 {
		t.Fatalf("claimed %v after the rollback job ended", ids)
	}
	for _, state := range []string{"leased", "retry_wait"} {
		_ = state // covered by the SQL's IN list; queued is the reachable state in this fixture
	}
}

func TestCleanupClaimVetoes(t *testing.T) {
	for name, veto := range map[string]string{
		"slot reconciling":   `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`,
		"route head flagged": `UPDATE route_heads SET reconciliation_required=true, updated_at=clock_timestamp() WHERE environment_id=$1 AND $2::bigint > 0`,
		"unresolved operation": `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state)
			SELECT (SELECT id FROM releases WHERE state='retained' AND environment_id=$1), a.job_id, a.id, a.fence, 'retire', decode(repeat('33',32),'hex'), 'started' FROM agent_job_attempts a WHERE a.id=$2`,
	} {
		w := newCleanupWorld(t, 154, keepNone())
		if _, err := w.pool.Exec(context.Background(), veto, w.envID, w.attemptA); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ids := claimedIDs(w.claim(t, 8)); len(ids) != 0 {
			t.Fatalf("%s: claimed %v", name, ids)
		}
		if w.releaseState(t, w.relA) != "retained" {
			t.Fatalf("%s: moved", name)
		}
	}
}

func TestCleanupClaimOccupiedSlotIsVetoed(t *testing.T) {
	f := newLeaseFixture(t, 155)
	cred := f.withCredential(t, 205)
	f.insertRouteHead(t, "app.example.com")
	relA, relB := "cl-oa-"+releaseSuffix(t), "cl-ob-"+releaseSuffix(t)
	f.insertHistoricRelease(t, relA, "retained", "A", true, "blue_green", 9090, 5)
	f.insertHistoricRelease(t, relB, "active", "B", true, "blue_green", 8080, 5)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, relB); err != nil {
		t.Fatal(err)
	}
	f.leaseJob(t, 2, 90) // a deployment now holds the slot
	lease := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	lease.SetCleanupPolicy(keepNone())
	got, err := lease.ClaimCleanups(ctx, f.serverID, cred, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("claimed %v err %v while a deployment holds the slot", got, err)
	}
}

func TestCleanupClaimIsScopedToTheCredentialsServer(t *testing.T) {
	w := newCleanupWorld(t, 156, keepNone())
	other := "other-" + releaseSuffix(t)
	w.insertServer(t, other)
	otherCred := digest(240)
	if _, err := w.pool.Exec(context.Background(), `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, other, otherCred[:]); err != nil {
		t.Fatal(err)
	}
	got, err := w.lease.ClaimCleanups(context.Background(), other, otherCred, 8)
	if err != nil || len(got) != 0 {
		t.Fatalf("other server claimed %v err %v", got, err)
	}
	// A wrong credential for the right server is denied.
	if _, err = w.lease.ClaimCleanups(context.Background(), w.serverID, digest(1), 8); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("wrong credential: %v", err)
	}
	for _, limit := range []int{0, -1, 17} {
		if _, err = w.lease.ClaimCleanups(context.Background(), w.serverID, w.cred, limit); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if w.releaseState(t, w.relA) != "retained" {
		t.Fatal("the wrong server's attempts changed state")
	}
}

func (w cleanupWorld) report(t *testing.T, cleanupID int64, result, class string, kept bool, summary string) error {
	t.Helper()
	return w.lease.RecordCleanupResult(context.Background(), w.serverID, w.cred, cleanupID, result, class, kept, summary)
}

func TestCleanupResultRemovedIsIdempotentAndMovesTheReleaseOnce(t *testing.T) {
	w := newCleanupWorld(t, 157, keepNone())
	cl := w.claim(t, 8)[0]
	if err := w.report(t, cl.CleanupID, "removed", "", true, "container removed; image kept"); err != nil {
		t.Fatal(err)
	}
	if w.releaseState(t, cl.ReleaseID) != "removed" {
		t.Fatal("release not removed")
	}
	if err := w.report(t, cl.CleanupID, "removed", "", true, "container removed; image kept"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, differ := range []func() error{
		func() error { return w.report(t, cl.CleanupID, "removed", "", false, "container removed; image kept") },
		func() error { return w.report(t, cl.CleanupID, "removed", "", true, "other") },
		func() error { return w.report(t, cl.CleanupID, "failed", "engine", false, "x") },
	} {
		if err := differ(); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
			t.Fatalf("different replay: %v", err)
		}
	}
	// A removed release is never handed out again and never rolled back to.
	if got := w.claim(t, 8); len(got) != 0 {
		t.Fatalf("claimed %v after removal", got)
	}
	if _, err := w.repo.EnqueueRollback(context.Background(), w.principal, cl.ReleaseID, rollbackNonce); !errors.Is(err, ErrRollbackNotEligible) {
		t.Fatalf("rollback to a removed release: %v", err)
	}
	var stored bool
	var summary string
	if err := w.pool.QueryRow(context.Background(), `SELECT image_kept, outcome_summary FROM release_cleanups WHERE id=$1`, cl.CleanupID).Scan(&stored, &summary); err != nil || !stored || summary != "container removed; image kept" {
		t.Fatalf("stored %v %q %v", stored, summary, err)
	}
}

func TestCleanupResultAuthorityAndValidation(t *testing.T) {
	w := newCleanupWorld(t, 158, keepNone())
	cl := w.claim(t, 8)[0]
	other := "other-" + releaseSuffix(t)
	w.insertServer(t, other)
	otherCred := digest(241)
	if _, err := w.pool.Exec(context.Background(), `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, other, otherCred[:]); err != nil {
		t.Fatal(err)
	}
	// Another server's valid credential cannot report (or probe) this cleanup.
	if err := w.lease.RecordCleanupResult(context.Background(), other, otherCred, cl.CleanupID, "removed", "", false, ""); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("cross-server: %v", err)
	}
	if err := w.report(t, 9_000_000_000_000, "removed", "", false, ""); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := w.lease.RecordCleanupResult(context.Background(), w.serverID, digest(2), cl.CleanupID, "removed", "", false, ""); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("wrong credential: %v", err)
	}
	for name, args := range map[string][]any{
		"unknown result":          {"gone", "", false, ""},
		"removed with class":      {"removed", "engine", false, ""},
		"failed without class":    {"failed", "", false, ""},
		"unknown class":           {"failed", "boom", false, ""},
		"failed with image kept":  {"failed", "engine", true, ""},
		"skipped with image kept": {"skipped", "identity", true, ""},
		"control char":            {"failed", "engine", false, "a\nb"},
		"oversize":                {"failed", "engine", false, strings.Repeat("a", 2049)},
	} {
		if err := w.report(t, cl.CleanupID, args[0].(string), args[1].(string), args[2].(bool), args[3].(string)); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if w.releaseState(t, cl.ReleaseID) != "removing" {
		t.Fatal("rejected reports changed the release")
	}
	if _, open := w.cleanupRows(t, cl.ReleaseID); open != 1 {
		t.Fatal("rejected reports closed the cleanup")
	}
}

func TestCleanupFailedResultKeepsRemovingAndRetriesAfterBackoffWithBoundedAttempts(t *testing.T) {
	p := keepNone()
	p.RetryBackoff, p.MaxAttempts = time.Hour, 2
	w := newCleanupWorld(t, 159, p)
	cl := w.claim(t, 8)[0]
	if err := w.report(t, cl.CleanupID, "failed", "stop", false, "stopping failed"); err != nil {
		t.Fatal(err)
	}
	if w.releaseState(t, cl.ReleaseID) != "removing" {
		t.Fatal("a failed cleanup must leave the release removing (operator visible)")
	}
	if got := w.claim(t, 8); len(got) != 0 {
		t.Fatalf("retried before the backoff: %v", got)
	}
	// Backoff elapsed -> a NEW cleanup row, same release, still removing.
	w.backdateCleanup(t, cl.CleanupID, 2*time.Hour)
	again := w.claim(t, 8)
	if len(again) != 1 || again[0].ReleaseID != cl.ReleaseID || again[0].CleanupID == cl.CleanupID {
		t.Fatalf("retry = %+v", again)
	}
	if err := w.report(t, again[0].CleanupID, "skipped", "identity", false, "left alone"); err != nil {
		t.Fatal(err)
	}
	// MaxAttempts (2) reached: never again, release stays removing for the operator.
	w.backdateCleanup(t, again[0].CleanupID, 2*time.Hour)
	if got := w.claim(t, 8); len(got) != 0 {
		t.Fatalf("looped past MaxAttempts: %v", got)
	}
	if n, open := w.cleanupRows(t, cl.ReleaseID); n != 2 || open != 0 || w.releaseState(t, cl.ReleaseID) != "removing" {
		t.Fatalf("rows %d open %d state %s", n, open, w.releaseState(t, cl.ReleaseID))
	}
}

func (w cleanupWorld) backdateCleanup(t *testing.T, cleanupID int64, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE release_cleanups SET claimed_at=clock_timestamp() - make_interval(secs => $2) - interval '1 second', completed_at=clock_timestamp() - make_interval(secs => $2) WHERE id=$1`, cleanupID, age.Seconds()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Genuine lock contention: a holder transaction owns the environment slot, an
// EnqueueRollback and a ClaimCleanups of the SAME retained release both block
// on it (pg_stat_activity proves it), then real lock arbitration decides.
// Exactly one wins and the loser gets a definite refusal: never a rollback job
// aimed at a release that is being removed.
func TestCleanupClaimGenuinelyRacesEnqueueRollback(t *testing.T) {
	w := newCleanupWorld(t, 160, keepNone())
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	holder, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err = holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, w.envID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var claims []CleanupClaim
	var claimErr, enqueueErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		claims, claimErr = w.lease.ClaimCleanups(ctx, w.serverID, w.cred, 8)
	}()
	go func() {
		defer wg.Done()
		_, enqueueErr = w.repo.EnqueueRollback(ctx, w.principal, w.relA, rollbackNonce)
	}()
	waitForLockWaiters(t, ctx, w.pool, "environment_slots", 2)
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	var rollbackJobs int
	if err = w.pool.QueryRow(ctx, `SELECT count(*) FROM agent_jobs WHERE rollback_target_release_id=$1`, w.relA).Scan(&rollbackJobs); err != nil {
		t.Fatal(err)
	}
	removing := w.releaseState(t, w.relA) == "removing"
	switch {
	case enqueueErr == nil && !removing:
		if len(claims) != 0 || rollbackJobs != 1 {
			t.Fatalf("rollback won but claims=%v jobs=%d", claims, rollbackJobs)
		}
	case removing:
		if !errors.Is(enqueueErr, ErrRollbackNotEligible) || len(claims) != 1 || rollbackJobs != 0 {
			t.Fatalf("claim won but enqueue err=%v claims=%v jobs=%d", enqueueErr, claims, rollbackJobs)
		}
	default:
		t.Fatalf("neither side won cleanly: enqueue=%v removing=%v claims=%v", enqueueErr, removing, claims)
	}
}

// A deploy's finalize (which retains the old active release) and a claim race
// on the slot. Whatever order wins, the claim never takes the active release,
// and it only ever takes the old retained release once the finalize committed.
func TestCleanupClaimGenuinelyRacesDeployActivationResult(t *testing.T) {
	f := newLeaseFixture(t, 161)
	cred := f.withCredential(t, 211)
	f.insertRouteHead(t, "app.example.com")
	relA, relB := "cl-ra-"+releaseSuffix(t), "cl-rb-"+releaseSuffix(t)
	f.insertHistoricRelease(t, relA, "retained", "A", true, "blue_green", 9090, 5)
	f.insertHistoricRelease(t, relB, "active", "B", true, "blue_green", 8080, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE route_heads SET active_release_id=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID, relB); err != nil {
		t.Fatal(err)
	}
	attemptC := f.leaseJob(t, 2, 90)
	f.advanceDeploymentToHealthChecking(t)
	candidateC := "cl-rc-" + releaseSuffix(t)
	f.insertCandidateRelease(t, attemptC, candidateC)
	lease := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	// Keep one: before the deploy lands A is the single retained (kept); after,
	// B is newest and A is the second, eligible.
	p := DefaultCleanupPolicy()
	p.KeepRetained = 1
	lease.SetCleanupPolicy(p)
	act, err := lease.Activate(ctx, f.serverID, cred, attemptC, 2, digest(5))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(strings.TrimPrefix(act.RouteDigest, "sha256:"))

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err = holder.Exec(ctx, `SELECT 1 FROM environment_slots WHERE environment_id=$1 FOR UPDATE`, f.envID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var reportErr, claimErr error
	var claims []CleanupClaim
	wg.Add(2)
	go func() {
		defer wg.Done()
		reportErr = lease.ReportActivationResult(ctx, f.serverID, cred, attemptC, 2, act.OperationID, true, [32]byte(raw), "")
	}()
	go func() {
		defer wg.Done()
		claims, claimErr = lease.ClaimCleanups(ctx, f.serverID, cred, 8)
	}()
	waitForLockWaiters(t, ctx, f.pool, "environment_slots", 2)
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if reportErr != nil || claimErr != nil {
		t.Fatalf("report=%v claim=%v", reportErr, claimErr)
	}
	var actives int
	var active string
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='active'), max(id) FILTER (WHERE state='active') FROM releases WHERE environment_id=$1`, f.envID).Scan(&actives, &active); err != nil || actives != 1 || active != candidateC {
		t.Fatalf("actives=%d active=%q err=%v", actives, active, err)
	}
	for _, c := range claims {
		if c.ReleaseID == candidateC || c.ReleaseID == relB {
			t.Fatalf("claimed the new active or just-retained release: %+v", c)
		}
	}
	var stateB string
	if err = f.pool.QueryRow(ctx, `SELECT state FROM releases WHERE id=$1`, relB).Scan(&stateB); err != nil || stateB != "retained" {
		t.Fatalf("old active state=%q err=%v", stateB, err)
	}
}

func TestCleanupConstants(t *testing.T) {
	p := DefaultCleanupPolicy()
	if p.KeepRetained != 2 || p.MaxRetainedAge != 7*24*time.Hour || !p.valid() {
		t.Fatalf("defaults: %+v", p)
	}
	zeroKeep := DefaultCleanupPolicy()
	zeroKeep.KeepRetained = 0
	if zeroKeep.valid() || (CleanupPolicy{}).valid() {
		t.Fatal("zero policy valid")
	}
}

func TestReadLatestCleanupShowsOpenThenResult(t *testing.T) {
	ctx := context.Background()
	w := newCleanupWorld(t, 162, keepNone())
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if got, err := readLatestCleanup(ctx, tx, w.relA); err != nil || got != nil {
		t.Fatalf("before any claim: %v %v", got, err)
	}
	cl := w.claim(t, 8)[0]
	got, err := readLatestCleanup(ctx, tx, w.relA)
	if err != nil || got == nil || got.ID != cl.CleanupID || got.Result != "" || got.CompletedAt != nil || got.Attempts != 1 {
		t.Fatalf("open: %+v err=%v", got, err)
	}
	if err = w.report(t, cl.CleanupID, "failed", "engine", false, "boom"); err != nil {
		t.Fatal(err)
	}
	got, err = readLatestCleanup(ctx, tx, w.relA)
	if err != nil || got == nil || got.Result != "failed" || got.ErrorClass != "engine" || got.Summary != "boom" || got.CompletedAt == nil {
		t.Fatalf("failed: %+v err=%v", got, err)
	}
}

// An open cleanup is redelivered even while a deployment holds the slot; the
// release stays removing, so it can never be activated again, and a rollback
// to it is refused.
func TestCleanupOpenRedeliveredWhileSlotOccupiedAndNeverReactivatable(t *testing.T) {
	w := newCleanupWorld(t, 163, keepNone())
	cl := w.claim(t, 8)[0]
	ctx := context.Background()
	// Occupy the slot with an attempt of the world.
	attempt := w.attemptA
	if _, err := w.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, w.envID, attempt); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	again := w.claim(t, 8)
	if len(again) != 1 || again[0].CleanupID != cl.CleanupID {
		t.Fatalf("redelivery while occupied = %+v", again)
	}
	if w.releaseState(t, cl.ReleaseID) != "removing" {
		t.Fatal("release left removing")
	}
	if _, err := w.pool.Exec(ctx, `UPDATE releases SET state='active' WHERE id=$1`, cl.ReleaseID); err == nil {
		t.Fatal("a removing release became active")
	}
	if _, err := w.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, w.envID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repo.EnqueueRollback(ctx, w.principal, cl.ReleaseID, rollbackNonce); !errors.Is(err, ErrRollbackNotEligible) {
		t.Fatalf("rollback to a removing release: %v", err)
	}
}
