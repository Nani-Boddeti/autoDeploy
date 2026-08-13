package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func checkoutID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:])), nil
}

var (
	ErrSetupStateInvalid = errors.New("GitHub setup state is invalid")
	ErrHandoffInvalid    = errors.New("GitHub setup handoff is invalid")
	ErrGitHubDenied      = errors.New("GitHub authorization denied")
)

type GitHubRepository struct{ pool *pgxpool.Pool }

func activeRegistration(ctx context.Context, tx interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var id int
	if err := tx.QueryRow(ctx, `SELECT id FROM github_app_registrations WHERE id=1 AND active FOR UPDATE`).Scan(&id); err != nil || id != 1 {
		return ErrGitHubDenied
	}
	return nil
}

func NewGitHubRepository(pool *pgxpool.Pool) *GitHubRepository { return &GitHubRepository{pool: pool} }
func (r *GitHubRepository) Ping(ctx context.Context) error {
	if r == nil || r.pool == nil {
		return errors.New("GitHub repository unavailable")
	}
	return r.pool.Ping(ctx)
}

// CaptureCheckoutBinding reads the complete active authority chain and returns
// the binding revision that must be revalidated after token issuance.
func (r *GitHubRepository) CaptureCheckoutBinding(ctx context.Context, ownerID, projectID string) (githubapp.CheckoutBinding, error) {
	if ownerID == "" || len(ownerID) > 128 || projectID == "" || len(projectID) > 128 {
		return githubapp.CheckoutBinding{}, ErrGitHubDenied
	}
	var binding githubapp.CheckoutBinding
	err := r.pool.QueryRow(ctx, `SELECT b.owner_id,b.project_id,b.installation_id,b.repository_id,b.authorization_revision,r.authorization_revision,i.authorization_revision,r.clone_url
		FROM github_project_bindings b
		JOIN github_repositories r ON r.repository_id=b.repository_id AND r.owner_id=b.owner_id
		JOIN github_installations i ON i.installation_id=b.installation_id AND i.owner_id=b.owner_id
		JOIN github_app_registrations a ON a.id=1 AND a.active
		WHERE b.owner_id=$1 AND b.project_id=$2 AND b.active AND r.active AND i.active AND NOT i.suspended`, ownerID, projectID).
		Scan(&binding.OwnerID, &binding.ProjectID, &binding.InstallationID, &binding.RepositoryID, &binding.BindingRevision, &binding.RepositoryRevision, &binding.InstallationRevision, &binding.CloneURL)
	if err != nil {
		return githubapp.CheckoutBinding{}, ErrGitHubDenied
	}
	return binding, nil
}

