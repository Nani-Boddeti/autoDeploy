package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/crypto"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinels for the GitHub App manifest flow. They are deliberately distinct
// so tests and logs can tell the causes apart; the web layer maps the state
// errors (and ErrManifestCreateInFlight) to one generic message.
var (
	ErrManifestAppExists = errors.New("GitHub App already registered")
	// ErrManifestPriorAppRegistered: the env-configured App row (000005) exists,
	// and EnsureAppRegistration would refuse a different App at startup.
	ErrManifestPriorAppRegistered = errors.New("a different GitHub App registration already exists")
	ErrManifestStateInvalid       = errors.New("GitHub App create state is invalid")
	ErrManifestStateExpired       = errors.New("GitHub App create state expired")
	ErrManifestStateConsumed      = errors.New("GitHub App create state already used")
	ErrManifestCreateInFlight     = errors.New("GitHub App creation already in progress")
	ErrManifestInputInvalid       = errors.New("GitHub App registration input is invalid")
	errManifestRegistrationRow    = errors.New("GitHub App registration row is malformed")
)

const (
	// manifestLockKey serializes consume/insert for the singleton App. Distinct
	// from the other advisory keys in this repo: 731492845631694122 (migration
	// runner), ...123 (checkout authority), ...124 (setup admission), ...199
	// (migration test only).
	manifestLockKey = 731492845631694125
	// manifestMaxActiveStates bounds live (unconsumed, unexpired) create states.
	manifestMaxActiveStates = 100
	// manifestMaxTTL mirrors the migration's CHECK.
	manifestMaxTTL = time.Hour
	// manifestInFlightWindow is how long a consumed state blocks another
	// consume: the conversion call plus the registration insert happen inside it.
	manifestInFlightWindow = "2 minutes"

	manifestPrivateKeyVariable    = "github-app-private-key"
	manifestWebhookSecretVariable = "github-webhook-secret"
)

// GitHubManifestRepository persists the manifest-flow capabilities and the
// resulting GitHub App registration (secrets only as KEK-wrapped envelopes).
type GitHubManifestRepository struct{ pool *pgxpool.Pool }

func NewGitHubManifestRepository(pool *pgxpool.Pool) *GitHubManifestRepository {
	return &GitHubManifestRepository{pool: pool}
}

// ManifestRegistration is the non-secret metadata of the stored App.
type ManifestRegistration struct {
	ID, Slug, ClientID, GitHubOwnerType, GitHubOwnerLogin, HTMLURL string
	AppID, GitHubOwnerID                                           int64
	OwnerID                                                        string
	CreatedAt                                                      time.Time
}

// ManifestRegistrationInput carries the converted App. InsertRegistration
// clears PrivateKeyPEM and WebhookSecret before returning.
type ManifestRegistrationInput struct {
	Principal                                                  auth.Principal
	StateDigest                                                [32]byte
	AppID, GitHubOwnerID                                       int64
	Slug, ClientID, GitHubOwnerType, GitHubOwnerLogin, HTMLURL string
	PrivateKeyPEM, WebhookSecret                               []byte
}

func manifestScope(ownerID, variable, registrationID string) crypto.Scope {
	return crypto.Scope{OwnerID: ownerID, ProjectID: "system", EnvironmentID: "system", VariableID: variable, SecretVersionID: registrationID, Purpose: "control_plane", DeliveryScope: "control_plane"}
}

type manifestQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// manifestPriorRefusal returns ErrManifestPriorAppRegistered when the 000005
// env-App singleton row exists: creating a different App would then fail every
// startup in EnsureAppRegistration.
func manifestPriorRefusal(ctx context.Context, q manifestQuerier) error {
	var prior bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_app_registrations)`).Scan(&prior); err != nil {
		return fmt.Errorf("check prior GitHub App registration: %w", err)
	}
	if prior {
		return ErrManifestPriorAppRegistered
	}
	return nil
}

// PriorAppRegistered reports whether the env-configured App row (000005) exists.
func (r *GitHubManifestRepository) PriorAppRegistered(ctx context.Context) (bool, error) {
	var prior bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_app_registrations)`).Scan(&prior); err != nil {
		return false, fmt.Errorf("check prior GitHub App registration: %w", err)
	}
	return prior, nil
}

