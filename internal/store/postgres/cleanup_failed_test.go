//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/runtime/dockerid"
)

// Step 21B-1: failed-candidate cleanup eligibility, claim payload and result.

type failedSpec struct {
	state    string            // release state: candidate (default) or ready
	digest   string            // one hex byte repeated 32 times; "" = no image digest
	status   deployment.Status // deployment status (default failed)
	age      time.Duration     // how long ago the deployment became terminal (default 25h)
	jobState string            // the deployment job's state (default failed)
	attempts int               // attempts of the job (default 1)
	serverID string            // server that ran the job (default the world's)
}

// forceSQL runs statements that bypass the state-machine triggers (fixture
// setup and veto injection only).
func (w cleanupWorld) forceSQL(t *testing.T, sql string, args ...any) {
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
	if _, err = tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("forceSQL: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// addFailedCandidate creates a never-activated release of a terminal
// deployment with sp.attempts closed attempts and no release operation. It
// returns the release id and the attempt ids, oldest first.
func (w cleanupWorld) addFailedCandidate(t *testing.T, tag string, sp failedSpec) (string, []int64) {
	t.Helper()
	ctx := context.Background()
	if sp.state == "" {
		sp.state = "candidate"
	}
	if sp.status == "" {
		sp.status = deployment.StatusFailed
	}
	if sp.age == 0 {
		sp.age = 25 * time.Hour
	}
	if sp.jobState == "" {
		sp.jobState = "failed"
	}
	if sp.attempts == 0 {
		sp.attempts = 1
	}
	if sp.serverID == "" {
		sp.serverID = w.serverID
	}
	id := "fc-" + tag + "-" + releaseSuffix(t)
	depID := "dep-" + id
	w.insertPlainDeployment(t, depID)
	if sp.status == deployment.StatusSuperseded { // only legal from queued
		w.walkDeployment(t, depID, sp.status)
	} else {
		w.walkDeployment(t, depID, deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding)
		if sp.status != deployment.StatusBuilding {
			w.walkDeployment(t, depID, sp.status)
		}
	}
	digest := "NULL"
	if sp.digest != "" {
		digest = "decode(repeat('" + sp.digest + "',32),'hex')"
	}
	w.forceSQL(t, `INSERT INTO releases(id, deployment_id, environment_id, image_digest, config_digest, commit_sha, strategy, state)
		VALUES ($1,$2,$3,`+digest+`,decode(repeat('22',32),'hex'),'abcdefabcdefabcdefabcdefabcdefabcdefabcd','blue_green',$4)`, id, depID, w.envID, sp.state)
	var jobID int64
	if err := w.pool.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state,attempt_count)
		VALUES (1,'deployment_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','abcdefabcdefabcdefabcdefabcdefabcdefabcd',0,clock_timestamp(),$5,decode(repeat('00',32),'hex'),$6,$7) RETURNING id`,
		w.ownerID, w.envID, sp.serverID, depID, "fc-key-"+id, sp.jobState, sp.attempts).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	var attemptIDs []int64
	for n := 1; n <= sp.attempts; n++ {
		var aid int64
		w.forceSQL(t, `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at, failure_class, result_operation_id, result_digest, closed_at)
			SELECT $1,$2,$3,$4,1,'failed',t.at,t.at,'transient','fc-result',decode(repeat('00',32),'hex'),t.at FROM (SELECT clock_timestamp() AS at) t`, jobID, n, sp.serverID, w.envID)
		if err := w.pool.QueryRow(ctx, `SELECT id FROM agent_job_attempts WHERE job_id=$1 AND attempt_number=$2`, jobID, n).Scan(&aid); err != nil {
			t.Fatal(err)
		}
		attemptIDs = append(attemptIDs, aid)
	}
	w.forceSQL(t, `UPDATE deployments SET created_at=LEAST(created_at, clock_timestamp() - make_interval(secs => $2) - interval '1 second'), updated_at=clock_timestamp() - make_interval(secs => $2) WHERE id=$1`, depID, sp.age.Seconds())
	return id, attemptIDs
}

func (w cleanupWorld) claimKinds(t *testing.T, limit int) []CleanupClaim {
	t.Helper()
	claims, err := w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, w.cred, limit, true)
	if err != nil {
		t.Fatalf("ClaimCleanupsWithKinds: %v", err)
	}
	return claims
}

func wantInstanceIDs(attempts []int64) []string {
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, dockerid.AttemptRuntimeInstanceID(a))
	}
	return out
}

func TestFailedCandidateClaimedWithoutDigestAndPerAttemptInstances(t *testing.T) {
	w := newCleanupWorld(t, 160, DefaultCleanupPolicy())
	rel, attempts := w.addFailedCandidate(t, "a", failedSpec{attempts: 2})
	got := w.claimKinds(t, 8)
	if len(got) != 1 || got[0].ReleaseID != rel || got[0].Kind != CleanupKindFailedCandidate || got[0].ImageDigest != "" || got[0].RuntimeInstanceID != "" || got[0].EnvironmentID != w.envID {
		t.Fatalf("claim = %+v", got)
	}
	if want := wantInstanceIDs(attempts); strings.Join(got[0].RuntimeInstanceIDs, ",") != strings.Join(want, ",") || len(want) != 2 {
		t.Fatalf("instance ids %v, want %v", got[0].RuntimeInstanceIDs, want)
	}
	if w.releaseState(t, rel) != "removing" {
		t.Fatalf("state = %s", w.releaseState(t, rel))
	}
	if n, open := w.cleanupRows(t, rel); n != 1 || open != 1 {
		t.Fatalf("rows %d open %d", n, open)
	}
	// A lost answer: the open cleanup is handed out again unchanged, no new row.
	again := w.claimKinds(t, 8)
	if len(again) != 1 || again[0].CleanupID != got[0].CleanupID || again[0].Kind != CleanupKindFailedCandidate ||
		strings.Join(again[0].RuntimeInstanceIDs, ",") != strings.Join(got[0].RuntimeInstanceIDs, ",") {
		t.Fatalf("redelivery = %+v", again)
	}
	if n, _ := w.cleanupRows(t, rel); n != 1 {
		t.Fatalf("rows after redelivery = %d", n)
	}
	// The retained release of the world was never touched.
	if w.releaseState(t, w.relA) != "retained" || w.releaseState(t, w.relB) != "active" {
		t.Fatal("other releases changed")
	}
}

func TestFailedCandidateReadyWithUniqueDigestHandsTheDigest(t *testing.T) {
	w := newCleanupWorld(t, 161, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "u", failedSpec{state: "ready", digest: "55"})
	got := w.claimKinds(t, 8)
	if len(got) != 1 || got[0].ReleaseID != rel || got[0].ImageDigest != strings.Repeat("55", 32) {
		t.Fatalf("claim = %+v", got)
	}
}

func TestFailedCandidateSharingItsDigestKeepsTheImage(t *testing.T) {
	// "11" is the digest of the world's retained and active releases.
	w := newCleanupWorld(t, 162, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "s", failedSpec{state: "ready", digest: "11"})
	got := w.claimKinds(t, 8)
	if len(got) != 1 || got[0].ReleaseID != rel || got[0].ImageDigest != "" {
		t.Fatalf("shared digest must be withheld: %+v", got)
	}
}

func TestFailedCandidateDigestSharedOnlyWithRemovedReleaseIsHanded(t *testing.T) {
	w := newCleanupWorld(t, 163, DefaultCleanupPolicy())
	w.forceSQL(t, `UPDATE releases SET state='removed' WHERE id=$1`, w.relA)
	// relB (active) still shares "11": use a digest only relA had.
	w.forceSQL(t, `UPDATE releases SET image_digest=decode(repeat('66',32),'hex') WHERE id=$1`, w.relA)
	rel, _ := w.addFailedCandidate(t, "r", failedSpec{state: "ready", digest: "66"})
	got := w.claimKinds(t, 8)
	if len(got) != 1 || got[0].ReleaseID != rel || got[0].ImageDigest != strings.Repeat("66", 32) {
		t.Fatalf("claim = %+v", got)
	}
}

func TestFailedCandidateSharingDigestWithAnotherFailedCandidateKeepsTheImage(t *testing.T) {
	w := newCleanupWorld(t, 164, DefaultCleanupPolicy())
	w.addFailedCandidate(t, "x", failedSpec{state: "ready", digest: "77"})
	w.addFailedCandidate(t, "y", failedSpec{state: "ready", digest: "77"})
	got := w.claimKinds(t, 8)
	if len(got) != 2 {
		t.Fatalf("claims = %+v", got)
	}
	for _, c := range got {
		if c.ImageDigest != "" {
			t.Fatalf("a digest held by two non-removed releases must not be handed: %+v", c)
		}
	}
}

func TestFailedCandidateCancelledDeploymentIsEligible(t *testing.T) {
	w := newCleanupWorld(t, 165, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "c", failedSpec{status: deployment.StatusCancelled})
	if got := w.claimKinds(t, 8); len(got) != 1 || got[0].ReleaseID != rel {
		t.Fatalf("claim = %+v", got)
	}
}

func TestFailedCandidateAgeBoundary(t *testing.T) {
	w := newCleanupWorld(t, 166, DefaultCleanupPolicy())
	w.addFailedCandidate(t, "y", failedSpec{age: 24*time.Hour - time.Minute})
	if got := w.claimKinds(t, 8); len(got) != 0 {
		t.Fatalf("claimed before 24h: %+v", got)
	}
	old, _ := w.addFailedCandidate(t, "o", failedSpec{age: 24*time.Hour + time.Minute})
	if got := w.claimKinds(t, 8); len(got) != 1 || got[0].ReleaseID != old {
		t.Fatalf("claim = %+v", got)
	}
}

// Every exclusion on its own: an otherwise eligible candidate that is vetoed
// is never claimed, never moved, and gets no cleanup row.
func TestFailedCandidateExclusions(t *testing.T) {
	type tc struct {
		name  string
		spec  failedSpec
		setup func(t *testing.T, w cleanupWorld, rel string, attempts []int64)
	}
	cases := []tc{
		{name: "younger than 24h", spec: failedSpec{age: 23 * time.Hour}},
		{name: "job queued", setup: func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE agent_jobs SET state='queued' WHERE deployment_id=(SELECT deployment_id FROM releases WHERE id=$1)`, rel)
		}},
		{name: "job leased", setup: func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE agent_jobs SET state='leased' WHERE deployment_id=(SELECT deployment_id FROM releases WHERE id=$1)`, rel)
		}},
		{name: "job retry_wait", setup: func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE agent_jobs SET state='retry_wait' WHERE deployment_id=(SELECT deployment_id FROM releases WHERE id=$1)`, rel)
		}},
		{name: "open release operation", setup: func(t *testing.T, w cleanupWorld, rel string, attempts []int64) {
			w.forceSQL(t, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state)
				SELECT $1, a.job_id, a.id, a.fence, 'prepare', decode(repeat('33',32),'hex'), 'started' FROM agent_job_attempts a WHERE a.id=$2`, rel, attempts[0])
		}},
		{name: "ever activated", setup: func(t *testing.T, w cleanupWorld, rel string, attempts []int64) {
			w.forceSQL(t, `INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, created_at, closed_at, environment_id, expected_route_revision, route_digest)
				SELECT $1, a.job_id, a.id, a.fence, 'activate', decode(repeat('33',32),'hex'), 'succeeded', clock_timestamp() - interval '1 second', clock_timestamp(), a.environment_id, 0, decode(repeat('44',32),'hex') FROM agent_job_attempts a WHERE a.id=$2`, rel, attempts[0])
		}},
		{name: "route head active release", setup: func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE route_heads SET active_release_id=$2 WHERE environment_id=$1`, w.envID, rel)
		}},
		{name: "slot busy", setup: func(t *testing.T, w cleanupWorld, _ string, attempts []int64) {
			w.forceSQL(t, `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, w.envID, attempts[0])
		}},
		{name: "slot reconciling", setup: func(t *testing.T, w cleanupWorld, _ string, attempts []int64) {
			w.forceSQL(t, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, w.envID, attempts[0])
		}},
		{name: "route head flagged", setup: func(t *testing.T, w cleanupWorld, _ string, _ []int64) {
			w.forceSQL(t, `UPDATE route_heads SET reconciliation_required=true WHERE environment_id=$1`, w.envID)
		}},
		{name: "deployment not terminal", spec: failedSpec{status: deployment.StatusBuilding}},
		{name: "release failed (terminal)", setup: func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE releases SET state='failed' WHERE id=$1`, rel)
		}},
		{name: "release removed", setup: func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE releases SET state='removed' WHERE id=$1`, rel)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newCleanupWorld(t, 167, DefaultCleanupPolicy())
			rel, attempts := w.addFailedCandidate(t, "e", c.spec)
			before := w.releaseState(t, rel)
			if c.setup != nil {
				c.setup(t, w, rel, attempts)
				before = w.releaseState(t, rel)
			}
			if got := w.claimKinds(t, 8); len(got) != 0 {
				t.Fatalf("claimed %+v", got)
			}
			if w.releaseState(t, rel) != before {
				t.Fatalf("state moved %s -> %s", before, w.releaseState(t, rel))
			}
			if n, _ := w.cleanupRows(t, rel); n != 0 {
				t.Fatalf("cleanup rows = %d", n)
			}
		})
	}
}

func TestFailedCandidateIsScopedToTheServerThatRanIt(t *testing.T) {
	w := newCleanupWorld(t, 168, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "m", failedSpec{})
	other := "other-" + releaseSuffix(t)
	w.insertServer(t, other)
	otherCred := digest(241)
	if _, err := w.pool.Exec(context.Background(), `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, other, otherCred[:]); err != nil {
		t.Fatal(err)
	}
	// The other server has a failed candidate of its own in the same
	// environment, so the environment is in its claim; only that one is its.
	otherRel, _ := w.addFailedCandidate(t, "o", failedSpec{serverID: other})
	got, err := w.lease.ClaimCleanupsWithKinds(context.Background(), other, otherCred, 8, true)
	if err != nil || len(got) != 1 || got[0].ReleaseID != otherRel {
		t.Fatalf("other server claimed %+v err %v", got, err)
	}
	if w.releaseState(t, rel) != "candidate" {
		t.Fatal("another server's claim moved the release")
	}
	if own := w.claimKinds(t, 8); len(own) != 1 || own[0].ReleaseID != rel {
		t.Fatalf("own claim = %+v", own)
	}
}

