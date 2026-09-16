package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebhookProcessor adapts verified ingress to durable effects. It deliberately
// has no provider client: all authorization is rechecked by the enqueue tx.
type WebhookProcessor struct{ repository *WebhookJobRepository }

func NewWebhookProcessor(repository *WebhookJobRepository) *WebhookProcessor {
	return &WebhookProcessor{repository: repository}
}

func (p *WebhookProcessor) Reject(ctx context.Context, delivery github.Delivery) error {
	_, err := p.repository.RecordTerminalRejection(ctx, TerminalRejectionInput{AppRegistrationID: 1, Delivery: delivery})
	return err
}

func (p *WebhookProcessor) IsTerminalWebhookError(err error) bool {
	return errors.Is(err, ErrWebhookUnauthorized) || errors.Is(err, ErrWebhookIntegrity)
}

func (p *WebhookProcessor) ProcessPush(ctx context.Context, delivery github.Delivery, push github.Push) error {
	if p == nil || p.repository == nil || p.repository.pool == nil {
		return ErrWebhookUnauthorized
	}
	_, err := p.repository.ProcessPush(ctx, delivery, push)
	return err
}

// webhookIDs uses unambiguous length-prefixed fields and domain separation so
// IDs cannot collide through field concatenation. Prefixes are bounded for the
// deployment and job-key database columns while retaining distinct hash bits.
func webhookIDs(target webhookTarget, push github.Push) (deployment.ID, string) {
	h := sha256.New()
	for _, value := range []string{"autodeploy:webhook:v1", target.ownerID, target.projectID, target.environmentID, target.serverID, fmt.Sprint(push.RepositoryID), push.Ref, push.CommitSHA} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	seed := hex.EncodeToString(h.Sum(nil))
	return deployment.ID("whd_" + seed[:40]), "whj_" + seed[40:]
}

func (p *WebhookProcessor) ProcessLifecycle(ctx context.Context, delivery github.Delivery, lifecycle github.Lifecycle) error {
	if p == nil || p.repository == nil || p.repository.pool == nil || delivery.Validate() != nil || lifecycle.Validate() != nil {
		return ErrWebhookUnauthorized
	}
	return p.repository.ProcessLifecycle(ctx, delivery, lifecycle)
}