func manifestAppExists(ctx context.Context, tx pgx.Tx) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_manifest_apps)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check GitHub App registration: %w", err)
	}
	return exists, nil
}

// CreateManifestState issues a single-use create capability bound to the
// principal's session. A session holds at most one live state: issuing a new
// one retires the previous by shortening its expiry (rows are retained).
func (r *GitHubManifestRepository) CreateManifestState(ctx context.Context, digest [32]byte, principal auth.Principal, ttl time.Duration) error {
	if principal.Validate() != nil || digest == ([32]byte{}) || ttl <= 0 || ttl > manifestMaxTTL {
		return ErrManifestInputInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin manifest state: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(manifestLockKey)); err != nil {
		return fmt.Errorf("lock manifest state: %w", err)
	}
	exists, err := manifestAppExists(ctx, tx)
	if err != nil {
		return err
	}
	if exists {
		return ErrManifestAppExists
	}
	if err = manifestPriorRefusal(ctx, tx); err != nil {
		return err
	}
	if err = lockLiveSession(ctx, tx, principal); err != nil {
		return err
	}
	// Retire this session's previous live state before counting, so replacing
	// never trips the cap.
	if _, err = tx.Exec(ctx, `UPDATE github_manifest_states SET expires_at=clock_timestamp() WHERE session_id=$1 AND consumed_at IS NULL AND expires_at>clock_timestamp()`, principal.SessionID); err != nil {
		return fmt.Errorf("retire manifest state: %w", err)
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM github_manifest_states WHERE consumed_at IS NULL AND expires_at>clock_timestamp()`).Scan(&active); err != nil {
		return fmt.Errorf("count manifest states: %w", err)
	}
	if active >= manifestMaxActiveStates {
		return ErrManifestInputInvalid
	}
	if _, err = tx.Exec(ctx, `WITH n AS (SELECT clock_timestamp() AS t) INSERT INTO github_manifest_states(digest,owner_id,user_id,session_id,auth_revision,created_at,expires_at) SELECT $1,$2,$3,$4,$5,n.t,n.t+make_interval(secs=>$6) FROM n`, digest[:], principal.OwnerID, principal.UserID, principal.SessionID, int64(principal.AuthRevision), ttl.Seconds()); err != nil {
		return fmt.Errorf("insert manifest state: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result) VALUES($1,$2,$3,'app_create_started','success')`, auditID("appcreate", digest), principal.OwnerID, principal.UserID); err != nil {
		return fmt.Errorf("audit manifest start: %w", err)
	}
	return tx.Commit(ctx)
}

// liveSessionSQL selects the principal's session when it is still valid. The
// locking variant appends FOR UPDATE; the pre-check reads without locks.
const liveSessionSQL = `SELECT 1 FROM admin_sessions s JOIN users u ON u.id=s.user_id AND u.owner_id=s.owner_id WHERE s.id=$1 AND s.user_id=$2 AND s.owner_id=$3 AND s.auth_revision=$4 AND u.auth_revision=$4 AND u.status='active' AND s.revoked_at IS NULL AND s.idle_expires_at>clock_timestamp() AND s.absolute_expires_at>clock_timestamp()`

func lockLiveSession(ctx context.Context, tx pgx.Tx, p auth.Principal) error {
	return checkLiveSession(ctx, tx, p, " FOR UPDATE OF s,u")
}

func checkLiveSession(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, p auth.Principal, lock string) error {
	var locked int
	if err := q.QueryRow(ctx, liveSessionSQL+lock, p.SessionID, p.UserID, p.OwnerID, int64(p.AuthRevision)).Scan(&locked); err != nil || locked != 1 {
		return ErrManifestStateInvalid
	}
	return nil
}

