//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWebhookJobRepositoryEnqueueVerifiedPush(t *testing.T) {
	githubRepo, _, principal := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(82), digest(83)
	if err := githubRepo.CreateSetupState(ctx, githubSetupInput(state, principal)); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.CompleteHandoff(ctx, handoff, principal, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.BindRepository(ctx, principal, binding("project"), "webhook-bind"); err != nil {
		t.Fatal(err)
	}
	resetWebhookJobs(t, githubRepo.pool)

	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	input := webhookInput(t, principal.OwnerID, "delivery-one", 1, "deployment-one", "intent-one")
	result, err := repo.EnqueueVerifiedPush(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeliveryID == 0 || result.JobID == 0 || result.DeploymentID != "deployment-one" {
		t.Fatalf("result=%+v", result)
	}

	progressed, err := input.Deployment.Transition(deployment.StatusAssigned, input.Deployment.CreatedAt().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.deployments.Save(ctx, progressed); err != nil {
		t.Fatal(err)
	}
	again, err := repo.EnqueueVerifiedPush(ctx, input)
	if err != nil || again != result {
		t.Fatalf("exact redelivery after progress result=%+v err=%v", again, err)
	}
	conflict := input
	conflict.Delivery.BodyDigest[1] = 9
	if _, err = repo.EnqueueVerifiedPush(ctx, conflict); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("delivery digest conflict=%v", err)
	}
	replay := webhookInput(t, principal.OwnerID, "delivery-replay", 1, "deployment-replay", "intent-replay")
	if _, err = repo.EnqueueVerifiedPush(ctx, replay); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("body replay=%v", err)
	}
	creationMismatch := input
	creationMismatch.Delivery.DeliveryID = "delivery-created-at-mismatch"
	creationMismatch.Delivery.BodyDigest[0] = 7
	creationMismatch.Deployment, err = deployment.New(input.Deployment.Identity(), input.Deployment.CreatedAt().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.EnqueueVerifiedPush(ctx, creationMismatch); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("creation timestamp mismatch=%v", err)
	}
	var mismatchReceipts int
	if err = githubRepo.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&mismatchReceipts); err != nil || mismatchReceipts != 1 {
		t.Fatalf("creation timestamp mismatch retained receipt=%d err=%v", mismatchReceipts, err)
	}
	intentReplay := input
	intentReplay.Delivery.DeliveryID = "delivery-intent"
	intentReplay.Delivery.BodyDigest[0] = 2
	duplicate, err := repo.EnqueueVerifiedPush(ctx, intentReplay)
	if err != nil || duplicate.DeploymentID != result.DeploymentID || duplicate.JobID != result.JobID {
		t.Fatalf("intent replay=%+v err=%v", duplicate, err)
	}
	var deliveries, deployments, jobs, events int
	if err = githubRepo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM webhook_deliveries),(SELECT count(*) FROM deployments),(SELECT count(*) FROM agent_jobs),(SELECT count(*) FROM agent_job_events)`).Scan(&deliveries, &deployments, &jobs, &events); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 || deployments != 1 || jobs != 1 || events != 1 {
		t.Fatalf("counts deliveries=%d deployments=%d jobs=%d events=%d", deliveries, deployments, jobs, events)
	}
	if _, err = githubRepo.pool.Exec(ctx, `UPDATE agent_jobs SET priority=1 WHERE id=$1`, result.JobID); err == nil {
		t.Fatal("mutable job update succeeded")
	}
	if _, err = githubRepo.pool.Exec(ctx, `DELETE FROM agent_job_events WHERE job_id=$1`, result.JobID); err == nil {
		t.Fatal("job event delete succeeded")
	}
}

func TestWebhookJobRepositoryDistinctDeliverySameIntentConcurrent(t *testing.T) {
	githubRepo, _, principal := webhookAuthority(t, "webhook-bind-intent-race", 91, 92)
	resetWebhookJobs(t, githubRepo.pool)
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	original := webhookInput(t, principal.OwnerID, "delivery-intent-original", 31, "deployment-intent-original", "intent-intent-original")
	second := original
	second.Delivery.DeliveryID = "delivery-intent-second"
	second.Delivery.BodyDigest[0] = 32
	start, results := make(chan struct{}), make(chan error, 2)
	for _, input := range []WebhookEnqueueInput{original, second} {
		go func(in WebhookEnqueueInput) {
			<-start
			_, err := repo.EnqueueVerifiedPush(context.Background(), in)
			results <- err
		}(input)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("same intent concurrent: %v", err)
		}
	}
	var deliveries, jobs int
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM webhook_deliveries),(SELECT count(*) FROM agent_jobs)`).Scan(&deliveries, &jobs); err != nil || deliveries != 2 || jobs != 1 {
		t.Fatalf("deliveries=%d jobs=%d err=%v", deliveries, jobs, err)
	}
	conflict := second
	conflict.Deployment, _ = deployment.New(deployment.Identity{DeploymentID: "different-deployment", ProjectID: "project", EnvironmentID: "environment", ServerID: "server", CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	conflict.Job, _ = queue.NewDeploymentExecutionJob(principal.OwnerID, "project", "environment", "server", "different-deployment", 201, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "different-key", 0, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	conflict.Delivery.DeliveryID = "delivery-intent-conflict"
	conflict.Delivery.BodyDigest[0] = 33
	if _, err := repo.EnqueueVerifiedPush(context.Background(), conflict); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("intent immutable mismatch=%v", err)
	}
}

func TestWebhookJobRepositoryRollsBackAndSerializesDuplicate(t *testing.T) {
	githubRepo, _, principal := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(84), digest(85)
	if err := githubRepo.CreateSetupState(ctx, githubSetupInput(state, principal)); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.CompleteHandoff(ctx, handoff, principal, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.BindRepository(ctx, principal, binding("project"), "webhook-bind-race"); err != nil {
		t.Fatal(err)
	}
	resetWebhookJobs(t, githubRepo.pool)
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	bad := webhookInput(t, principal.OwnerID, "delivery-rollback", 3, "deployment-rollback", "intent-rollback")
	bad.Job.PayloadDigest = [32]byte{}
	if _, err := repo.EnqueueVerifiedPush(ctx, bad); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("invalid job=%v", err)
	}
	var count int
	if err := githubRepo.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback receipts=%d err=%v", count, err)
	}

	in := webhookInput(t, principal.OwnerID, "delivery-concurrent", 4, "deployment-concurrent", "intent-concurrent")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, err := repo.EnqueueVerifiedPush(ctx, in); results <- err }()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent enqueue=%v", err)
		}
	}
	if err := githubRepo.pool.QueryRow(ctx, `SELECT count(*) FROM agent_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent jobs=%d err=%v", count, err)
	}
}

func TestWebhookJobRepositoryInjectedFailureRollsBackEveryStage(t *testing.T) {
	githubRepo, _, principal := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(86), digest(87)
	if err := githubRepo.CreateSetupState(ctx, githubSetupInput(state, principal)); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.CompleteHandoff(ctx, handoff, principal, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.BindRepository(ctx, principal, binding("project"), "webhook-bind-failure"); err != nil {
		t.Fatal(err)
	}
	resetWebhookJobs(t, githubRepo.pool)
	for index, stage := range []string{"receipt", "intent", "job", "deployment", "deployment_event", "job_event", "processed"} {
		repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
		repo.failAfter = func(current string) error {
			if current == stage {
				return errors.New("injected")
			}
			return nil
		}
		if _, err := repo.EnqueueVerifiedPush(ctx, webhookInput(t, principal.OwnerID, "delivery-failure-"+stage, byte(10+index), "deployment-failure-"+stage, "intent-failure-"+stage)); err == nil {
			t.Fatalf("stage %s succeeded", stage)
		}
		var receipts, intents, jobs, deployments, deploymentEvents, jobEvents int
		if err := githubRepo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM webhook_deliveries),(SELECT count(*) FROM automatic_deployment_intents),(SELECT count(*) FROM agent_jobs),(SELECT count(*) FROM deployments),(SELECT count(*) FROM deployment_events),(SELECT count(*) FROM agent_job_events)`).Scan(&receipts, &intents, &jobs, &deployments, &deploymentEvents, &jobEvents); err != nil {
			t.Fatal(err)
		}
		if receipts != 0 || intents != 0 || jobs != 0 || deployments != 0 || deploymentEvents != 0 || jobEvents != 0 {
			t.Fatalf("stage %s persisted receipt=%d intent=%d job=%d deployment=%d deployment_event=%d job_event=%d", stage, receipts, intents, jobs, deployments, deploymentEvents, jobEvents)
		}
	}
}