// ProcessPush constructs all durable values only after authority is locked and
// revalidated in the same READ COMMITTED transaction.
func (r *WebhookJobRepository) ProcessPush(ctx context.Context, delivery github.Delivery, push github.Push) (WebhookEnqueueResult, error) {
	if r == nil || r.pool == nil || r.deployments == nil || delivery.Validate() != nil || delivery.Event != github.EventPush || delivery.Verification != github.VerificationVerified || delivery.Processing != github.ProcessingPending || push.Validate() != nil {
		return WebhookEnqueueResult{}, ErrWebhookUnauthorized
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return WebhookEnqueueResult{}, fmt.Errorf("begin webhook push: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	target, err := lockWebhookAuthorityForPush(ctx, tx, 1, push)
	if err != nil {
		return WebhookEnqueueResult{}, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return WebhookEnqueueResult{}, fmt.Errorf("webhook canonical time: %w", err)
	}
	deploymentID, jobKey := webhookIDs(target, push)
	d, err := deployment.New(deployment.Identity{DeploymentID: deploymentID, ProjectID: target.projectID, EnvironmentID: target.environmentID, ServerID: target.serverID, CommitSHA: push.CommitSHA}, now)
	if err != nil {
		return WebhookEnqueueResult{}, ErrWebhookUnauthorized
	}
	j, err := queue.NewDeploymentExecutionJob(target.ownerID, target.projectID, target.environmentID, target.serverID, string(deploymentID), push.RepositoryID, push.Ref, push.CommitSHA, jobKey, 0, now)
	if err != nil {
		return WebhookEnqueueResult{}, ErrWebhookUnauthorized
	}
	result, err := r.enqueueVerifiedPushTx(ctx, tx, WebhookEnqueueInput{AppRegistrationID: 1, Delivery: delivery, Push: push, Deployment: d, Job: j}, target, false)
	if err != nil {
		return WebhookEnqueueResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WebhookEnqueueResult{}, fmt.Errorf("commit webhook push: %w", err)
	}
	return result, nil
}

func (r *WebhookJobRepository) ProcessLifecycle(ctx context.Context, delivery github.Delivery, lifecycle github.Lifecycle) error {
	if r == nil || r.pool == nil || delivery.Validate() != nil || lifecycle.Validate() != nil {
		return ErrWebhookUnauthorized
	}
	lifecycle.RepositoryIDs = lifecycleRepositoryIDs(lifecycle)
	if len(lifecycle.RepositoryIDs) > 0 {
		lifecycle.RepositoryID = lifecycle.RepositoryIDs[0]
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = lockLifecycleAuthority(ctx, tx, lifecycle); err != nil {
		return err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,installation_id,repository_id,github_owner_id) VALUES(1,$1,$2,$3,$4,'verified','pending',$5,$6,$7) ON CONFLICT(app_registration_id,delivery_id) DO NOTHING RETURNING id`, delivery.DeliveryID, delivery.Event, lifecycle.Action, delivery.BodyDigest[:], lifecycle.InstallationID, nullableRepository(lifecycle), lifecycle.OwnerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var digest []byte
		var event github.EventType
		var action string
		var installationID, repositoryID, ownerID int64
		err = tx.QueryRow(ctx, `SELECT id,body_digest,event_type,action,installation_id,COALESCE(repository_id,0),github_owner_id FROM webhook_deliveries WHERE app_registration_id=1 AND delivery_id=$1 FOR UPDATE`, delivery.DeliveryID).Scan(&id, &digest, &event, &action, &installationID, &repositoryID, &ownerID)
		if err != nil || !bytes.Equal(digest, delivery.BodyDigest[:]) || event != delivery.Event || action != lifecycle.Action || installationID != lifecycle.InstallationID || repositoryID != lifecycle.RepositoryID || ownerID != lifecycle.OwnerID {
			return ErrWebhookIntegrity
		}
		if err = lifecycleRepositoriesMatch(ctx, tx, id, lifecycle.RepositoryIDs); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err != nil {
		return fmt.Errorf("record lifecycle receipt: %w", err)
	}
	if err = insertLifecycleRepositories(ctx, tx, id, lifecycle.RepositoryIDs); err != nil {
		return err
	}
	if err = r.inject("lifecycle_receipt"); err != nil {
		return err
	}
	if lifecycle.Event == github.EventInstallation && (lifecycle.Action == "deleted" || lifecycle.Action == "suspend") {
		if err = requireOneUpdate(ctx, tx, `UPDATE github_installations SET active=false,suspended=true,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE installation_id=$1 AND account_id=$2`, lifecycle.InstallationID, lifecycle.OwnerID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE github_repositories SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE installation_id=$1`, lifecycle.InstallationID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE github_project_bindings SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE installation_id=$1`, lifecycle.InstallationID); err != nil {
			return err
		}
	}
	if lifecycle.Event == github.EventInstallationRepositories && lifecycle.Action == "removed" {
		for _, repositoryID := range lifecycle.RepositoryIDs {
			if err = requireOneUpdate(ctx, tx, `UPDATE github_repositories SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE repository_id=$1 AND installation_id=$2`, repositoryID, lifecycle.InstallationID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE github_project_bindings SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE repository_id=$1 AND installation_id=$2`, repositoryID, lifecycle.InstallationID); err != nil {
				return err
			}
		}
	}
	if err = r.inject("lifecycle_effect"); err != nil {
		return err
	}
	if err = markDeliveryProcessed(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockLifecycleAuthority(ctx context.Context, tx pgx.Tx, lifecycle github.Lifecycle) error {
	var appID int16
	if err := tx.QueryRow(ctx, `SELECT id FROM github_app_registrations WHERE id=1 AND active FOR SHARE`).Scan(&appID); err != nil || appID != 1 {
		return ErrWebhookUnauthorized
	}
	var known int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM github_installations WHERE installation_id=$1 AND account_id=$2 FOR UPDATE`, lifecycle.InstallationID, lifecycle.OwnerID).Scan(&known); err != nil {
		return ErrWebhookUnauthorized
	}
	if lifecycle.Event == github.EventInstallationRepositories {
		for _, repositoryID := range lifecycleRepositoryIDs(lifecycle) {
			if err := tx.QueryRow(ctx, `SELECT 1 FROM github_repositories WHERE repository_id=$1 AND installation_id=$2 AND github_owner_id=$3 FOR UPDATE`, repositoryID, lifecycle.InstallationID, lifecycle.OwnerID).Scan(&known); err != nil {
				return ErrWebhookUnauthorized
			}
		}
	}
	return nil
}

func lifecycleRepositoryIDs(l github.Lifecycle) []int64 {
	if len(l.RepositoryIDs) > 0 {
		return l.RepositoryIDs
	}
	if l.RepositoryID > 0 {
		return []int64{l.RepositoryID}
	}
	return nil
}

func insertLifecycleRepositories(ctx context.Context, tx pgx.Tx, deliveryID int64, repositoryIDs []int64) error {
	for _, repositoryID := range repositoryIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_lifecycle_repositories(delivery_id,repository_id) VALUES($1,$2)`, deliveryID, repositoryID); err != nil {
			return fmt.Errorf("record lifecycle repository: %w", err)
		}
	}
	return nil
}