// RevalidateCheckoutBinding locks the same binding and its upstream authority
// after a checkout token is minted. Any authority/revision change denies use.
func (r *GitHubRepository) RevalidateCheckoutBinding(ctx context.Context, binding githubapp.CheckoutBinding) error {
	if binding.OwnerID == "" || binding.ProjectID == "" || binding.InstallationID <= 0 || binding.RepositoryID <= 0 || binding.BindingRevision <= 0 || binding.RepositoryRevision <= 0 || binding.InstallationRevision <= 0 {
		return ErrGitHubDenied
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin checkout revalidation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var bindingRevision, repositoryRevision, installationRevision int64
	err = tx.QueryRow(ctx, `SELECT b.authorization_revision,r.authorization_revision,i.authorization_revision
		FROM github_project_bindings b
		JOIN github_repositories r ON r.repository_id=b.repository_id AND r.owner_id=b.owner_id
		JOIN github_installations i ON i.installation_id=b.installation_id AND i.owner_id=b.owner_id
		JOIN github_app_registrations a ON a.id=1 AND a.active
		WHERE b.owner_id=$1 AND b.project_id=$2 AND b.installation_id=$3 AND b.repository_id=$4
		AND b.active AND r.active AND i.active AND NOT i.suspended
		FOR UPDATE OF b,r,i,a`, binding.OwnerID, binding.ProjectID, binding.InstallationID, binding.RepositoryID).Scan(&bindingRevision, &repositoryRevision, &installationRevision)
	if err != nil || bindingRevision != binding.BindingRevision || repositoryRevision != binding.RepositoryRevision || installationRevision != binding.InstallationRevision {
		return ErrGitHubDenied
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit checkout revalidation: %w", err)
	}
	return nil
}

func (r *GitHubRepository) ReserveCheckout(ctx context.Context, owner, project, sha string) (githubapp.CheckoutReservation, error) {
	if len(sha) != 40 {
		return githubapp.CheckoutReservation{}, ErrGitHubDenied
	}
	id, e := checkoutID()
	if e != nil {
		return githubapp.CheckoutReservation{}, ErrGitHubDenied
	}
	tx, e := r.pool.Begin(ctx)
	if e != nil {
		return githubapp.CheckoutReservation{}, e
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// The per-binding row is the lock root; upstream authority follows it.
	var b githubapp.CheckoutBinding
	e = tx.QueryRow(ctx, `SELECT b.owner_id,b.project_id,b.installation_id,b.repository_id,b.authorization_revision,r.authorization_revision,i.authorization_revision,r.clone_url FROM github_project_bindings b JOIN github_repositories r ON r.repository_id=b.repository_id AND r.owner_id=b.owner_id JOIN github_installations i ON i.installation_id=b.installation_id AND i.owner_id=b.owner_id JOIN github_app_registrations a ON a.id=1 AND a.active WHERE b.owner_id=$1 AND b.project_id=$2 AND b.active AND r.active AND i.active AND NOT i.suspended FOR UPDATE OF b,r,i,a`, owner, project).Scan(&b.OwnerID, &b.ProjectID, &b.InstallationID, &b.RepositoryID, &b.BindingRevision, &b.RepositoryRevision, &b.InstallationRevision, &b.CloneURL)
	if e != nil || !b.CloneURLValid() {
		return githubapp.CheckoutReservation{}, ErrGitHubDenied
	}
	if _, e = tx.Exec(ctx, `UPDATE github_checkout_reservations SET state='expired' WHERE owner_id=$1 AND project_id=$2 AND state IN ('reserved','minting','active') AND expires_at<=clock_timestamp()`, owner, project); e != nil {
		return githubapp.CheckoutReservation{}, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO github_checkout_reservations(id,owner_id,project_id,installation_id,repository_id,clone_url,commit_sha,binding_revision,repository_revision,installation_revision,state,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reserved',clock_timestamp()+interval '10 minutes')`, id, owner, project, b.InstallationID, b.RepositoryID, b.CloneURL, sha, b.BindingRevision, b.RepositoryRevision, b.InstallationRevision)
	if e != nil {
		return githubapp.CheckoutReservation{}, ErrGitHubDenied
	}
	if e = tx.Commit(ctx); e != nil {
		return githubapp.CheckoutReservation{}, e
	}
	b.CommitSHA = sha
	b.ReservationID = id
	return githubapp.CheckoutReservation{CheckoutBinding: b, ID: id, ExpiresAt: time.Now().Add(10 * time.Minute), State: "reserved"}, nil
}

func (r *GitHubRepository) BeginMintAttempt(ctx context.Context, id string) (githubapp.CheckoutBinding, error) {
	tx, e := r.pool.Begin(ctx)
	if e != nil {
		return githubapp.CheckoutBinding{}, e
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var b githubapp.CheckoutBinding
	var attempts int16
	e = tx.QueryRow(ctx, `SELECT owner_id,project_id,installation_id,repository_id,binding_revision,repository_revision,installation_revision,clone_url,commit_sha,attempts FROM github_checkout_reservations WHERE id=$1 AND state='reserved' AND expires_at>clock_timestamp() FOR UPDATE`, id).Scan(&b.OwnerID, &b.ProjectID, &b.InstallationID, &b.RepositoryID, &b.BindingRevision, &b.RepositoryRevision, &b.InstallationRevision, &b.CloneURL, &b.CommitSHA, &attempts)
	if e != nil || attempts >= 1 {
		return githubapp.CheckoutBinding{}, ErrGitHubDenied
	}
	b.ReservationID = id
	if e = r.revalidateTx(ctx, tx, b); e != nil {
		return githubapp.CheckoutBinding{}, ErrGitHubDenied
	}
	if _, e = tx.Exec(ctx, `UPDATE github_checkout_reservations SET state='minting',attempts=attempts+1 WHERE id=$1`, id); e != nil {
		return githubapp.CheckoutBinding{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return githubapp.CheckoutBinding{}, e
	}
	return b, nil
}
func (r *GitHubRepository) ActivateMintAttempt(ctx context.Context, id string, b githubapp.CheckoutBinding) error {
	tx, e := r.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if e = r.revalidateTx(ctx, tx, b); e != nil {
		return ErrGitHubDenied
	}
	tag, e := tx.Exec(ctx, `UPDATE github_checkout_reservations SET state='active' WHERE id=$1 AND state='minting' AND expires_at>clock_timestamp() AND binding_revision=$2 AND repository_revision=$3 AND installation_revision=$4`, id, b.BindingRevision, b.RepositoryRevision, b.InstallationRevision)
	if e != nil || tag.RowsAffected() != 1 {
		return ErrGitHubDenied
	}
	return tx.Commit(ctx)
}

// StartCheckout is the last durable authorization gate, immediately before a
// token is delivered to Git. It moves active to running under the same
// authority locks used by activation.
func (r *GitHubRepository) StartCheckout(ctx context.Context, id string, b githubapp.CheckoutBinding) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM github_checkout_reservations WHERE id=$1 AND expires_at>clock_timestamp() FOR UPDATE`, id).Scan(&state)
	if err != nil || state != "active" || r.revalidateTx(ctx, tx, b) != nil {
		return ErrGitHubDenied
	}
	tag, err := tx.Exec(ctx, `UPDATE github_checkout_reservations SET state='running' WHERE id=$1 AND state='active' AND expires_at>clock_timestamp()`, id)
	if err != nil || tag.RowsAffected() != 1 {
		return ErrGitHubDenied
	}
	return tx.Commit(ctx)
}
func (r *GitHubRepository) CloseCheckout(ctx context.Context, id string) error {
	tag, e := r.pool.Exec(ctx, `UPDATE github_checkout_reservations SET state='consumed',consumed_at=clock_timestamp() WHERE id=$1 AND state IN ('reserved','minting','active','running')`, id)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var state string
	if e = r.pool.QueryRow(ctx, `SELECT state FROM github_checkout_reservations WHERE id=$1`, id).Scan(&state); e != nil || state != "consumed" {
		return ErrGitHubDenied
	}
	return nil
}
func (r *GitHubRepository) revalidateTx(ctx context.Context, tx pgx.Tx, b githubapp.CheckoutBinding) error {
	var x, y, z int64
	e := tx.QueryRow(ctx, `SELECT b.authorization_revision,r.authorization_revision,i.authorization_revision FROM github_project_bindings b JOIN github_repositories r ON r.repository_id=b.repository_id AND r.owner_id=b.owner_id JOIN github_installations i ON i.installation_id=b.installation_id AND i.owner_id=b.owner_id JOIN github_app_registrations a ON a.id=1 AND a.active WHERE b.owner_id=$1 AND b.project_id=$2 AND b.installation_id=$3 AND b.repository_id=$4 AND b.active AND r.active AND i.active AND NOT i.suspended FOR UPDATE OF b,r,i,a`, b.OwnerID, b.ProjectID, b.InstallationID, b.RepositoryID).Scan(&x, &y, &z)
	if e != nil || x != b.BindingRevision || y != b.RepositoryRevision || z != b.InstallationRevision {
		return ErrGitHubDenied
	}
	return nil
}

func (r *GitHubRepository) EnsureAppRegistration(ctx context.Context, identity githubapp.AppIdentity, owner githubapp.Owner) error {
	if !identity.Valid() || !owner.Valid() {
		return ErrGitHubDenied
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO github_app_registrations(id,app_id,client_id,slug,owner_account_id,owner_type) VALUES(1,$1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET updated_at=clock_timestamp() WHERE github_app_registrations.app_id=EXCLUDED.app_id AND github_app_registrations.client_id=EXCLUDED.client_id AND github_app_registrations.slug=EXCLUDED.slug AND github_app_registrations.owner_account_id=EXCLUDED.owner_account_id AND github_app_registrations.owner_type=EXCLUDED.owner_type AND github_app_registrations.active`, identity.AppID, identity.ClientID, identity.Slug, owner.AccountID, owner.Type)
	if err != nil {
		return fmt.Errorf("persist GitHub App registration: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrGitHubDenied
	}
	return nil
}

func (r *GitHubRepository) CreateSetupState(ctx context.Context, in githubapp.SetupStateInput) error {
	if in.Principal.Validate() != nil || in.Digest == ([32]byte{}) {
		return ErrGitHubDenied
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin setup state: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = activeRegistration(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(731492845631694124)`); err != nil {
		return fmt.Errorf("lock setup admission: %w", err)
	}
	// A pending handoff retains a foreign key to its setup state. Remove those
	// short-lived dependents before pruning expired/consumed states so cleanup
	// cannot be blocked by an abandoned callback.
	if _, err = tx.Exec(ctx, `DELETE FROM github_pending_handoffs h USING github_setup_states s WHERE h.state_digest=s.digest AND (s.expires_at<=clock_timestamp() OR (s.consumed_at IS NOT NULL AND s.consumed_at<=clock_timestamp()-interval '10 minutes'))`); err != nil {
		return fmt.Errorf("cleanup pending handoffs: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM github_setup_states WHERE expires_at<=clock_timestamp() OR (consumed_at IS NOT NULL AND consumed_at<=clock_timestamp()-interval '10 minutes')`); err != nil {
		return fmt.Errorf("cleanup setup states: %w", err)
	}
	var active, cap int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM github_setup_states WHERE consumed_at IS NULL AND expires_at>clock_timestamp()`).Scan(&active); err != nil {
		return fmt.Errorf("count setup states: %w", err)
	}
	if err = tx.QueryRow(ctx, `SELECT max_active FROM github_setup_state_limits WHERE id=1 FOR UPDATE`).Scan(&cap); err != nil || active >= cap {
		return ErrGitHubDenied
	}
	var locked int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp() FOR UPDATE OF s,u`, in.Principal.SessionID, in.Principal.UserID, in.Principal.OwnerID, int64(in.Principal.AuthRevision)).Scan(&locked); err != nil || locked != 1 {
		return ErrGitHubDenied
	}
	// At most one active capability per session; replacing it prevents a tab from
	// creating unbounded callback authority.
	if _, err = tx.Exec(ctx, `DELETE FROM github_setup_states WHERE session_id=$1 AND consumed_at IS NULL`, in.Principal.SessionID); err != nil {
		return fmt.Errorf("replace setup state: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_setup_states(digest,owner_id,user_id,session_id,auth_revision,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '10 minutes')`, in.Digest[:], in.Principal.OwnerID, in.Principal.UserID, in.Principal.SessionID, int64(in.Principal.AuthRevision)); err != nil {
		return fmt.Errorf("insert setup state: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result) VALUES($1,$2,$3,'setup_started','success')`, auditID("setup", in.Digest), in.Principal.OwnerID, in.Principal.UserID); err != nil {
		return fmt.Errorf("audit setup start: %w", err)
	}
	return tx.Commit(ctx)
}

func (r *GitHubRepository) PendingInstallation(ctx context.Context, digest [32]byte, principal auth.Principal) (githubapp.Installation, error) {
	if digest == ([32]byte{}) || principal.Validate() != nil {
		return githubapp.Installation{}, ErrHandoffInvalid
	}
	var installation githubapp.Installation
	err := r.pool.QueryRow(ctx, `SELECT installation_id,account_id,account_type FROM github_pending_handoffs WHERE digest=$1 AND owner_id=$2 AND user_id=$3 AND session_id=$4 AND auth_revision=$5 AND consumed_at IS NULL AND expires_at>clock_timestamp()`, digest[:], principal.OwnerID, principal.UserID, principal.SessionID, int64(principal.AuthRevision)).Scan(&installation.ID, &installation.Account.AccountID, &installation.Account.Type)
	if err != nil {
		return githubapp.Installation{}, ErrHandoffInvalid
	}
	return installation, nil
}

// BeginCallback obtains a short lease for provider work without consuming the
// state. A stale lease can be retried after one minute, and CancelCallback
// releases a lease after a provider failure.
func (r *GitHubRepository) BeginCallback(ctx context.Context, digest [32]byte) error {
	if digest == ([32]byte{}) {
		return ErrSetupStateInvalid
	}
	tag, err := r.pool.Exec(ctx, `UPDATE github_setup_states s SET recoverable_failure_at=clock_timestamp() FROM admin_sessions a, users u WHERE s.digest=$1 AND s.consumed_at IS NULL AND s.expires_at>clock_timestamp() AND (s.recoverable_failure_at IS NULL OR s.recoverable_failure_at<=clock_timestamp()-interval '1 minute') AND a.id=s.session_id AND a.user_id=s.user_id AND a.owner_id=s.owner_id AND a.auth_revision=s.auth_revision AND a.revoked_at IS NULL AND a.idle_expires_at>clock_timestamp() AND a.absolute_expires_at>clock_timestamp() AND u.id=s.user_id AND u.owner_id=s.owner_id AND u.auth_revision=s.auth_revision AND u.status='active'`, digest[:])
	if err != nil {
		return fmt.Errorf("begin callback: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrSetupStateInvalid
	}
	return nil
}

func (r *GitHubRepository) CancelCallback(ctx context.Context, digest [32]byte) error {
	if digest == ([32]byte{}) {
		return ErrSetupStateInvalid
	}
	_, err := r.pool.Exec(ctx, `UPDATE github_setup_states SET recoverable_failure_at=NULL WHERE digest=$1 AND consumed_at IS NULL`, digest[:])
	if err != nil {
		return fmt.Errorf("cancel callback: %w", err)
	}
	return nil
}

// ReserveCallback consumes the setup capability only after the provider lookup
// has succeeded. It creates no installation authorization.
func (r *GitHubRepository) ReserveCallback(ctx context.Context, state, handoff [32]byte, installation githubapp.Installation) error {
	if state == ([32]byte{}) || handoff == ([32]byte{}) || installation.ID <= 0 || !installation.Account.Valid() || installation.Suspended {
		return ErrSetupStateInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin callback: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = activeRegistration(ctx, tx); err != nil {
		return err
	}
	var p auth.Principal
	err = tx.QueryRow(ctx, `SELECT s.owner_id,s.user_id,s.session_id,s.auth_revision FROM github_setup_states s JOIN admin_sessions a ON a.id=s.session_id AND a.user_id=s.user_id AND a.owner_id=s.owner_id AND a.auth_revision=s.auth_revision JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id AND u.auth_revision=s.auth_revision WHERE s.digest=$1 AND s.consumed_at IS NULL AND s.expires_at>clock_timestamp() AND a.revoked_at IS NULL AND a.idle_expires_at>clock_timestamp() AND a.absolute_expires_at>clock_timestamp() AND u.status='active' FOR UPDATE OF s,a,u`, state[:]).Scan(&p.OwnerID, &p.UserID, &p.SessionID, &p.AuthRevision)
	if err != nil {
		return ErrSetupStateInvalid
	}
	if _, err = tx.Exec(ctx, `UPDATE github_setup_states SET consumed_at=clock_timestamp() WHERE digest=$1 AND consumed_at IS NULL`, state[:]); err != nil {
		return fmt.Errorf("consume setup state: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_pending_handoffs(digest,state_digest,owner_id,user_id,session_id,auth_revision,installation_id,account_id,account_type,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()+interval '10 minutes')`, handoff[:], state[:], p.OwnerID, p.UserID, p.SessionID, int64(p.AuthRevision), installation.ID, installation.Account.AccountID, installation.Account.Type); err != nil {
		return fmt.Errorf("insert handoff: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,installation_id) VALUES($1,$2,$3,'callback_verified','success',$4)`, auditID("callback", handoff), p.OwnerID, p.UserID, installation.ID); err != nil {
		return fmt.Errorf("audit callback: %w", err)
	}
	return tx.Commit(ctx)
}

func (r *GitHubRepository) CompleteHandoff(ctx context.Context, digest [32]byte, principal auth.Principal, installation githubapp.Installation, repositories []githubapp.Repository) error {
	if principal.Validate() != nil || digest == ([32]byte{}) || installation.ID <= 0 || installation.Suspended || !installation.Account.Valid() || len(repositories) > 1000 {
		return ErrHandoffInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete handoff: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = activeRegistration(ctx, tx); err != nil {
		return err
	}
	var owner, user, session string
	var revision int64
	var installationID, accountID int64
	var accountType string
	err = tx.QueryRow(ctx, `SELECT owner_id,user_id,session_id,auth_revision,installation_id,account_id,account_type FROM github_pending_handoffs WHERE digest=$1 AND consumed_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, digest[:]).Scan(&owner, &user, &session, &revision, &installationID, &accountID, &accountType)
	if err != nil || owner != principal.OwnerID || user != principal.UserID || session != principal.SessionID || revision != int64(principal.AuthRevision) || installationID != installation.ID || accountID != installation.Account.AccountID || accountType != installation.Account.Type {
		return ErrHandoffInvalid
	}
	var locked int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp() FOR UPDATE OF s,u`, session, user, owner, revision).Scan(&locked); err != nil || locked != 1 {
		return ErrGitHubDenied
	}
	tag, err := tx.Exec(ctx, `INSERT INTO github_installations(installation_id,owner_id,account_id,account_type,active,suspended) VALUES($1,$2,$3,$4,true,false) ON CONFLICT(installation_id) DO UPDATE SET active=true,suspended=false,updated_at=clock_timestamp() WHERE github_installations.owner_id=EXCLUDED.owner_id AND github_installations.account_id=EXCLUDED.account_id AND github_installations.account_type=EXCLUDED.account_type`, installation.ID, owner, accountID, accountType)
	if err != nil {
		return fmt.Errorf("upsert installation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrGitHubDenied
	}
	repositoryIDs := make([]int64, 0, len(repositories))
	for _, repo := range repositories {
		if repo.ID <= 0 || repo.OwnerID != accountID || repo.Name == "" || repo.FullName == "" {
			return ErrGitHubDenied
		}
		repositoryIDs = append(repositoryIDs, repo.ID)
		tag, err = tx.Exec(ctx, `INSERT INTO github_repositories(repository_id,installation_id,owner_id,node_id,github_owner_id,name,full_name,clone_url,default_branch,active) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,'https://github.com/'||$7||'.git',NULLIF($8,''),true) ON CONFLICT(repository_id) DO UPDATE SET node_id=EXCLUDED.node_id,name=EXCLUDED.name,full_name=EXCLUDED.full_name,clone_url=EXCLUDED.clone_url,default_branch=EXCLUDED.default_branch,active=true,authorization_revision=github_repositories.authorization_revision+1,updated_at=clock_timestamp() WHERE github_repositories.owner_id=EXCLUDED.owner_id AND github_repositories.installation_id=EXCLUDED.installation_id AND github_repositories.github_owner_id=EXCLUDED.github_owner_id`, repo.ID, installation.ID, owner, repo.NodeID, repo.OwnerID, repo.Name, repo.FullName, repo.DefaultBranch)
		if err != nil {
			return fmt.Errorf("upsert repository: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrGitHubDenied
		}
	}
	// The provider response is a complete discovery snapshot. Repositories that
	// were previously active but are absent from it must no longer authorize
	// bindings. The CTE limits binding revision changes to this exact snapshot.
	if _, err = tx.Exec(ctx, `WITH deactivated AS (
		UPDATE github_repositories
		SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp()
		WHERE owner_id=$1 AND installation_id=$2 AND active AND NOT (repository_id=ANY($3::bigint[]))
		RETURNING repository_id
	)
	UPDATE github_project_bindings b
	SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp()
	FROM deactivated d
	WHERE b.owner_id=$1 AND b.repository_id=d.repository_id AND b.active`, owner, installation.ID, repositoryIDs); err != nil {
		return fmt.Errorf("deactivate removed repositories: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE github_pending_handoffs SET consumed_at=clock_timestamp() WHERE digest=$1`, digest[:]); err != nil {
		return fmt.Errorf("consume handoff: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,installation_id) VALUES($1,$2,$3,'setup_completed','success',$4)`, auditID("handoff", digest), owner, user, installation.ID)
	if err != nil {
		return fmt.Errorf("audit completion: %w", err)
	}
	return tx.Commit(ctx)
}

func (r *GitHubRepository) BindRepository(ctx context.Context, principal auth.Principal, projectID string, repositoryID int64, audit string) error {
	if principal.Validate() != nil || len(projectID) == 0 || len(projectID) > 128 || repositoryID <= 0 || validID(audit) != nil {
		return ErrGitHubDenied
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = activeRegistration(ctx, tx); err != nil {
		return err
	}
	var locked int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp() FOR UPDATE OF s,u`, principal.SessionID, principal.UserID, principal.OwnerID, int64(principal.AuthRevision)).Scan(&locked); err != nil || locked != 1 {
		return ErrGitHubDenied
	}
	var installID int64
	var active, suspended bool
	err = tx.QueryRow(ctx, `SELECT r.installation_id,r.active,i.suspended FROM github_repositories r JOIN github_installations i ON i.installation_id=r.installation_id AND i.owner_id=r.owner_id WHERE r.repository_id=$1 AND r.owner_id=$2 AND i.active=true FOR UPDATE OF r,i`, repositoryID, principal.OwnerID).Scan(&installID, &active, &suspended)
	if err != nil || !active || suspended {
		return ErrGitHubDenied
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_project_bindings(owner_id,project_id,repository_id,installation_id,active) VALUES($1,$2,$3,$4,true) ON CONFLICT(owner_id,project_id) DO UPDATE SET repository_id=EXCLUDED.repository_id,installation_id=EXCLUDED.installation_id,active=true,authorization_revision=github_project_bindings.authorization_revision+1,updated_at=clock_timestamp()`, principal.OwnerID, projectID, repositoryID, installID); err != nil {
		return fmt.Errorf("upsert binding: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,installation_id,repository_id,project_id) VALUES($1,$2,$3,'repository_bound','success',$4,$5,$6)`, audit, principal.OwnerID, principal.UserID, installID, repositoryID, projectID); err != nil {
		return fmt.Errorf("audit binding: %w", err)
	}
	return tx.Commit(ctx)
}

// DisableInstallation fails closed for one owner-scoped installation. It is
// idempotent: a prior disable is accepted without creating duplicate audits.
func (r *GitHubRepository) DisableInstallation(ctx context.Context, principal auth.Principal, installationID int64, audit string) error {
	if principal.Validate() != nil || installationID <= 0 || validID(audit) != nil {
		return ErrGitHubDenied
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin disable installation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = activeRegistration(ctx, tx); err != nil {
		return err
	}
	var locked int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp() FOR UPDATE OF s,u`, principal.SessionID, principal.UserID, principal.OwnerID, int64(principal.AuthRevision)).Scan(&locked); err != nil || locked != 1 {
		return ErrGitHubDenied
	}
	tag, err := tx.Exec(ctx, `UPDATE github_installations SET active=false,suspended=true,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE installation_id=$1 AND owner_id=$2 AND active`, installationID, principal.OwnerID)
	if err != nil {
		return fmt.Errorf("disable installation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists int
		if err = tx.QueryRow(ctx, `SELECT 1 FROM github_installations WHERE installation_id=$1 AND owner_id=$2 FOR UPDATE`, installationID, principal.OwnerID).Scan(&exists); err != nil {
			return ErrGitHubDenied
		}
		return tx.Commit(ctx)
	}
	if tag.RowsAffected() != 1 {
		return ErrGitHubDenied
	}
	rows, err := tx.Query(ctx, `UPDATE github_repositories SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE installation_id=$1 AND owner_id=$2 AND active RETURNING repository_id`, installationID, principal.OwnerID)
	if err != nil {
		return fmt.Errorf("disable repositories: %w", err)
	}
	deactivated := make([]int64, 0)
	for rows.Next() {
		var repositoryID int64
		if err = rows.Scan(&repositoryID); err != nil {
			rows.Close()
			return fmt.Errorf("read disabled repository: %w", err)
		}
		deactivated = append(deactivated, repositoryID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("disable repositories: %w", err)
	}
	rows.Close()
	if len(deactivated) > 0 {
		if _, err = tx.Exec(ctx, `UPDATE github_project_bindings SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND active AND repository_id=ANY($2::bigint[])`, principal.OwnerID, deactivated); err != nil {
			return fmt.Errorf("disable repository bindings: %w", err)
		}
	}
	invalidated, err := invalidatedCheckoutIDs(ctx, tx, principal.OwnerID, installationID)
	if err != nil {
		return fmt.Errorf("invalidate checkout reservations: %w", err)
	}
	if tag, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,installation_id) VALUES($1,$2,$3,'installation_disabled','success',$4)`, audit, principal.OwnerID, principal.UserID, installationID); err != nil {
		return fmt.Errorf("audit disabled installation: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ErrGitHubDenied
	}
	for _, repositoryID := range deactivated {
		id := fmt.Sprintf("%s_r_%d", audit, repositoryID)
		if tag, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,installation_id,repository_id) VALUES($1,$2,$3,'repository_disabled','success',$4,$5)`, id, principal.OwnerID, principal.UserID, installationID, repositoryID); err != nil {
			return fmt.Errorf("audit disabled repository: %w", err)
		} else if tag.RowsAffected() != 1 {
			return ErrGitHubDenied
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	for _, id := range invalidated {
		githubapp.InvalidateCheckoutReservation(id)
	}
	return nil
}

// ReconcileInstallation applies one complete, successfully fetched discovery
// snapshot. It never creates an installation; setup completion is the sole
// activation path.
func (r *GitHubRepository) ReconcileInstallation(ctx context.Context, principal auth.Principal, installation githubapp.Installation, repositories []githubapp.Repository, audit string) error {
	if principal.Validate() != nil || installation.ID <= 0 || installation.Suspended || !installation.Account.Valid() || len(repositories) > 1000 || validID(audit) != nil {
		return ErrGitHubDenied
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin reconcile installation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = activeRegistration(ctx, tx); err != nil {
		return err
	}
	var locked int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp() FOR UPDATE OF s,u`, principal.SessionID, principal.UserID, principal.OwnerID, int64(principal.AuthRevision)).Scan(&locked); err != nil || locked != 1 {
		return ErrGitHubDenied
	}
	var exists int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM github_installations WHERE installation_id=$1 AND owner_id=$2 AND account_id=$3 AND account_type=$4 AND active AND NOT suspended FOR UPDATE`, installation.ID, principal.OwnerID, installation.Account.AccountID, installation.Account.Type).Scan(&exists); err != nil || exists != 1 {
		return ErrGitHubDenied
	}
	ids := make([]int64, 0, len(repositories))
	for _, repository := range repositories {
		if repository.ID <= 0 || repository.OwnerID != installation.Account.AccountID || repository.Name == "" || repository.FullName == "" {
			return ErrGitHubDenied
		}
		ids = append(ids, repository.ID)
		// Discovery is metadata-only, but its complete snapshot can refresh metadata.
		if _, err = tx.Exec(ctx, `UPDATE github_repositories SET node_id=NULLIF($4,''),name=$5,full_name=$6,clone_url='https://github.com/'||$6||'.git',default_branch=NULLIF($7,''),active=true,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE repository_id=$1 AND owner_id=$2 AND installation_id=$3`, repository.ID, principal.OwnerID, installation.ID, repository.NodeID, repository.Name, repository.FullName, repository.DefaultBranch); err != nil {
			return fmt.Errorf("refresh repository: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `UPDATE github_repositories SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND installation_id=$2 AND active AND NOT (repository_id=ANY($3::bigint[])) RETURNING repository_id`, principal.OwnerID, installation.ID, ids)
	if err != nil {
		return fmt.Errorf("deactivate removed repositories: %w", err)
	}
	var removed []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		removed = append(removed, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(removed) > 0 {
		if _, err = tx.Exec(ctx, `UPDATE github_project_bindings SET active=false,authorization_revision=authorization_revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND active AND repository_id=ANY($2::bigint[])`, principal.OwnerID, removed); err != nil {
			return fmt.Errorf("disable removed bindings: %w", err)
		}
	}
	invalidated, err := invalidatedCheckoutIDs(ctx, tx, principal.OwnerID, installation.ID)
	if err != nil {
		return fmt.Errorf("invalidate checkout reservations: %w", err)
	}
	for _, id := range removed {
		if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,installation_id,repository_id) VALUES($1,$2,$3,'repository_disabled','success',$4,$5)`, fmt.Sprintf("%s_r_%d", audit, id), principal.OwnerID, principal.UserID, installation.ID, id); err != nil {
			return fmt.Errorf("audit removed repository: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	for _, id := range invalidated {
		githubapp.InvalidateCheckoutReservation(id)
	}
	return nil
}

func invalidatedCheckoutIDs(ctx context.Context, tx pgx.Tx, owner string, installationID int64) ([]string, error) {
	rows, err := tx.Query(ctx, `UPDATE github_checkout_reservations SET state='invalidated' WHERE owner_id=$1 AND installation_id=$2 AND state IN ('reserved','minting','active','running') RETURNING id::text`, owner, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// auditID derives a stable, opaque event identifier. The domain separates audit
// events that are tied to the same handoff capability but represent distinct
// state transitions.
func auditID(domain string, digest [32]byte) string {
	return fmt.Sprintf("gh_%s_%x", domain, digest[:16])
}