func TestWebhookJobRepositoryRecordTerminalRejectionAndReceiptConstraints(t *testing.T) {
	githubRepo, _, _ := webhookAuthority(t, "webhook-bind-record", 93, 94)
	resetWebhookJobs(t, githubRepo.pool)
	repo := NewWebhookJobRepository(githubRepo.pool, nil)
	digest := github.Digest{}
	digest[0] = 41
	in := TerminalRejectionInput{AppRegistrationID: 1, Delivery: github.Delivery{DeliveryID: "delivery-rejected", Event: github.EventPush, BodyDigest: digest, Verification: github.VerificationRejected, Processing: github.ProcessingRejected, Failure: github.FailureSignature}}
	id, err := repo.RecordTerminalRejection(context.Background(), in)
	if err != nil || id == 0 {
		t.Fatalf("record rejected=%d %v", id, err)
	}
	if got, err := repo.RecordTerminalRejection(context.Background(), in); err != nil || got != id {
		t.Fatalf("record replay=%d %v", got, err)
	}
	installationID := int64(101)
	changed := in
	changed.InstallationID = &installationID
	if _, err := repo.RecordTerminalRejection(context.Background(), changed); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("terminal replay identifier mismatch=%v", err)
	}
	other := in
	other.Delivery.DeliveryID = "delivery-rejected-other"
	if _, err := repo.RecordTerminalRejection(context.Background(), other); err != nil {
		t.Fatalf("signature rejection permits same digest under a distinct delivery ID: %v", err)
	}
	if _, err := githubRepo.pool.Exec(context.Background(), `UPDATE github_app_registrations SET active=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	verifiedDigest := github.Digest{}
	verifiedDigest[0] = 42
	if _, err := repo.RecordTerminalRejection(context.Background(), TerminalRejectionInput{AppRegistrationID: 1, Delivery: github.Delivery{DeliveryID: "delivery-malformed", Event: github.EventPush, BodyDigest: verifiedDigest, Verification: github.VerificationVerified, Processing: github.ProcessingRejected, Failure: github.FailureMalformed}}); err != nil {
		t.Fatalf("inactive registration rejection audit=%v", err)
	}
	if _, err := repo.RecordTerminalRejection(context.Background(), TerminalRejectionInput{AppRegistrationID: 1, Delivery: github.Delivery{DeliveryID: "not-terminal", Event: github.EventPush, BodyDigest: github.Digest{1}, Verification: github.VerificationVerified, Processing: github.ProcessingProcessed}}); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("successful receipt accepted=%v", err)
	}
	if _, err := repo.RecordTerminalRejection(context.Background(), TerminalRejectionInput{AppRegistrationID: 1, Action: "untrusted", Delivery: github.Delivery{DeliveryID: "rejected-action", Event: github.EventPush, BodyDigest: github.Digest{2}, Verification: github.VerificationRejected, Processing: github.ProcessingRejected, Failure: github.FailureSignature}}); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("rejected action accepted=%v", err)
	}
	if _, err := githubRepo.pool.Exec(context.Background(), `UPDATE webhook_deliveries SET created_at=clock_timestamp() WHERE id=$1`, id); err == nil {
		t.Fatal("created_at mutation succeeded")
	}
	if _, err := githubRepo.pool.Exec(context.Background(), `DELETE FROM webhook_deliveries WHERE id=$1`, id); err == nil {
		t.Fatal("delivery delete succeeded")
	}
	_, err = githubRepo.pool.Exec(context.Background(), `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class) VALUES(1,'delivery-invalid-event','unknown','',decode(repeat('03',32),'hex'),'rejected','rejected','signature')`)
	assertPgConstraint(t, err, "23514", "webhook_deliveries_event_type_check")
	for _, check := range []struct {
		column, value, constraint string
	}{
		{"installation_id", "0", "webhook_deliveries_installation_id_check"},
		{"repository_id", "0", "webhook_deliveries_repository_id_check"},
		{"github_owner_id", "0", "webhook_deliveries_github_owner_id_check"},
	} {
		query := `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class,` + check.column + `) VALUES(1,'delivery-invalid-` + check.column + `','push','',decode(repeat('03',32),'hex'),'rejected','rejected','signature',` + check.value + `)`
		_, err := githubRepo.pool.Exec(context.Background(), query)
		assertPgConstraint(t, err, "23514", check.constraint)
	}
	for _, ref := range []string{"refs/heads/trailing.", "refs/heads/a\x7fb", "refs/heads/a" + string(rune(1)) + "b"} {
		_, err := githubRepo.pool.Exec(context.Background(), `INSERT INTO automatic_deployment_intents(delivery_id,binding_owner_id,binding_project_id,repository_id,full_ref,commit_sha,deployment_id) VALUES($1,'owner','project',201,$2,'0123456789abcdef0123456789abcdef01234567','missing')`, id, ref)
		assertPgConstraint(t, err, "23514", "automatic_deployment_intents_full_ref_check")
	}
	validDeployment, err := deployment.New(deployment.Identity{DeploymentID: "constraint-deployment", ProjectID: "project", EnvironmentID: "environment", ServerID: "server", CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(githubRepo.pool).Create(context.Background(), validDeployment); err != nil {
		t.Fatal(err)
	}
	_, err = githubRepo.pool.Exec(context.Background(), `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state) VALUES(1,'deployment_execution','owner','project','environment','server','constraint-deployment',201,'refs/heads/main','0123456789abcdef0123456789abcdef01234567',0,'infinity','infinity-key',decode(repeat('01',32),'hex'),'queued')`)
	assertPgConstraint(t, err, "23514", "agent_jobs_available_at_finite_check")
}

func TestWebhookJobRepositoryAuthorityLockBlocksDisableThenRevalidates(t *testing.T) {
	githubRepo, _, principal := webhookAuthority(t, "webhook-bind-authority", 95, 96)
	resetWebhookJobs(t, githubRepo.pool)
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	paused, release := make(chan struct{}), make(chan struct{})
	repo.failAfter = func(stage string) error {
		if stage == "receipt" {
			close(paused)
			<-release
		}
		return nil
	}
	enqueued := make(chan error, 1)
	go func() {
		_, err := repo.EnqueueVerifiedPush(context.Background(), webhookInput(t, principal.OwnerID, "delivery-authority", 51, "deployment-authority", "intent-authority"))
		enqueued <- err
	}()
	<-paused
	for _, query := range []string{
		`UPDATE github_app_registrations SET slug=slug WHERE id=1`,
		`UPDATE github_installations SET updated_at=updated_at WHERE installation_id=101`,
		`UPDATE github_repositories SET name=name WHERE repository_id=201`,
		`UPDATE github_project_bindings SET deploy_ref=deploy_ref WHERE owner_id=$1 AND project_id='project'`,
	} {
		tx, err := githubRepo.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(context.Background(), `SET LOCAL lock_timeout = '100ms'`); err != nil {
			tx.Rollback(context.Background()) //nolint:errcheck
			t.Fatal(err)
		}
		args := []any(nil)
		if query == `UPDATE github_project_bindings SET deploy_ref=deploy_ref WHERE owner_id=$1 AND project_id='project'` {
			args = []any{principal.OwnerID}
		}
		_, err = tx.Exec(context.Background(), query, args...)
		assertPgSQLState(t, err, "55P03")
		if err = tx.Rollback(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	if err := <-enqueued; err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.DisableInstallation(context.Background(), principal, 101, "disable-authority"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool)).EnqueueVerifiedPush(context.Background(), webhookInput(t, principal.OwnerID, "delivery-after-disable", 52, "deployment-after-disable", "intent-after-disable")); !errors.Is(err, ErrWebhookUnauthorized) {
		t.Fatalf("disable-before-lock=%v", err)
	}
}

func TestWebhookJobRepositoryProcessLifecycleIsAtomicAndFailClosed(t *testing.T) {
	githubRepo, _, _ := webhookAuthority(t, "webhook-bind-lifecycle-atomic", 97, 98)
	resetWebhookJobs(t, githubRepo.pool)
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	delivery := lifecycleDelivery("lifecycle-suspend", 61, github.EventInstallation)
	lifecycle := github.Lifecycle{Event: github.EventInstallation, Action: "suspend", InstallationID: 101, OwnerID: 9}
	repo.failAfter = func(stage string) error {
		if stage == "lifecycle_effect" {
			return errors.New("injected")
		}
		return nil
	}
	if err := repo.ProcessLifecycle(context.Background(), delivery, lifecycle); err == nil {
		t.Fatal("injected lifecycle failure succeeded")
	}
	var active, suspended bool
	var receipts int
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active,suspended FROM github_installations WHERE installation_id=101`).Scan(&active, &suspended); err != nil || !active || suspended {
		t.Fatalf("rollback installation active=%t suspended=%t err=%v", active, suspended, err)
	}
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_deliveries`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("rollback receipt count=%d err=%v", receipts, err)
	}
	repo.failAfter = nil
	if err := repo.ProcessLifecycle(context.Background(), delivery, lifecycle); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active,suspended FROM github_installations WHERE installation_id=101`).Scan(&active, &suspended); err != nil || active || !suspended {
		t.Fatalf("suspended installation active=%t suspended=%t err=%v", active, suspended, err)
	}
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT count(*) FROM github_repositories WHERE installation_id=101 AND active`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("active repositories=%d err=%v", receipts, err)
	}
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT count(*) FROM github_project_bindings WHERE installation_id=101 AND active`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("active bindings=%d err=%v", receipts, err)
	}
	if err := repo.ProcessLifecycle(context.Background(), delivery, lifecycle); err != nil {
		t.Fatalf("exact replay=%v", err)
	}
	changed := delivery
	changed.BodyDigest[1] = 1
	if err := repo.ProcessLifecycle(context.Background(), changed, lifecycle); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("digest mismatch=%v", err)
	}
	created := lifecycle
	created.Action = "created"
	if err := repo.ProcessLifecycle(context.Background(), lifecycleDelivery("lifecycle-created", 62, github.EventInstallation), created); err != nil {
		t.Fatalf("created lifecycle=%v", err)
	}
	unsuspend := lifecycle
	unsuspend.Action = "unsuspend"
	if err := repo.ProcessLifecycle(context.Background(), lifecycleDelivery("lifecycle-unsuspend", 63, github.EventInstallation), unsuspend); err != nil {
		t.Fatalf("unsuspend lifecycle=%v", err)
	}
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active,suspended FROM github_installations WHERE installation_id=101`).Scan(&active, &suspended); err != nil || active || !suspended {
		t.Fatalf("created/unsuspend reactivated authority active=%t suspended=%t err=%v", active, suspended, err)
	}
}

func TestWebhookJobRepositoryLifecycleDeletedAndRepositoryRemovalAreAtomic(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		githubRepo, _, _ := webhookAuthority(t, "webhook-bind-lifecycle-deleted", 101, 102)
		resetWebhookJobs(t, githubRepo.pool)
		repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
		delivery := lifecycleDelivery("lifecycle-deleted", 64, github.EventInstallation)
		value := github.Lifecycle{Event: github.EventInstallation, Action: "deleted", InstallationID: 101, OwnerID: 9}
		if err := repo.ProcessLifecycle(context.Background(), delivery, value); err != nil {
			t.Fatal(err)
		}
		var active, suspended bool
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active,suspended FROM github_installations WHERE installation_id=101`).Scan(&active, &suspended); err != nil || active || !suspended {
			t.Fatalf("deleted installation active=%t suspended=%t err=%v", active, suspended, err)
		}
		var count int
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT count(*) FROM github_repositories WHERE installation_id=101 AND active`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("deleted active repositories=%d err=%v", count, err)
		}
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT count(*) FROM github_project_bindings WHERE installation_id=101 AND active`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("deleted active bindings=%d err=%v", count, err)
		}
		if err := repo.ProcessLifecycle(context.Background(), delivery, value); err != nil {
			t.Fatalf("deleted exact replay=%v", err)
		}
	})

	t.Run("remove", func(t *testing.T) {
		githubRepo, _, _ := webhookAuthority(t, "webhook-bind-lifecycle-removed", 103, 104)
		resetWebhookJobs(t, githubRepo.pool)
		repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
		removedDelivery := lifecycleDelivery("lifecycle-removed", 65, github.EventInstallationRepositories)
		removed := github.Lifecycle{Event: github.EventInstallationRepositories, Action: "removed", InstallationID: 101, RepositoryID: 201, OwnerID: 9}
		repo.failAfter = func(stage string) error {
			if stage == "lifecycle_receipt" {
				return errors.New("injected")
			}
			return nil
		}
		if err := repo.ProcessLifecycle(context.Background(), removedDelivery, removed); err == nil {
			t.Fatal("receipt failure succeeded")
		}
		var active bool
		var receipts int
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active FROM github_repositories WHERE repository_id=201`).Scan(&active); err != nil || !active {
			t.Fatalf("receipt rollback repository active=%t err=%v", active, err)
		}
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_deliveries`).Scan(&receipts); err != nil || receipts != 0 {
			t.Fatalf("receipt rollback deliveries=%d err=%v", receipts, err)
		}
		repo.failAfter = nil
		if err := repo.ProcessLifecycle(context.Background(), removedDelivery, removed); err != nil {
			t.Fatal(err)
		}
		var deliveryID int64
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT id FROM webhook_deliveries WHERE delivery_id=$1`, removedDelivery.DeliveryID).Scan(&deliveryID); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{
			`UPDATE webhook_lifecycle_repositories SET repository_id=202 WHERE delivery_id=$1 AND repository_id=201`,
			`DELETE FROM webhook_lifecycle_repositories WHERE delivery_id=$1 AND repository_id=201`,
			`INSERT INTO webhook_lifecycle_repositories(delivery_id,repository_id) VALUES($1,202)`,
			`DELETE FROM webhook_deliveries WHERE id=$1`,
		} {
			if _, err := githubRepo.pool.Exec(context.Background(), query, deliveryID); err == nil {
				t.Fatalf("mutable lifecycle repository set query succeeded: %s", query)
			}
		}
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active FROM github_repositories WHERE repository_id=201`).Scan(&active); err != nil || active {
			t.Fatalf("removed repository active=%t err=%v", active, err)
		}
		if err := repo.ProcessLifecycle(context.Background(), removedDelivery, removed); err != nil {
			t.Fatalf("removed exact replay=%v", err)
		}
		added := github.Lifecycle{Event: github.EventInstallationRepositories, Action: "added", InstallationID: 101, RepositoryID: 201, OwnerID: 9}
		if err := repo.ProcessLifecycle(context.Background(), lifecycleDelivery("lifecycle-added", 66, github.EventInstallationRepositories), added); err != nil {
			t.Fatalf("added lifecycle=%v", err)
		}
		if err := githubRepo.pool.QueryRow(context.Background(), `SELECT active FROM github_repositories WHERE repository_id=201`).Scan(&active); err != nil || active {
			t.Fatalf("added reactivated repository active=%t err=%v", active, err)
		}
	})
}