func lifecycleRepositoriesMatch(ctx context.Context, tx pgx.Tx, deliveryID int64, want []int64) error {
	rows, err := tx.Query(ctx, `SELECT repository_id FROM webhook_lifecycle_repositories WHERE delivery_id=$1 ORDER BY repository_id`, deliveryID)
	if err != nil {
		return ErrWebhookIntegrity
	}
	defer rows.Close()
	for _, repositoryID := range want {
		if !rows.Next() {
			return ErrWebhookIntegrity
		}
		var got int64
		if err = rows.Scan(&got); err != nil || got != repositoryID {
			return ErrWebhookIntegrity
		}
	}
	if rows.Next() || rows.Err() != nil {
		return ErrWebhookIntegrity
	}
	return nil
}

func requireOneUpdate(ctx context.Context, tx pgx.Tx, query string, args ...any) error {
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrWebhookUnauthorized
	}
	return nil
}

func nullableRepository(l github.Lifecycle) *int64 {
	if l.RepositoryID == 0 {
		return nil
	}
	return &l.RepositoryID
}

// WebhookEnqueueInput contains only already-verified, normalized values. Raw
// webhook material never crosses this persistence boundary.
type WebhookEnqueueInput struct {
	AppRegistrationID int16
	Delivery          github.Delivery
	Push              github.Push
	Deployment        deployment.Deployment
	Job               queue.Job
}

// WebhookEnqueueResult identifies the durable automatic intent and its one
// logical execution job. Existing values are returned on exact redelivery.
type WebhookEnqueueResult struct {
	DeliveryID   int64
	DeploymentID deployment.ID
	JobID        int64
}

// TerminalRejectionInput records only a terminal rejected receipt. It accepts
// no raw body, signature, secret, token, or arbitrary payload data. Optional
// normalized identifiers may be absent when rejection happened before parsing.
type TerminalRejectionInput struct {
	AppRegistrationID int16
	Delivery          github.Delivery
	Action            string
	InstallationID    *int64
	RepositoryID      *int64
	OwnerID           *int64
}

// WebhookJobRepository commits verified receipt, automatic deployment intent,
// deployment aggregate, and immutable queued job in one caller-owned unit of
// work. It intentionally implements enqueue only; claims and leases are not
// part of this slice.
type WebhookJobRepository struct {
	pool        *pgxpool.Pool
	deployments *DeploymentRepository
	failAfter   func(string) error // test-only deterministic rollback seam
}