// ConsumeManifestStateForCreate atomically checks that no App exists and no
// other creation is in flight, validates the state against the principal, and
// marks it consumed. Success is the caller's only license to start the GitHub
// conversion; a consumed state can never be reused.
func (r *GitHubManifestRepository) ConsumeManifestStateForCreate(ctx context.Context, digest [32]byte, principal auth.Principal) error {
	if principal.Validate() != nil || digest == ([32]byte{}) {
		return ErrManifestStateInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin manifest consume: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(manifestLockKey)); err != nil {
		return fmt.Errorf("lock manifest consume: %w", err)
	}
	exists, err := manifestAppExists(ctx, tx)
	if err != nil {
		return err
	}
	if exists {
		return ErrManifestAppExists
	}
	if err = manifestPriorRefusal(ctx, tx); err != nil {
		return err
	}
	var ownerID, userID, sessionID string
	var revision int64
	var expired, consumed bool
	err = tx.QueryRow(ctx, `SELECT owner_id,user_id,session_id,auth_revision,expires_at<=clock_timestamp(),consumed_at IS NOT NULL FROM github_manifest_states WHERE digest=$1 FOR UPDATE`, digest[:]).Scan(&ownerID, &userID, &sessionID, &revision, &expired, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrManifestStateInvalid
	}
	if err != nil {
		return fmt.Errorf("read manifest state: %w", err)
	}
	if ownerID != principal.OwnerID || userID != principal.UserID || sessionID != principal.SessionID || revision != int64(principal.AuthRevision) {
		return ErrManifestStateInvalid
	}
	if consumed {
		return ErrManifestStateConsumed
	}
	if expired {
		return ErrManifestStateExpired
	}
	if err = lockLiveSession(ctx, tx, principal); err != nil {
		return err
	}
	var inFlight bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_manifest_states WHERE consumed_at IS NOT NULL AND consumed_at>clock_timestamp()-interval '`+manifestInFlightWindow+`')`).Scan(&inFlight); err != nil {
		return fmt.Errorf("check in-flight creation: %w", err)
	}
	if inFlight {
		return ErrManifestCreateInFlight
	}
	// One timestamp for the expiry test and consumed_at, so consumed_at<=expires_at can never be violated.
	tag, err := tx.Exec(ctx, `WITH n AS (SELECT clock_timestamp() AS t) UPDATE github_manifest_states s SET consumed_at=n.t FROM n WHERE s.digest=$1 AND s.consumed_at IS NULL AND s.expires_at>n.t`, digest[:])
	if err != nil {
		return fmt.Errorf("consume manifest state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrManifestStateExpired
	}
	return tx.Commit(ctx)
}

// manifestInsertable reports whether an App may be inserted for the state: no
// App exists and the principal consumed this very state within the in-flight
// window (so no other consume can have become possible since).
func manifestInsertable(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, in ManifestRegistrationInput) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_manifest_apps)`).Scan(&exists); err != nil {
		return fmt.Errorf("check GitHub App registration: %w", err)
	}
	if exists {
		return ErrManifestAppExists
	}
	if err := manifestPriorRefusal(ctx, q); err != nil {
		return err
	}
	var matched int
	err := q.QueryRow(ctx, `SELECT 1 FROM github_manifest_states WHERE digest=$1 AND owner_id=$2 AND user_id=$3 AND session_id=$4 AND auth_revision=$5 AND consumed_at IS NOT NULL AND consumed_at>clock_timestamp()-interval '`+manifestInFlightWindow+`'`, in.StateDigest[:], in.Principal.OwnerID, in.Principal.UserID, in.Principal.SessionID, int64(in.Principal.AuthRevision)).Scan(&matched)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrManifestStateInvalid
	}
	if err != nil {
		return fmt.Errorf("verify consumed manifest state: %w", err)
	}
	return nil
}