func TestWebhookJobRepositoryProcessPushBuildsAndReplaysInsideTransaction(t *testing.T) {
	githubRepo, _, principal := webhookAuthority(t, "webhook-bind-process-push", 99, 100)
	resetWebhookJobs(t, githubRepo.pool)
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	in := webhookInput(t, principal.OwnerID, "process-push", 71, "ignored-by-process-push", "ignored-by-process-push")
	result, err := repo.ProcessPush(context.Background(), in.Delivery, in.Push)
	if err != nil || result.DeliveryID == 0 || result.JobID == 0 || result.DeploymentID == "" {
		t.Fatalf("process push result=%+v err=%v", result, err)
	}
	if again, err := repo.ProcessPush(context.Background(), in.Delivery, in.Push); err != nil || again != result {
		t.Fatalf("exact replay result=%+v err=%v", again, err)
	}
	other := in.Delivery
	other.DeliveryID = "process-push-other"
	other.BodyDigest[1] = 1
	if replay, err := repo.ProcessPush(context.Background(), other, in.Push); err != nil || replay.DeploymentID != result.DeploymentID || replay.JobID != result.JobID {
		t.Fatalf("intent replay result=%+v err=%v", replay, err)
	}
}

func lifecycleDelivery(id string, body byte, event github.EventType) github.Delivery {
	digest := github.Digest{}
	digest[0] = body
	return github.Delivery{DeliveryID: id, Event: event, BodyDigest: digest, Verification: github.VerificationVerified, Processing: github.ProcessingPending}
}