func NewWebhookJobRepository(pool *pgxpool.Pool, deployments *DeploymentRepository) *WebhookJobRepository {
	if deployments == nil {
		deployments = NewDeploymentRepository(pool)
	}
	return &WebhookJobRepository{pool: pool, deployments: deployments}
}

// RecordTerminalRejection durably records a rejected receipt and exact
// redelivery. It intentionally accepts neither pending nor successful
// processed deliveries; successful lifecycle effects are Step 3 work.
//
// The fixed registration FK deliberately permits audit retention after that
// registration is inactive; admission of successful work still requires an
// active registration in EnqueueVerifiedPush.
func (r *WebhookJobRepository) RecordTerminalRejection(ctx context.Context, in TerminalRejectionInput) (int64, error) {
	if r == nil || r.pool == nil || !validTerminalRejection(in) {
		return 0, ErrWebhookUnauthorized
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin delivery record: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,action,body_digest,verification_state,processing_state,failure_class,installation_id,repository_id,github_owner_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(app_registration_id,delivery_id) DO NOTHING RETURNING id`, in.AppRegistrationID, in.Delivery.DeliveryID, in.Delivery.Event, in.Action, in.Delivery.BodyDigest[:], in.Delivery.Verification, in.Delivery.Processing, in.Delivery.Failure, in.InstallationID, in.RepositoryID, in.OwnerID).Scan(&id)
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit delivery record: %w", err)
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if isUniqueViolation(err) {
			return 0, ErrWebhookIntegrity
		}
		return 0, fmt.Errorf("insert webhook delivery: %w", err)
	}
	var digest []byte
	var event github.EventType
	var actionStored string
	var verification github.VerificationState
	var processing github.ProcessingState
	var failure github.Failure
	var install, repoID, owner *int64
	err = tx.QueryRow(ctx, `SELECT id,body_digest,event_type,action,verification_state,processing_state,failure_class,installation_id,repository_id,github_owner_id FROM webhook_deliveries WHERE app_registration_id=$1 AND delivery_id=$2 FOR SHARE`, in.AppRegistrationID, in.Delivery.DeliveryID).Scan(&id, &digest, &event, &actionStored, &verification, &processing, &failure, &install, &repoID, &owner)
	if err != nil || !bytes.Equal(digest, in.Delivery.BodyDigest[:]) || event != in.Delivery.Event || actionStored != in.Action || verification != in.Delivery.Verification || processing != in.Delivery.Processing || failure != in.Delivery.Failure || !equalOptionalID(install, in.InstallationID) || !equalOptionalID(repoID, in.RepositoryID) || !equalOptionalID(owner, in.OwnerID) {
		return 0, ErrWebhookIntegrity
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit delivery replay: %w", err)
	}
	return id, nil
}

func equalOptionalID(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func validTerminalRejection(in TerminalRejectionInput) bool {
	if in.AppRegistrationID != 1 || in.Delivery.Validate() != nil || in.Delivery.Processing != github.ProcessingRejected || in.Action != "" {
		return false
	}
	for _, value := range []*int64{in.InstallationID, in.RepositoryID, in.OwnerID} {
		if value != nil && *value <= 0 {
			return false
		}
	}
	return in.Delivery.Verification == github.VerificationRejected || (in.Delivery.Verification == github.VerificationVerified && (in.Delivery.Failure == github.FailureMalformed || in.Delivery.Failure == github.FailureUnsupported || in.Delivery.Failure == github.FailureUnauthorized))
}

func (r *WebhookJobRepository) EnqueueVerifiedPush(ctx context.Context, in WebhookEnqueueInput) (WebhookEnqueueResult, error) {
	if r == nil || r.pool == nil || r.deployments == nil || !validWebhookEnqueueInput(in) {
		return WebhookEnqueueResult{}, ErrWebhookUnauthorized
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return WebhookEnqueueResult{}, fmt.Errorf("begin webhook enqueue: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	target, err := lockWebhookAuthority(ctx, tx, in)
	if err != nil {
		return WebhookEnqueueResult{}, err
	}
	result, err := r.enqueueVerifiedPushTx(ctx, tx, in, target, true)
	if err != nil {
		return WebhookEnqueueResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WebhookEnqueueResult{}, fmt.Errorf("commit webhook enqueue: %w", err)
	}
	return result, nil
}

// enqueueVerifiedPushTx persists a fully constructed push after the caller has
// acquired the canonical authority locks. It never starts or commits a tx.
func (r *WebhookJobRepository) enqueueVerifiedPushTx(ctx context.Context, tx pgx.Tx, in WebhookEnqueueInput, target webhookTarget, strictReplay bool) (WebhookEnqueueResult, error) {
	if err := targetMatches(in, target); err != nil {
		return WebhookEnqueueResult{}, err
	}

	deliveryID, created, err := insertOrResolveDelivery(ctx, tx, in)
	if err != nil {
		return WebhookEnqueueResult{}, err
	}
	if !created {
		result, err := resultForDelivery(ctx, tx, deliveryID, in)
		if err != nil {
			return WebhookEnqueueResult{}, err
		}
		return result, nil
	}
	if err = r.inject("receipt"); err != nil {
		return WebhookEnqueueResult{}, err
	}

	intentID, inserted, err := insertOrResolveIntent(ctx, tx, deliveryID, in)
	if err != nil {
		return WebhookEnqueueResult{}, err
	}
	if err = r.inject("intent"); err != nil {
		return WebhookEnqueueResult{}, err
	}
	if !inserted {
		var result WebhookEnqueueResult
		if strictReplay {
			result, err = resultForIntent(ctx, tx, intentID, in)
		} else {
			result, err = resultForStoredIntent(ctx, tx, intentID, deliveryID)
		}
		if err != nil {
			return WebhookEnqueueResult{}, err
		}
		if err = markDeliveryProcessed(ctx, tx, deliveryID); err != nil {
			return WebhookEnqueueResult{}, err
		}
		return result, nil
	}

	var jobID int64
	if err = tx.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`, int16(in.Job.Version), in.Job.Kind, in.Job.OwnerID, in.Job.ProjectID, in.Job.EnvironmentID, in.Job.ServerID, in.Job.DeploymentID, in.Job.RepositoryID, in.Job.Ref, in.Job.CommitSHA, in.Job.Priority, in.Job.AvailableAt, in.Job.IdempotencyKey, in.Job.PayloadDigest[:], in.Job.State).Scan(&jobID); err != nil {
		if isUniqueViolation(err) {
			return WebhookEnqueueResult{}, ErrWebhookIntegrity
		}
		return WebhookEnqueueResult{}, fmt.Errorf("insert queued job: %w", err)
	}
	if err = r.inject("job"); err != nil {
		return WebhookEnqueueResult{}, err
	}
	if err = r.deployments.createTxWithHook(ctx, tx, in.Deployment, r.inject); err != nil {
		if isUniqueViolation(err) || errors.Is(err, ErrAlreadyExists) {
			return WebhookEnqueueResult{}, ErrWebhookIntegrity
		}
		return WebhookEnqueueResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES($1,1,'enqueued')`, jobID); err != nil {
		return WebhookEnqueueResult{}, fmt.Errorf("append enqueue event: %w", err)
	}
	if err = r.inject("job_event"); err != nil {
		return WebhookEnqueueResult{}, err
	}
	if err = markDeliveryProcessed(ctx, tx, deliveryID); err != nil {
		return WebhookEnqueueResult{}, err
	}
	if err = r.inject("processed"); err != nil {
		return WebhookEnqueueResult{}, err
	}
	return WebhookEnqueueResult{DeliveryID: deliveryID, DeploymentID: in.Deployment.Identity().DeploymentID, JobID: jobID}, nil
}

type webhookTarget struct{ ownerID, projectID, environmentID, serverID string }

// Lock order follows ADR 0004: App registration, installation, repository,
// then binding. Each query rechecks active authority before moving forward.
func lockWebhookAuthority(ctx context.Context, tx pgx.Tx, in WebhookEnqueueInput) (webhookTarget, error) {
	return lockWebhookAuthorityForPush(ctx, tx, in.AppRegistrationID, in.Push)
}

func lockWebhookAuthorityForPush(ctx context.Context, tx pgx.Tx, appRegistrationID int16, push github.Push) (webhookTarget, error) {
	var appID int16
	if err := tx.QueryRow(ctx, `SELECT id FROM github_app_registrations WHERE id=$1 AND active FOR SHARE`, appRegistrationID).Scan(&appID); err != nil || appID != appRegistrationID {
		return webhookTarget{}, ErrWebhookUnauthorized
	}
	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM github_installations WHERE installation_id=$1 AND account_id=$2 AND active AND NOT suspended FOR SHARE`, push.InstallationID, push.OwnerID).Scan(&ownerID); err != nil {
		return webhookTarget{}, ErrWebhookUnauthorized
	}
	var repositoryID int64
	if err := tx.QueryRow(ctx, `SELECT repository_id FROM github_repositories WHERE repository_id=$1 AND installation_id=$2 AND owner_id=$3 AND github_owner_id=$4 AND active FOR SHARE`, push.RepositoryID, push.InstallationID, ownerID, push.OwnerID).Scan(&repositoryID); err != nil || repositoryID != push.RepositoryID {
		return webhookTarget{}, ErrWebhookUnauthorized
	}
	var target webhookTarget
	if err := tx.QueryRow(ctx, `SELECT owner_id,project_id,environment_id,server_id FROM github_project_bindings WHERE owner_id=$1 AND repository_id=$2 AND installation_id=$3 AND active AND deploy_ref=$4 AND environment_id IS NOT NULL AND server_id IS NOT NULL FOR SHARE`, ownerID, push.RepositoryID, push.InstallationID, push.Ref).Scan(&target.ownerID, &target.projectID, &target.environmentID, &target.serverID); err != nil {
		return webhookTarget{}, ErrWebhookUnauthorized
	}
	return target, nil
}