// InsertRegistration encrypts the PEM and webhook secret under the
// control_plane scope and stores the App with its audit row. The state must
// already be consumed by the same principal; a registration that exists makes
// it fail with ErrManifestAppExists. Plaintext input buffers are cleared.
func (r *GitHubManifestRepository) InsertRegistration(ctx context.Context, in ManifestRegistrationInput, provider crypto.KeyProvider) (ManifestRegistration, error) {
	defer clear(in.PrivateKeyPEM)
	defer clear(in.WebhookSecret)
	if in.Principal.Validate() != nil || in.StateDigest == ([32]byte{}) || in.AppID <= 0 || in.GitHubOwnerID <= 0 ||
		in.Slug == "" || len(in.Slug) > 100 || in.ClientID == "" || len(in.ClientID) > 255 || in.GitHubOwnerLogin == "" || len(in.GitHubOwnerLogin) > 100 || in.HTMLURL == "" || len(in.HTMLURL) > 512 ||
		(in.GitHubOwnerType != "User" && in.GitHubOwnerType != "Organization") ||
		len(in.PrivateKeyPEM) == 0 || len(in.WebhookSecret) == 0 || provider == nil {
		return ManifestRegistration{}, ErrManifestInputInvalid
	}
	// Cheap unlocked pre-check so refusals never burn wrap-ledger reservations.
	// It is inherently racy (state can change between it and the lock), so the
	// locked re-check below is authoritative; a lost race merely wastes two
	// reservations, never correctness.
	if err := manifestInsertable(ctx, r.pool, in); err != nil {
		return ManifestRegistration{}, err
	}
	if err := checkLiveSession(ctx, r.pool, in.Principal, ""); err != nil {
		return ManifestRegistration{}, err
	}
	id, err := checkoutID()
	if err != nil {
		return ManifestRegistration{}, fmt.Errorf("generate registration id: %w", err)
	}
	reserver := NewKeyRepository(r.pool)
	keyEnvelope, err := crypto.Encrypt(provider, reserver, manifestScope(in.Principal.OwnerID, manifestPrivateKeyVariable, id), in.PrivateKeyPEM)
	if err != nil {
		return ManifestRegistration{}, fmt.Errorf("encrypt GitHub App private key: %w", err)
	}
	secretEnvelope, err := crypto.Encrypt(provider, reserver, manifestScope(in.Principal.OwnerID, manifestWebhookSecretVariable, id), in.WebhookSecret)
	if err != nil {
		return ManifestRegistration{}, fmt.Errorf("encrypt GitHub webhook secret: %w", err)
	}
	keyBlob, err := keyEnvelope.MarshalBinary()
	if err != nil {
		return ManifestRegistration{}, fmt.Errorf("encode GitHub App private key envelope: %w", err)
	}
	secretBlob, err := secretEnvelope.MarshalBinary()
	if err != nil {
		return ManifestRegistration{}, fmt.Errorf("encode GitHub webhook secret envelope: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ManifestRegistration{}, fmt.Errorf("begin registration insert: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(manifestLockKey)); err != nil {
		return ManifestRegistration{}, fmt.Errorf("lock registration insert: %w", err)
	}
	if err = manifestInsertable(ctx, tx, in); err != nil {
		return ManifestRegistration{}, err
	}
	if err = lockLiveSession(ctx, tx, in.Principal); err != nil {
		return ManifestRegistration{}, err
	}
	reg := ManifestRegistration{ID: id, Slug: in.Slug, ClientID: in.ClientID, GitHubOwnerType: in.GitHubOwnerType, GitHubOwnerLogin: in.GitHubOwnerLogin, HTMLURL: in.HTMLURL, AppID: in.AppID, GitHubOwnerID: in.GitHubOwnerID, OwnerID: in.Principal.OwnerID}
	if err = tx.QueryRow(ctx, `INSERT INTO github_manifest_apps(id,app_id,slug,client_id,github_owner_id,github_owner_type,github_owner_login,html_url,private_key_envelope,webhook_secret_envelope,owner_id,created_by_user_id,private_key_kek_id,webhook_secret_kek_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING created_at`,
		id, in.AppID, in.Slug, in.ClientID, in.GitHubOwnerID, in.GitHubOwnerType, in.GitHubOwnerLogin, in.HTMLURL, keyBlob, secretBlob, in.Principal.OwnerID, in.Principal.UserID, keyEnvelope.KEKID, secretEnvelope.KEKID).Scan(&reg.CreatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ManifestRegistration{}, ErrManifestAppExists
		}
		return ManifestRegistration{}, fmt.Errorf("insert GitHub App registration: %w", err)
	}
	// Audit metadata is identity only: app id, slug, GitHub owner. No secrets.
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,actor_user_id,action,result,metadata) VALUES($1,$2,$3,'app_created','success',jsonb_build_object('app_id',$4::bigint,'slug',$5::text,'github_owner',$6::text))`,
		"gh_appcreated_"+id, in.Principal.OwnerID, in.Principal.UserID, in.AppID, in.Slug, in.GitHubOwnerLogin); err != nil {
		return ManifestRegistration{}, fmt.Errorf("audit GitHub App created: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ManifestRegistration{}, fmt.Errorf("commit GitHub App registration: %w", err)
	}
	return reg, nil
}

// ManifestAppExists reports whether a manifest-created App is stored, without
// reading or decrypting any secret.
func (r *GitHubManifestRepository) ManifestAppExists(ctx context.Context) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_manifest_apps)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check GitHub App registration: %w", err)
	}
	return exists, nil
}

// LoadRegistration returns the stored App and its decrypted secrets. A decrypt
// failure is an error, never "not found". The caller owns and should clear the
// returned plaintext buffers.
func (r *GitHubManifestRepository) LoadRegistration(ctx context.Context, provider crypto.KeyProvider) (ManifestRegistration, []byte, []byte, bool, error) {
	var reg ManifestRegistration
	var keyBlob, secretBlob []byte
	var keyKEK, secretKEK string
	err := r.pool.QueryRow(ctx, `SELECT id,app_id,slug,client_id,github_owner_id,github_owner_type,github_owner_login,html_url,private_key_envelope,webhook_secret_envelope,owner_id,created_at,private_key_kek_id,webhook_secret_kek_id FROM github_manifest_apps`).Scan(
		&reg.ID, &reg.AppID, &reg.Slug, &reg.ClientID, &reg.GitHubOwnerID, &reg.GitHubOwnerType, &reg.GitHubOwnerLogin, &reg.HTMLURL, &keyBlob, &secretBlob, &reg.OwnerID, &reg.CreatedAt, &keyKEK, &secretKEK)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManifestRegistration{}, nil, nil, false, nil
	}
	if err != nil {
		return ManifestRegistration{}, nil, nil, false, fmt.Errorf("load GitHub App registration: %w", err)
	}
	if provider == nil {
		return ManifestRegistration{}, nil, nil, false, ErrManifestInputInvalid
	}
	pem, err := openManifestSecret(provider, reg, manifestPrivateKeyVariable, keyBlob, keyKEK)
	if err != nil {
		return ManifestRegistration{}, nil, nil, false, fmt.Errorf("decrypt GitHub App private key: %w", err)
	}
	secret, err := openManifestSecret(provider, reg, manifestWebhookSecretVariable, secretBlob, secretKEK)
	if err != nil {
		clear(pem)
		return ManifestRegistration{}, nil, nil, false, fmt.Errorf("decrypt GitHub webhook secret: %w", err)
	}
	return reg, pem, secret, true, nil
}

func openManifestSecret(provider crypto.KeyProvider, reg ManifestRegistration, variable string, blob []byte, kekID string) ([]byte, error) {
	envelope, err := crypto.ParseEnvelope(blob)
	// The recorded KEK id is what retirement checks consult; it must match the envelope.
	if err != nil || envelope.KEKID != kekID {
		return nil, errManifestRegistrationRow
	}
	return crypto.Decrypt(provider, manifestScope(reg.OwnerID, variable, reg.ID), envelope)
}