func assertPgConstraint(t *testing.T, err error, state, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state || pgErr.ConstraintName != constraint {
		t.Fatalf("error=%v, want SQLSTATE=%s constraint=%s", err, state, constraint)
	}
}

func assertPgSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state {
		t.Fatalf("error=%v, want SQLSTATE=%s", err, state)
	}
}

func webhookAuthority(t *testing.T, audit string, stateByte, handoffByte byte) (*GitHubRepository, *AuthRepository, auth.Principal) {
	t.Helper()
	repo, authRepo, principal := freshGitHubRepository(t)
	state, handoff := digest(stateByte), digest(handoffByte)
	if err := repo.CreateSetupState(context.Background(), githubSetupInput(state, principal)); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(context.Background(), state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(context.Background(), handoff, principal, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(context.Background(), principal, binding("project"), audit); err != nil {
		t.Fatal(err)
	}
	return repo, authRepo, principal
}

func resetWebhookJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE releases, release_operations, release_runtime_instances, agent_job_attempt_execution, frozen_secret_versions, agent_job_events, agent_job_attempts, environment_slots, agent_jobs, automatic_deployment_intents, webhook_lifecycle_repositories, webhook_deliveries, deployment_events, deployments RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
}

func githubSetupInput(d [32]byte, p auth.Principal) githubapp.SetupStateInput {
	return githubapp.SetupStateInput{Digest: d, Principal: p}
}

func webhookInput(t *testing.T, ownerID, deliveryID string, body byte, deploymentID, key string) WebhookEnqueueInput {
	t.Helper()
	digest := github.Digest{}
	digest[0] = body
	when := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	d, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(deploymentID), ProjectID: "project", EnvironmentID: "environment", ServerID: "server", CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, when)
	if err != nil {
		t.Fatal(err)
	}
	j, err := queue.NewDeploymentExecutionJob(ownerID, "project", "environment", "server", deploymentID, 201, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", key, 0, when)
	if err != nil {
		t.Fatal(err)
	}
	return WebhookEnqueueInput{AppRegistrationID: 1, Delivery: github.Delivery{DeliveryID: deliveryID, Event: github.EventPush, BodyDigest: digest, Verification: github.VerificationVerified, Processing: github.ProcessingPending}, Push: github.Push{InstallationID: 101, RepositoryID: 201, OwnerID: 9, Ref: "refs/heads/main", CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, Deployment: d, Job: j}
}
