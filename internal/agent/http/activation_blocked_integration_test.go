//go:build integration

package agenthttp

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// TestAgentHTTPStrandedEnvironmentActivateIsBlockedAfterLegacySweeperClose
// drives the stranded scenario through real HTTP and the real repositories.
// The final stranded state is NOT reachable through production code since step
// D, so the legacy sweeper close is forced in SQL (hence the name): claim ->
// activate -> ambiguous activation-result -> the stranded attempt's authority
// lapses, the batch sweep leaves it leased and the pre-step-D close is forced
// in SQL (see the comment at that step) ->
// the agent's real reconcile is refused as corrupt state (step G2a) and its
// former slot-flag clear is forced in SQL too -> a DIFFERENT attempt on
// the same environment calls activate. The route-head flag and the stranded
// operation are what remain, and the answer must be 409
// environment_activation_blocked: not 409 stale_lease, not the retryable 503.
//
// Coupled to steps D/E/G: the precondition triple is asserted right before the
// final call so a later change to the sweeper or reconcile fails loudly.
func TestAgentHTTPStrandedEnvironmentActivateIsBlockedAfterLegacySweeperClose(t *testing.T) {
	ctx := context.Background()
	f := newAgentHTTPFixture(t)
	leases := postgres.NewAgentLeaseRepository(f.pool, postgres.NewDeploymentRepository(f.pool))
	h := newTestHandler(f.pool)
	h.Activator = leases
	h.ActivationReporter = leases
	srv := httptest.NewServer(withTLS(h))
	defer srv.Close()

	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := server.CredentialTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := postJSON(t, srv, pathEnroll, "", "", enrollRequest{ServerID: f.serverID, EnrollmentToken: string(f.issueEnrollment(t)), CredentialDigest: hexOf(digest)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll: status=%d body=%v", resp.StatusCode, body)
	}

	claim := func(wantJob int64) (attemptID, fence int64) {
		t.Helper()
		resp, body := postJSON(t, srv, pathClaim, token, f.serverID, claimRequest{Limit: 1, ExecutorAvailable: true})
		leasesBody, _ := body["leases"].([]any)
		if resp.StatusCode != http.StatusOK || len(leasesBody) != 1 {
			t.Fatalf("claim: status=%d body=%v", resp.StatusCode, body)
		}
		lease := leasesBody[0].(map[string]any)
		if int64(lease["job_id"].(float64)) != wantJob {
			t.Fatalf("claimed job %v, want %d", lease["job_id"], wantJob)
		}
		return int64(lease["attempt_id"].(float64)), int64(lease["fence"].(float64))
	}
	toHealthChecking := func(attemptID, fence int64) {
		t.Helper()
		for _, phase := range []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking} {
			resp, body := postJSON(t, srv, pathProgress, token, f.serverID, progressRequest{AttemptID: attemptID, Fence: fence, Phase: string(phase)})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("progress %s: status=%d body=%v", phase, resp.StatusCode, body)
			}
		}
	}
	imageDigest := hexOf(digestFixture(7))
	// route_heads.hostname is globally unique and route_heads rows cannot be
	// deleted (trigger route_heads_no_delete), and this fixture never resets
	// shared tables (see newAgentHTTPFixture), so a re-run against a
	// non-recreated database must use a fresh hostname instead of a cleanup.

	attempt1, fence1 := claim(f.jobID)
	toHealthChecking(attempt1, fence1)
	if _, err := f.pool.Exec(ctx, `INSERT INTO route_heads(environment_id, hostname, route_revision) VALUES ($1,$2,0)`, f.envID, "app-"+randomSuffix(t)+".example.com"); err != nil {
		t.Fatal(err)
	}
	resp, body = postJSON(t, srv, pathActivate, token, f.serverID, activateRequest{AttemptID: attempt1, Fence: fence1, ImageDigest: imageDigest})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first activate: status=%d body=%v", resp.StatusCode, body)
	}
	operationID, _ := body["operation_id"].(string)
	failed := false
	resp, body = postJSON(t, srv, pathActivationResult, token, f.serverID, activationResultRequest{AttemptID: attempt1, Fence: fence1, OperationID: operationID, Succeeded: &failed, FailureSummary: "reload timed out"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ambiguous activation-result: status=%d body=%v", resp.StatusCode, body)
	}
	var opState string
	var flagged bool
	if err := f.pool.QueryRow(ctx, `SELECT state FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, attempt1).Scan(&opState); err != nil || opState != "reconciliation_required" {
		t.Fatalf("operation state=%q err=%v, want reconciliation_required", opState, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT reconciliation_required FROM route_heads WHERE environment_id=$1`, f.envID).Scan(&flagged); err != nil || !flagged {
		t.Fatalf("route head flagged=%v err=%v, want true", flagged, err)
	}

	// Only time is simulated: the stranded attempt's deadlines lapse. Since
	// step D the sweeper does not close such an attempt (it records the block
	// and leaves the attempt leased), so it no longer produces a
	// stranded-but-activatable environment, and nothing does until step G. The
	// real batch sweep is run and the attempt is asserted to be LEFT LEASED
	// (through the batch path, which is what this test exercises; discovery
	// alone already excludes a recorded attempt, so this does not by itself
	// prove the diversion -- the store tests drive expireOne directly for
	// that). The pre-step-D close it used to perform (attempt expired, fence
	// advanced, slot vacated and flagged, job retry_wait) is then forced in
	// SQL, which is the shape an already-deployed database may hold. Everything
	// after it (the real reconcile, the second claim, the assertions) is
	// unchanged.
	if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET lease_expires_at=now()-interval '2 seconds', authority_expires_at=now()-interval '2 seconds' WHERE id=$1`, attempt1); err != nil {
		t.Fatal(err)
	}
	// ExpireBatch is global; assert on THIS attempt.
	if _, err := leases.ExpireBatch(ctx, 10); err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}
	var attemptState string
	if err := f.pool.QueryRow(ctx, `SELECT state FROM agent_job_attempts WHERE id=$1`, attempt1).Scan(&attemptState); err != nil || attemptState != "leased" {
		t.Fatalf("stranded attempt state=%q err=%v, want left leased by the batch sweep", attemptState, err)
	}
	for _, stmt := range []string{
		`UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`,
		`UPDATE environment_slots SET fence=fence+1, current_attempt_id=NULL, reconciliation_required=true, reconciliation_attempt_id=$1, updated_at=clock_timestamp() WHERE environment_id=(SELECT environment_id FROM agent_job_attempts WHERE id=$1)`,
		`UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp(), updated_at=clock_timestamp() WHERE id=(SELECT job_id FROM agent_job_attempts WHERE id=$1)`,
	} {
		if _, err := f.pool.Exec(ctx, stmt, attempt1); err != nil {
			t.Fatalf("force legacy sweeper close: %v", err)
		}
	}
	// Since step G2a the real reconcile refuses this legacy shape (closed
	// attempt, unresolved activation) as corrupt state instead of clearing the
	// flag, so the clear it used to perform is forced in SQL as well.
	resp, body = postJSON(t, srv, pathReconcile, token, f.serverID, reconcileRequest{EnvironmentID: f.envID, AttemptID: attempt1, Fence: fence1, OutcomeDigest: hexOf(digestFixture(8))})
	if resp.StatusCode != http.StatusConflict || body["error"] != "corrupt_state" {
		t.Fatalf("reconcile on the legacy shape: status=%d body=%v, want 409 corrupt_state", resp.StatusCode, body)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=false, reconciliation_attempt_id=NULL, updated_at=clock_timestamp() WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatalf("force legacy reconcile clear: %v", err)
	}

	// Slot must be free and clear before the second claim takes it.
	var slotCurrent *int64
	var slotFlag bool
	if err := f.pool.QueryRow(ctx, `SELECT current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1`, f.envID).Scan(&slotCurrent, &slotFlag); err != nil || slotCurrent != nil || slotFlag {
		t.Fatalf("after reconcile slot current=%v flagged=%v err=%v, want free and clear", slotCurrent, slotFlag, err)
	}

	job2 := f.insertSecondJob(t)
	attempt2, fence2 := claim(job2)
	if attempt2 == attempt1 {
		t.Fatal("second claim returned the stranded attempt")
	}
	toHealthChecking(attempt2, fence2)

	// Full precondition triple, asserted immediately before the final call. It
	// couples this proof to steps D/E/G: if a later step changes what the
	// sweeper or reconcile leave behind, this fails loudly instead of the
	// activate being blocked by the INSERT backstop and the test silently
	// proving something weaker.
	if err := f.pool.QueryRow(ctx, `SELECT state FROM release_operations WHERE attempt_id=$1 AND kind='activate'`, attempt1).Scan(&opState); err != nil || (opState != "switch_authorized" && opState != "reconciliation_required") {
		t.Fatalf("stranded operation state=%q err=%v, want still unresolved", opState, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT reconciliation_required FROM route_heads WHERE environment_id=$1`, f.envID).Scan(&flagged); err != nil || !flagged {
		t.Fatalf("route head flagged=%v err=%v, want still set", flagged, err)
	}
	// The slot is now occupied by the second attempt's own claim; its flag
	// must still be clear and the stranded attempt must not be the occupant.
	if err := f.pool.QueryRow(ctx, `SELECT current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1`, f.envID).Scan(&slotCurrent, &slotFlag); err != nil {
		t.Fatal(err)
	}
	if slotCurrent == nil || *slotCurrent != attempt2 || slotFlag {
		t.Fatalf("slot current=%v flagged=%v, want second attempt as occupant and flag clear", slotCurrent, slotFlag)
	}

	resp, body = postJSON(t, srv, pathActivate, token, f.serverID, activateRequest{AttemptID: attempt2, Fence: fence2, ImageDigest: imageDigest})
	if resp.StatusCode != http.StatusConflict || body["error"] != "environment_activation_blocked" {
		t.Fatalf("second activate: status=%d body=%v, want 409 environment_activation_blocked", resp.StatusCode, body)
	}
}

// insertSecondJob queues another deployment job on f's environment, shaped
// exactly like the fixture's own job.
func (f agentHTTPFixture) insertSecondJob(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	deploymentID := "agent-http-deployment2-" + t.Name() + "-" + randomSuffix(t)
	var createdAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(deploymentID), ProjectID: "project", EnvironmentID: f.envID, ServerID: f.serverID, CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := postgres.NewDeploymentRepository(f.pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state) VALUES (1,'deployment_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','0123456789abcdef0123456789abcdef01234567',10,clock_timestamp(),$5,$6,'queued') RETURNING id`,
		f.ownerID, f.envID, f.serverID, deploymentID, "agent-http-key2-"+deploymentID, make([]byte, 32)).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES ($1,1,'enqueued')`, jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}