func validWebhookEnqueueInput(in WebhookEnqueueInput) bool {
	if in.AppRegistrationID != 1 || in.Delivery.Validate() != nil || in.Delivery.Event != github.EventPush || in.Delivery.Verification != github.VerificationVerified || in.Delivery.Processing != github.ProcessingPending || in.Push.Validate() != nil || in.Job.Validate() != nil {
		return false
	}
	identity := in.Deployment.Identity()
	snapshot, err := in.Deployment.Snapshot()
	return err == nil && snapshot.Status == deployment.StatusQueued && snapshot.Revision == 1 && len(snapshot.Events) == 1 && snapshot.Events[0].From == "" && snapshot.Events[0].To == deployment.StatusQueued && snapshot.Events[0].At.Equal(snapshot.CreatedAt) && identity.DeploymentID == deployment.ID(in.Job.DeploymentID) && identity.CommitSHA == in.Push.CommitSHA && in.Job.RepositoryID == in.Push.RepositoryID && in.Job.Ref == in.Push.Ref && in.Job.CommitSHA == in.Push.CommitSHA
}

func targetMatches(in WebhookEnqueueInput, target webhookTarget) error {
	identity := in.Deployment.Identity()
	if in.Job.OwnerID != target.ownerID || in.Job.ProjectID != target.projectID || identity.ProjectID != target.projectID || in.Job.EnvironmentID != target.environmentID || identity.EnvironmentID != target.environmentID || in.Job.ServerID != target.serverID || identity.ServerID != target.serverID {
		return ErrWebhookUnauthorized
	}
	return nil
}