func TestFailedCandidateWithoutOptInIsInvisibleAndTodaysBehaviourStays(t *testing.T) {
	w := newCleanupWorld(t, 169, keepNone())
	rel, _ := w.addFailedCandidate(t, "n", failedSpec{state: "ready", digest: "55"})
	got := w.claim(t, 8) // the 20E entry point
	if len(got) != 1 || got[0].ReleaseID != w.relA || got[0].Kind != CleanupKindRetained || len(got[0].RuntimeInstanceIDs) != 0 {
		t.Fatalf("non-opted-in claim = %+v", got)
	}
	if want := dockerid.AttemptRuntimeInstanceID(w.attemptA); got[0].RuntimeInstanceID != want || got[0].ImageDigest != strings.Repeat("11", 32) {
		t.Fatalf("retained item changed: %+v", got[0])
	}
	if w.releaseState(t, rel) != "ready" {
		t.Fatal("failed candidate moved without an opt-in")
	}
	// Opt-in with an explicit false is the same code path.
	off, err := w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, w.cred, 8, false)
	if err != nil || len(off) != 1 || off[0].ReleaseID != w.relA || off[0].CleanupID != got[0].CleanupID {
		t.Fatalf("includeFailedCandidates=false: %+v err %v", off, err)
	}
	// Opted in: the open retained cleanup is redelivered with the failed candidate added.
	on := w.claimKinds(t, 8)
	kinds := map[string]string{}
	for _, c := range on {
		kinds[c.ReleaseID] = c.Kind
	}
	if len(on) != 2 || kinds[w.relA] != CleanupKindRetained || kinds[rel] != CleanupKindFailedCandidate {
		t.Fatalf("opted-in claim = %+v", on)
	}
	// An old agent now never sees the open failed-candidate cleanup.
	off, err = w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, w.cred, 8, false)
	if err != nil || len(off) != 1 || off[0].ReleaseID != w.relA {
		t.Fatalf("old agent saw %+v err %v", off, err)
	}
}

