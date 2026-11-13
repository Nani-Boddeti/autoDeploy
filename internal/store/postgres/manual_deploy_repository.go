package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/github"
	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5"
)

// ManualDeployInput is everything an owner supplies. The repository, ref,
// environment and server always come from the project's binding row. The three
// Expected* fields are REQUIRED and are the installation, repository and ref
// the commit was resolved against (ManualDeployTarget): a binding that changed
// since is refused with ErrManualDeployConflict rather than deploying a commit
// verified for one repository under another.
type ManualDeployInput struct {
	ProjectID              string
	CommitSHA              string
	RequestNonce           string
	ExpectedInstallationID int64
	ExpectedRepositoryID   int64
	ExpectedRef            string
}

// ManualDeployResult identifies the durable deployment and job. Replayed is
// true when the nonce had already been committed.
type ManualDeployResult struct {
	DeploymentID deployment.ID
	JobID        int64
	Replayed     bool
}

func validManualNonce(nonce string) bool {
	if len(nonce) < 16 || len(nonce) > 128 {
		return false
	}
	for _, c := range nonce {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// manualIDs mirrors webhookIDs under its own domain so manual and push
// identities can never collide.
func manualIDs(target webhookTarget, repositoryID int64, ref, sha, nonce string) (deployment.ID, string) {
	h := sha256.New()
	for _, value := range []string{"autodeploy:manual:v1", target.ownerID, target.projectID, target.environmentID, target.serverID, strconv.FormatInt(repositoryID, 10), ref, sha, nonce} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	seed := hex.EncodeToString(h.Sum(nil))
	return deployment.ID("mnd_" + seed[:40]), "mnj_" + seed[40:]
}

// ManualDeployTarget is the plain, owner-scoped read the web layer uses to
// resolve a commit before EnqueueManualDeploy. It takes no row locks; the
// enqueue transaction revalidates everything under FOR SHARE.
func (r *WebhookJobRepository) ManualDeployTarget(ctx context.Context, principal auth.Principal, projectID string) (installationID, repositoryID int64, fullRef string, err error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(projectID) {
		return 0, 0, "", ErrManualDeployUnauthorized
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, 0, "", fmt.Errorf("begin manual deploy target: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = requireLiveSessionRow(ctx, tx, principal); err != nil {
		return 0, 0, "", manualDenied(err, "manual deploy target session")
	}
	if err = readManualBinding(ctx, tx, principal.OwnerID, projectID, true).Scan(&installationID, new(int64), &repositoryID, &fullRef); err != nil {
		return 0, 0, "", manualDenied(err, "manual deploy target binding")
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, 0, "", fmt.Errorf("commit manual deploy target: %w", err)
	}
	return installationID, repositoryID, fullRef, nil
}

// manualLockKey is the text hashed into the manual-only advisory lock that
// serialises manual deploys of one commit to one environment.
func manualLockKey(environmentID, commitSHA string) string {
	return "autodeploy:manual:" + environmentID + ":" + commitSHA
}

// manualDenied maps only "no such row" to ErrManualDeployUnauthorized; any
// other database error is wrapped so the caller reports a retryable failure.
func manualDenied(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrManualDeployUnauthorized
	}
	return fmt.Errorf("%s: %w", what, err)
}

// readManualBinding reads the owner's bound project and its installation
// account. requireActive additionally demands the whole chain be active (the
// plain read); the enqueue tx passes false and re-proves it under locks.
func readManualBinding(ctx context.Context, tx pgx.Tx, ownerID, projectID string, requireActive bool) pgx.Row {
	query := `SELECT b.installation_id,i.account_id,b.repository_id,b.deploy_ref FROM github_project_bindings b JOIN github_installations i ON i.installation_id=b.installation_id AND i.owner_id=b.owner_id WHERE b.owner_id=$1 AND b.project_id=$2 AND b.environment_id IS NOT NULL AND b.server_id IS NOT NULL`
	if requireActive {
		query += ` AND b.active AND i.active AND NOT i.suspended AND EXISTS (SELECT 1 FROM github_app_registrations a WHERE a.id=1 AND a.active) AND EXISTS (SELECT 1 FROM github_repositories r WHERE r.repository_id=b.repository_id AND r.installation_id=b.installation_id AND r.owner_id=b.owner_id AND r.active)`
	}
	return tx.QueryRow(ctx, query, ownerID, projectID)
}

// EnqueueManualDeploy creates one deployment for an owner-initiated deploy of
// commitSHA on the project's bound ref. Lock order: app registration (SHARE),
// admin session (FOR UPDATE, the registration -> session -> installation order
// DisableInstallation/BindRepository use), installation, repository, binding
// (SHARE), then a manual-only advisory lock on (environment, sha). All in one
// READ COMMITTED tx.
func (r *WebhookJobRepository) EnqueueManualDeploy(ctx context.Context, principal auth.Principal, in ManualDeployInput) (ManualDeployResult, error) {
	if r == nil || r.pool == nil || r.deployments == nil || principal.Validate() != nil {
		return ManualDeployResult{}, ErrManualDeployUnauthorized
	}
	if !github.ValidOpaqueID(in.ProjectID) || !github.ValidCommitSHA(in.CommitSHA) || !validManualNonce(in.RequestNonce) || in.ExpectedInstallationID <= 0 || in.ExpectedRepositoryID <= 0 || !github.ValidBranchRef(in.ExpectedRef) {
		return ManualDeployResult{}, ErrManualDeployInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ManualDeployResult{}, fmt.Errorf("begin manual deploy: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Unlocked read only to learn which chain to lock (the account id is not
	// part of the expected values). The expected values are compared to it now,
	// and the locking queries below re-prove each of them against the locked rows
	// (a concurrent rebind between this read and the locks fails closed):
	//   installation id -> github_installations lock `installation_id=$1`, and the
	//                      binding lock `installation_id=$3`
	//   repository id   -> github_repositories lock `repository_id=$1`, and the
	//                      binding lock `repository_id=$2`
	//   ref             -> binding lock `deploy_ref=$4`
	//   project/owner   -> binding lock `owner_id=$1`, `project_id=$5`
	var installationID, accountID, repositoryID int64
	var ref string
	if err = readManualBinding(ctx, tx, principal.OwnerID, in.ProjectID, false).Scan(&installationID, &accountID, &repositoryID, &ref); err != nil {
		return ManualDeployResult{}, manualDenied(err, "manual deploy binding read")
	}
	if installationID != in.ExpectedInstallationID || repositoryID != in.ExpectedRepositoryID || ref != in.ExpectedRef {
		return ManualDeployResult{}, ErrManualDeployConflict
	}
	target, err := lockGitHubAuthorityChain(ctx, tx, func() error {
		if serr := lockAdministratorSessionRow(ctx, tx, principal); serr != nil {
			return manualDenied(serr, "manual deploy session lock")
		}
		return nil
	}, githubAuthorityKey{appRegistrationID: 1, installationID: installationID, accountID: accountID, repositoryID: repositoryID, ref: ref, ownerID: principal.OwnerID, projectID: in.ProjectID, strictErrors: true}, ErrManualDeployUnauthorized)
	if err != nil {
		return ManualDeployResult{}, err
	}
	// Serializes concurrent manual deploys of one commit to one environment so
	// the in-flight check below cannot be raced. Taken after every row lock of
	// the chain and held by no other path, so it cannot form a cycle.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, manualLockKey(target.environmentID, in.CommitSHA)); err != nil {
		return ManualDeployResult{}, fmt.Errorf("manual deploy serialize: %w", err)
	}
	want := manualPayload{projectID: in.ProjectID, repositoryID: repositoryID, ref: ref, sha: in.CommitSHA}
	if result, found, rerr := resultForManualNonce(ctx, tx, principal.OwnerID, in.RequestNonce, want); rerr != nil || found {
		if rerr != nil {
			return ManualDeployResult{}, rerr
		}
		if err = tx.Commit(ctx); err != nil {
			return ManualDeployResult{}, fmt.Errorf("commit manual deploy replay: %w", err)
		}
		return result, nil
	}
	// In flight means queued or in progress only (deployment.ActiveStatuses): a
	// deliberate redeploy of a healthy or terminal commit is allowed. Scoped to
	// the owner through the deployment's execution job, so another owner's rows
	// are never observed. A concurrent webhook push of the same commit is not
	// serialised by this lock, so a push and a manual deploy of one commit may
	// both create deployments; environment slots serialise their execution and
	// this is accepted.
	active := make([]string, 0, 8)
	for _, status := range deployment.ActiveStatuses() {
		active = append(active, string(status))
	}
	var inFlight bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deployments d JOIN agent_jobs j ON j.deployment_id=d.id AND j.job_kind='deployment_execution' WHERE j.owner_id=$1 AND d.environment_id=$2 AND d.commit_sha=$3 AND d.status = ANY($4))`, principal.OwnerID, target.environmentID, in.CommitSHA, active).Scan(&inFlight); err != nil {
		return ManualDeployResult{}, fmt.Errorf("manual deploy in-flight check: %w", err)
	}
	if inFlight {
		return ManualDeployResult{}, ErrManualDeployInFlight
	}

	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return ManualDeployResult{}, fmt.Errorf("manual deploy canonical time: %w", err)
	}
	deploymentID, jobKey := manualIDs(target, repositoryID, ref, in.CommitSHA, in.RequestNonce)
	d, err := deployment.New(deployment.Identity{DeploymentID: deploymentID, ProjectID: target.projectID, EnvironmentID: target.environmentID, ServerID: target.serverID, CommitSHA: in.CommitSHA}, now)
	if err != nil {
		return ManualDeployResult{}, fmt.Errorf("manual deploy build deployment: %w", err)
	}
	j, err := queue.NewDeploymentExecutionJob(target.ownerID, target.projectID, target.environmentID, target.serverID, string(deploymentID), repositoryID, ref, in.CommitSHA, jobKey, 0, now)
	if err != nil {
		return ManualDeployResult{}, fmt.Errorf("manual deploy build job: %w", err)
	}
	var intentID int64
	err = tx.QueryRow(ctx, `INSERT INTO manual_deployment_intents(owner_id,actor_user_id,binding_project_id,repository_id,full_ref,commit_sha,request_nonce,deployment_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (owner_id,request_nonce) DO NOTHING RETURNING id`, principal.OwnerID, principal.UserID, in.ProjectID, repositoryID, ref, in.CommitSHA, in.RequestNonce, string(deploymentID)).Scan(&intentID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent request committed the same nonce after our replay read.
		result, found, rerr := resultForManualNonce(ctx, tx, principal.OwnerID, in.RequestNonce, want)
		if rerr != nil {
			return ManualDeployResult{}, rerr
		}
		if !found {
			return ManualDeployResult{}, ErrManualDeployConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return ManualDeployResult{}, fmt.Errorf("commit manual deploy replay: %w", err)
		}
		return result, nil
	}
	if err != nil {
		if isUniqueViolation(err) {
			return ManualDeployResult{}, ErrManualDeployConflict
		}
		return ManualDeployResult{}, fmt.Errorf("insert manual deployment intent: %w", err)
	}
	if err = r.inject("manual_intent"); err != nil {
		return ManualDeployResult{}, err
	}
	jobID, err := r.insertQueuedDeploymentTx(ctx, tx, d, j, ErrManualDeployConflict)
	if err != nil {
		return ManualDeployResult{}, err
	}
	if err = r.inject("manual_done"); err != nil {
		return ManualDeployResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ManualDeployResult{}, fmt.Errorf("commit manual deploy: %w", err)
	}
	return ManualDeployResult{DeploymentID: deploymentID, JobID: jobID}, nil
}

type manualPayload struct {
	projectID    string
	repositoryID int64
	ref, sha     string
}

// resultForManualNonce returns the committed result for (owner, nonce) when the
// stored payload equals want, ErrManualDeployConflict when it differs, and
// found=false when the nonce is unused.
func resultForManualNonce(ctx context.Context, tx pgx.Tx, ownerID, nonce string, want manualPayload) (ManualDeployResult, bool, error) {
	var got manualPayload
	var deploymentID string
	var jobID int64
	err := tx.QueryRow(ctx, `SELECT i.binding_project_id,i.repository_id,i.full_ref,i.commit_sha,i.deployment_id,j.id FROM manual_deployment_intents i JOIN agent_jobs j ON j.deployment_id=i.deployment_id AND j.job_kind='deployment_execution' WHERE i.owner_id=$1 AND i.request_nonce=$2`, ownerID, nonce).Scan(&got.projectID, &got.repositoryID, &got.ref, &got.sha, &deploymentID, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualDeployResult{}, false, nil
	}
	if err != nil {
		return ManualDeployResult{}, false, fmt.Errorf("resolve manual deploy intent: %w", err)
	}
	if got != want {
		return ManualDeployResult{}, false, ErrManualDeployConflict
	}
	return ManualDeployResult{DeploymentID: deployment.ID(deploymentID), JobID: jobID, Replayed: true}, true, nil
}