func insertOrResolveDelivery(ctx context.Context, tx pgx.Tx, in WebhookEnqueueInput) (int64, bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO webhook_deliveries(app_registration_id,delivery_id,event_type,body_digest,verification_state,processing_state,installation_id,repository_id,github_owner_id) VALUES($1,$2,$3,$4,'verified','pending',$5,$6,$7) ON CONFLICT (app_registration_id,delivery_id) DO NOTHING RETURNING id`, in.AppRegistrationID, in.Delivery.DeliveryID, in.Delivery.Event, in.Delivery.BodyDigest[:], in.Push.InstallationID, in.Push.RepositoryID, in.Push.OwnerID).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if isUniqueViolation(err) {
			return 0, false, ErrWebhookIntegrity
		}
		return 0, false, fmt.Errorf("insert webhook delivery: %w", err)
	}
	var digest []byte
	var event github.EventType
	var installationID, repositoryID, ownerID int64
	err = tx.QueryRow(ctx, `SELECT id,body_digest,event_type,installation_id,repository_id,github_owner_id FROM webhook_deliveries WHERE app_registration_id=$1 AND delivery_id=$2 FOR UPDATE`, in.AppRegistrationID, in.Delivery.DeliveryID).Scan(&id, &digest, &event, &installationID, &repositoryID, &ownerID)
	if err != nil {
		return 0, false, fmt.Errorf("resolve webhook delivery: %w", err)
	}
	if !bytes.Equal(digest, in.Delivery.BodyDigest[:]) || event != in.Delivery.Event || installationID != in.Push.InstallationID || repositoryID != in.Push.RepositoryID || ownerID != in.Push.OwnerID {
		return 0, false, ErrWebhookIntegrity
	}
	return id, false, nil
}

func insertOrResolveIntent(ctx context.Context, tx pgx.Tx, deliveryID int64, in WebhookEnqueueInput) (int64, bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO automatic_deployment_intents(delivery_id,binding_owner_id,binding_project_id,repository_id,full_ref,commit_sha,deployment_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (binding_owner_id,binding_project_id,repository_id,full_ref,commit_sha) DO NOTHING RETURNING id`, deliveryID, in.Job.OwnerID, in.Job.ProjectID, in.Push.RepositoryID, in.Push.Ref, in.Push.CommitSHA, in.Job.DeploymentID).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if isUniqueViolation(err) {
			return 0, false, ErrWebhookIntegrity
		}
		return 0, false, fmt.Errorf("insert automatic deployment intent: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT id FROM automatic_deployment_intents WHERE binding_owner_id=$1 AND binding_project_id=$2 AND repository_id=$3 AND full_ref=$4 AND commit_sha=$5 FOR UPDATE`, in.Job.OwnerID, in.Job.ProjectID, in.Push.RepositoryID, in.Push.Ref, in.Push.CommitSHA).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("resolve automatic deployment intent: %w", err)
	}
	return id, false, nil
}