func TestFailedCandidateResultRemovedIsIdempotentAndMovesTheReleaseOnce(t *testing.T) {
	w := newCleanupWorld(t, 170, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "r", failedSpec{state: "ready", digest: "55"})
	cl := w.claimKinds(t, 8)[0]
	if err := w.report(t, cl.CleanupID, "removed", "", false, "removed 1 container"); err != nil {
		t.Fatal(err)
	}
	if w.releaseState(t, rel) != "removed" {
		t.Fatalf("state = %s", w.releaseState(t, rel))
	}
	if err := w.report(t, cl.CleanupID, "removed", "", false, "removed 1 container"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := w.report(t, cl.CleanupID, "removed", "", true, "removed 1 container"); !errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("different payload: %v", err)
	}
	if got := w.claimKinds(t, 8); len(got) != 0 {
		t.Fatalf("a removed release was handed out again: %+v", got)
	}
	if n, open := w.cleanupRows(t, rel); n != 1 || open != 0 {
		t.Fatalf("rows %d open %d", n, open)
	}
}

func TestFailedCandidateFailedResultRetriesAfterBackoffAsFailedCandidate(t *testing.T) {
	p := DefaultCleanupPolicy()
	p.RetryBackoff, p.MaxAttempts = time.Hour, 2
	w := newCleanupWorld(t, 171, p)
	rel, attempts := w.addFailedCandidate(t, "t", failedSpec{attempts: 2})
	cl := w.claimKinds(t, 8)[0]
	if err := w.report(t, cl.CleanupID, "failed", "stop", false, "stop failed"); err != nil {
		t.Fatal(err)
	}
	if w.releaseState(t, rel) != "removing" {
		t.Fatal("a failed cleanup must leave the release removing")
	}
	if got := w.claimKinds(t, 8); len(got) != 0 {
		t.Fatalf("retried before the backoff: %+v", got)
	}
	w.backdateCleanup(t, cl.CleanupID, 2*time.Hour)
	if off, err := w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, w.cred, 8, false); err != nil || len(off) != 0 {
		t.Fatalf("old agent got the retry: %+v err %v", off, err)
	}
	again := w.claimKinds(t, 8)
	if len(again) != 1 || again[0].ReleaseID != rel || again[0].CleanupID == cl.CleanupID || again[0].Kind != CleanupKindFailedCandidate ||
		strings.Join(again[0].RuntimeInstanceIDs, ",") != strings.Join(wantInstanceIDs(attempts), ",") {
		t.Fatalf("retry = %+v", again)
	}
	if err := w.report(t, again[0].CleanupID, "removed", "", false, "ok"); err != nil {
		t.Fatal(err)
	}
	if w.releaseState(t, rel) != "removed" {
		t.Fatalf("state = %s", w.releaseState(t, rel))
	}
}

func TestFailedCandidateOpenCleanupSurvivesLaterVetoes(t *testing.T) {
	// Like retained: an open cleanup claimed while the environment was quiet is
	// redelivered even when the slot is occupied afterwards (removal is idempotent).
	w := newCleanupWorld(t, 172, DefaultCleanupPolicy())
	rel, attempts := w.addFailedCandidate(t, "v", failedSpec{})
	first := w.claimKinds(t, 8)
	if len(first) != 1 {
		t.Fatalf("claim = %+v", first)
	}
	w.forceSQL(t, `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, w.envID, attempts[0])
	again := w.claimKinds(t, 8)
	if len(again) != 1 || again[0].CleanupID != first[0].CleanupID || again[0].ReleaseID != rel {
		t.Fatalf("redelivery = %+v", again)
	}
}

func TestFailedCandidateClaimLimitAndInvalidInput(t *testing.T) {
	w := newCleanupWorld(t, 173, DefaultCleanupPolicy())
	for _, tag := range []string{"p", "q", "r"} {
		w.addFailedCandidate(t, tag, failedSpec{})
	}
	if got := w.claimKinds(t, 2); len(got) != 2 {
		t.Fatalf("limit 2 claimed %d", len(got))
	}
	for _, limit := range []int{0, -1, 17} {
		if _, err := w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, w.cred, limit, true); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if _, err := w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, digest(1), 8, true); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("wrong credential: %v", err)
	}
}

func TestCleanupPolicyHasTheFailedCandidateFloor(t *testing.T) {
	p := DefaultCleanupPolicy()
	if p.FailedCandidateAge != 24*time.Hour || !p.valid() {
		t.Fatalf("defaults: %+v", p)
	}
	p.FailedCandidateAge = 0
	if p.valid() {
		t.Fatal("zero failed candidate age must be invalid: it would make every failed candidate eligible at once")
	}
}

func TestFailedCandidateSupersededDeploymentIsEligible(t *testing.T) {
	w := newCleanupWorld(t, 174, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "sp", failedSpec{status: deployment.StatusSuperseded})
	if got := w.claimKinds(t, 8); len(got) != 1 || got[0].ReleaseID != rel || got[0].Kind != CleanupKindFailedCandidate {
		t.Fatalf("claim = %+v", got)
	}
}

// The environment has no retained release and no cleanup row, so only the
// failed-candidate branch of the environment peek can find it.
func TestFailedCandidateEnvironmentFoundByItsOwnPeekBranch(t *testing.T) {
	w := newCleanupWorld(t, 175, DefaultCleanupPolicy())
	w.forceSQL(t, `UPDATE releases SET state='removed' WHERE id=$1`, w.relA)
	rel, _ := w.addFailedCandidate(t, "pk", failedSpec{})
	if got := w.claimKinds(t, 8); len(got) != 1 || got[0].ReleaseID != rel {
		t.Fatalf("claim = %+v", got)
	}
}

// Retained-origin and failed-candidate cleanups are disjoint under opt-in: a
// retained release in retry state is a retained item with its winning instance.
func TestFailedCandidateOptInRetryOfRetainedStaysRetained(t *testing.T) {
	p := keepNone()
	p.RetryBackoff = time.Hour
	w := newCleanupWorld(t, 176, p)
	first := w.claim(t, 8)
	if len(first) != 1 || first[0].ReleaseID != w.relA {
		t.Fatalf("first = %+v", first)
	}
	if err := w.report(t, first[0].CleanupID, "failed", "stop", false, "x"); err != nil {
		t.Fatal(err)
	}
	w.backdateCleanup(t, first[0].CleanupID, 2*time.Hour)
	got := w.claimKinds(t, 8)
	if len(got) != 1 || got[0].ReleaseID != w.relA || got[0].CleanupID == first[0].CleanupID || got[0].Kind != CleanupKindRetained ||
		got[0].RuntimeInstanceID != dockerid.AttemptRuntimeInstanceID(w.attemptA) || len(got[0].RuntimeInstanceIDs) != 0 || got[0].ImageDigest != strings.Repeat("11", 32) {
		t.Fatalf("retry of a retained release = %+v", got)
	}
}

// Without the opt-in, neither an open nor a retry-state failed-candidate
// cleanup is ever handed out.
func TestFailedCandidateOpenAndRetryNeverReachAnOldAgent(t *testing.T) {
	p := DefaultCleanupPolicy()
	p.RetryBackoff = time.Hour
	w := newCleanupWorld(t, 177, p)
	retryRel, _ := w.addFailedCandidate(t, "rt", failedSpec{state: "ready", digest: "55"})
	first := w.claimKinds(t, 8)
	if len(first) != 1 || first[0].ReleaseID != retryRel {
		t.Fatalf("first = %+v", first)
	}
	if err := w.report(t, first[0].CleanupID, "failed", "stop", false, "x"); err != nil {
		t.Fatal(err)
	}
	w.backdateCleanup(t, first[0].CleanupID, 2*time.Hour)
	old := func(when string) {
		t.Helper()
		if got, err := w.lease.ClaimCleanupsWithKinds(context.Background(), w.serverID, w.cred, 8, false); err != nil || len(got) != 0 {
			t.Fatalf("%s: old agent saw %+v err %v", when, got, err)
		}
	}
	old("retry state")
	openRel, _ := w.addFailedCandidate(t, "op", failedSpec{state: "ready", digest: "56"})
	if got := w.claimKinds(t, 8); len(got) != 2 {
		t.Fatalf("opted-in claim = %+v", got) // the retry row and the new row, both now open
	}
	if n, open := w.cleanupRows(t, openRel); n != 1 || open != 1 {
		t.Fatalf("open rel rows %d open %d", n, open)
	}
	if n, open := w.cleanupRows(t, retryRel); n != 2 || open != 1 {
		t.Fatalf("retry rel rows %d open %d", n, open)
	}
	old("open state")
}

// A succeeded activate operation by a ROLLBACK job still excludes the release
// (and, forced into an open cleanup, the claim-time re-check skips it).
func TestFailedCandidateRollbackJobActivationExcludes(t *testing.T) {
	w := newCleanupWorld(t, 178, DefaultCleanupPolicy())
	rel, _ := w.addFailedCandidate(t, "rb", failedSpec{})
	w.addRollbackJobActivation(t, rel)
	if got := w.claimKinds(t, 8); len(got) != 0 {
		t.Fatalf("claimed %+v", got)
	}
	if w.releaseState(t, rel) != "candidate" {
		t.Fatal("state moved")
	}
}

func (w cleanupWorld) addRollbackJobActivation(t *testing.T, releaseID string) {
	t.Helper()
	depID := "dep-rbj-" + releaseID
	w.insertPlainDeployment(t, depID)
	w.forceSQL(t, `WITH j AS (
		INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state,attempt_count,rollback_target_release_id)
		VALUES (1,'rollback_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','abcdefabcdefabcdefabcdefabcdefabcdefabcd',0,clock_timestamp(),$5,decode(repeat('00',32),'hex'),'succeeded',1,$6) RETURNING id),
	a AS (
		INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at, failure_class, result_operation_id, result_digest, closed_at)
		SELECT j.id,1,$3,$2,1,'succeeded',t.at,t.at,NULL,'rb-result',decode(repeat('00',32),'hex'),t.at FROM j, (SELECT clock_timestamp() AS at) t RETURNING id, job_id)
	INSERT INTO release_operations(release_id, job_id, attempt_id, fence, kind, digest, state, created_at, closed_at, environment_id, expected_route_revision, route_digest)
		SELECT $7, a.job_id, a.id, 1, 'activate', decode(repeat('33',32),'hex'), 'succeeded', clock_timestamp() - interval '1 second', clock_timestamp(), $2, 0, decode(repeat('44',32),'hex') FROM a`,
		w.ownerID, w.envID, w.serverID, depID, "rbj-key-"+releaseID, w.relA, releaseID)
}

// The claim-time re-check under the locked release row, exercised directly:
// the queries cannot return such a release together with these facts, so the
// helper is called with a hand-built candidate.
func TestClaimFailedCandidateSkipsAReleaseThatWentLive(t *testing.T) {
	for name, live := range map[string]func(t *testing.T, w cleanupWorld, rel string, attempts []int64){
		"succeeded activate op (any job)": func(t *testing.T, w cleanupWorld, rel string, _ []int64) { w.addRollbackJobActivation(t, rel) },
		"route head active release": func(t *testing.T, w cleanupWorld, rel string, _ []int64) {
			w.forceSQL(t, `UPDATE route_heads SET active_release_id=$2 WHERE environment_id=$1`, w.envID, rel)
		},
	} {
		for _, shape := range []string{"open", "retry", "fresh"} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				w := newCleanupWorld(t, 179, DefaultCleanupPolicy())
				rel, attempts := w.addFailedCandidate(t, "lv", failedSpec{})
				c := cleanupCandidate{releaseID: rel, environmentID: w.envID, failed: true}
				state := "candidate"
				if shape != "fresh" {
					cl := w.claimKinds(t, 8)[0]
					c.openCleanupID = cl.CleanupID
					state = "removing"
					if shape == "retry" {
						if err := w.report(t, cl.CleanupID, "failed", "stop", false, "x"); err != nil {
							t.Fatal(err)
						}
						c.openCleanupID, c.retry = 0, true
					}
				}
				live(t, w, rel, attempts)
				ctx := context.Background()
				tx, err := w.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx) //nolint:errcheck
				_, ok, err := w.lease.claimFailedCandidate(ctx, tx, w.serverID, c, state)
				if err != nil || ok {
					t.Fatalf("ok=%v err=%v: a release that went live must never be handed out", ok, err)
				}
				var n int
				if err = tx.QueryRow(ctx, `SELECT count(*) FROM release_cleanups WHERE release_id=$1`, rel).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if want := map[string]int{"fresh": 0, "open": 1, "retry": 1}[shape]; n != want {
					t.Fatalf("cleanup rows = %d, want %d (no new row for a skipped item)", n, want)
				}
			})
		}
	}
}