func markDeliveryProcessed(ctx context.Context, tx pgx.Tx, deliveryID int64) error {
	tag, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET processing_state='processed',processed_at=clock_timestamp() WHERE id=$1 AND verification_state='verified' AND processing_state='pending'`, deliveryID)
	if err != nil {
		return fmt.Errorf("mark webhook delivery processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrWebhookIntegrity
	}
	return nil
}

func (r *WebhookJobRepository) inject(stage string) error {
	if r.failAfter == nil {
		return nil
	}
	return r.failAfter(stage)
}

func resultForDelivery(ctx context.Context, tx pgx.Tx, deliveryID int64, in WebhookEnqueueInput) (WebhookEnqueueResult, error) {
	var result WebhookEnqueueResult
	var deploymentID string
	err := tx.QueryRow(ctx, `SELECT i.deployment_id,j.id FROM automatic_deployment_intents i JOIN agent_jobs j ON j.deployment_id=i.deployment_id AND j.job_kind='deployment_execution' WHERE i.delivery_id=$1`, deliveryID).Scan(&deploymentID, &result.JobID)
	if err != nil || deploymentID == "" {
		return WebhookEnqueueResult{}, ErrWebhookIntegrity
	}
	result.DeliveryID = deliveryID
	result.DeploymentID = deployment.ID(deploymentID)
	return result, nil
}

func resultForStoredIntent(ctx context.Context, tx pgx.Tx, intentID, deliveryID int64) (WebhookEnqueueResult, error) {
	var result WebhookEnqueueResult
	var storedDeploymentID string
	err := tx.QueryRow(ctx, `SELECT i.deployment_id,j.id FROM automatic_deployment_intents i JOIN agent_jobs j ON j.deployment_id=i.deployment_id AND j.job_kind='deployment_execution' WHERE i.id=$1`, intentID).Scan(&storedDeploymentID, &result.JobID)
	if err != nil || storedDeploymentID == "" {
		return WebhookEnqueueResult{}, ErrWebhookIntegrity
	}
	result.DeliveryID = deliveryID
	result.DeploymentID = deployment.ID(storedDeploymentID)
	return result, nil
}

func resultForIntent(ctx context.Context, tx pgx.Tx, intentID int64, in WebhookEnqueueInput) (WebhookEnqueueResult, error) {
	var result WebhookEnqueueResult
	var deploymentID, projectID, environmentID, serverID, sha string
	var createdAt, eventAt time.Time
	var eventFrom, eventTo string
	var version int16
	var jobVersion int16
	var kind, ownerID, jobProject, jobEnvironment, jobServer, jobDeployment, ref, jobSHA, key, state string
	var repositoryID int64
	var priority int16
	var availableAt time.Time
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT i.delivery_id,i.deployment_id,j.id,d.project_id,d.environment_id,d.server_id,d.commit_sha,d.schema_version,d.created_at,e.from_status,e.to_status,e.occurred_at,j.schema_version,j.job_kind,j.owner_id,j.project_id,j.environment_id,j.server_id,j.deployment_id,j.repository_id,j.full_ref,j.commit_sha,j.priority,j.available_at,j.idempotency_key,j.payload_digest,j.state FROM automatic_deployment_intents i JOIN deployments d ON d.id=i.deployment_id JOIN deployment_events e ON e.deployment_id=d.id AND e.revision=1 JOIN agent_jobs j ON j.deployment_id=i.deployment_id AND j.job_kind='deployment_execution' WHERE i.id=$1`, intentID).Scan(&result.DeliveryID, &deploymentID, &result.JobID, &projectID, &environmentID, &serverID, &sha, &version, &createdAt, &eventFrom, &eventTo, &eventAt, &jobVersion, &kind, &ownerID, &jobProject, &jobEnvironment, &jobServer, &jobDeployment, &repositoryID, &ref, &jobSHA, &priority, &availableAt, &key, &digest, &state)
	if err != nil {
		return WebhookEnqueueResult{}, ErrWebhookIntegrity
	}
	snapshot, snapErr := in.Deployment.Snapshot()
	if snapErr != nil || deploymentID != string(in.Deployment.Identity().DeploymentID) || projectID != in.Deployment.Identity().ProjectID || environmentID != in.Deployment.Identity().EnvironmentID || serverID != in.Deployment.Identity().ServerID || sha != in.Deployment.Identity().CommitSHA || version != int16(snapshot.Version) || !createdAt.Equal(snapshot.CreatedAt) || eventFrom != "" || eventTo != string(deployment.StatusQueued) || !eventAt.Equal(snapshot.Events[0].At) || jobVersion != int16(in.Job.Version) || kind != string(in.Job.Kind) || ownerID != in.Job.OwnerID || jobProject != in.Job.ProjectID || jobEnvironment != in.Job.EnvironmentID || jobServer != in.Job.ServerID || jobDeployment != in.Job.DeploymentID || repositoryID != in.Job.RepositoryID || ref != in.Job.Ref || jobSHA != in.Job.CommitSHA || priority != in.Job.Priority || !availableAt.Equal(in.Job.AvailableAt) || key != in.Job.IdempotencyKey || !bytes.Equal(digest, in.Job.PayloadDigest[:]) || state != string(in.Job.State) {
		return WebhookEnqueueResult{}, ErrWebhookIntegrity
	}
	result.DeploymentID = deployment.ID(deploymentID)
	return result, nil
}
