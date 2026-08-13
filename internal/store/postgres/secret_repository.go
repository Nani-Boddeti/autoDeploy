package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"autodeploy/internal/crypto"
	"autodeploy/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrSecretNotFound = errors.New("secret not found")

type SecretRepository struct{ pool *pgxpool.Pool }

func NewSecretRepository(pool *pgxpool.Pool) *SecretRepository { return &SecretRepository{pool: pool} }
func (r *SecretRepository) ReserveWrapInvocation(kekID string) error {
	if r == nil || r.pool == nil {
		return ErrWrapInvocationExhausted
	}
	return NewKeyRepository(r.pool).ReserveWrapInvocation(kekID)
}

func (r *SecretRepository) Create(ctx context.Context, mutation secret.Mutation) error {
	if r == nil || r.pool == nil {
		return secret.ErrInvalidInput
	}
	if err := validSecretMutation(mutation, 1); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin secret create: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	v := mutation.Version
	s := v.Scope
	_, err = tx.Exec(ctx, `INSERT INTO secret_heads(id,owner_id,project_id,environment_id,purpose,delivery_scope) VALUES($1,$2,$3,$4,$5,$6)`, s.SecretID, s.OwnerID, s.ProjectID, s.EnvironmentID, s.Purpose, s.DeliveryScope)
	if err != nil {
		return secretWriteErr("insert secret head", err)
	}
	if err = insertSecretVersion(ctx, tx, v); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE secret_heads SET current_version_id=$2,revision=revision+1,updated_at=now() WHERE id=$1 AND revision=0`, s.SecretID, v.ID)
	if err != nil {
		return fmt.Errorf("set initial secret version: %w", err)
	}
	if err := requireOneSecretHeadAdvance(command.RowsAffected()); err != nil {
		return err
	}
	if err = insertSecretAudit(ctx, tx, mutation.Audit); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit secret create: %w", err)
	}
	return nil
}

// requireOneSecretHeadAdvance keeps the initial head/version linkage a strict
// CAS: a zero-row update must roll back the version and audit transaction.
func requireOneSecretHeadAdvance(rowsAffected int64) error {
	if rowsAffected != 1 {
		return ErrRevisionConflict
	}
	return nil
}
func (r *SecretRepository) Replace(ctx context.Context, mutation secret.Mutation) error {
	return r.replace(ctx, mutation, false)
}
func (r *SecretRepository) ReplaceCompromised(ctx context.Context, mutation secret.Mutation) error {
	return r.replace(ctx, mutation, true)
}
func (r *SecretRepository) replace(ctx context.Context, mutation secret.Mutation, compromised bool) error {
	if r == nil || r.pool == nil {
		return secret.ErrInvalidInput
	}
	if err := validSecretMutation(mutation, 0); err != nil {
		return err
	}
	if compromised != (mutation.Audit.Action == "replaced_compromised") {
		return secret.ErrInvalidInput
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin secret replace: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	v := mutation.Version
	s := v.Scope
	var current string
	var number int64
	var owner, project, environment, purpose, delivery string
	err = tx.QueryRow(ctx, `SELECT current_version_id,owner_id,project_id,environment_id,purpose,delivery_scope FROM secret_heads WHERE id=$1 FOR UPDATE`, s.SecretID).Scan(&current, &owner, &project, &environment, &purpose, &delivery)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSecretNotFound
	}
	if err != nil {
		return fmt.Errorf("lock secret head: %w", err)
	}
	if owner != s.OwnerID || project != s.ProjectID || environment != s.EnvironmentID || purpose != s.Purpose || delivery != s.DeliveryScope {
		return ErrSecretNotFound
	}
	err = tx.QueryRow(ctx, `SELECT version_number FROM secret_versions WHERE id=$1 AND secret_id=$2 AND state='active'`, current, s.SecretID).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSecretNotFound
	}
	if err != nil {
		return fmt.Errorf("load active secret version: %w", err)
	}
	if v.Number != uint64(number+1) {
		return ErrRevisionConflict
	}
	if mutation.Audit.OldVersionID != current {
		return secret.ErrInvalidInput
	}
	oldState := "superseded"
	if compromised {
		oldState = "revoked"
	}
	if _, err = tx.Exec(ctx, `UPDATE secret_versions SET state=$2 WHERE id=$1`, current, oldState); err != nil {
		return fmt.Errorf("replace active secret version: %w", err)
	}
	if err = insertSecretVersion(ctx, tx, v); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE secret_heads SET current_version_id=$2,revision=revision+1,updated_at=now() WHERE id=$1 AND current_version_id=$3`, s.SecretID, v.ID, current)
	if err != nil {
		return fmt.Errorf("advance secret head: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrRevisionConflict
	}
	if err = insertSecretAudit(ctx, tx, mutation.Audit); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit secret replace: %w", err)
	}
	return nil
}
func (r *SecretRepository) GetVersion(ctx context.Context, scope secret.Scope, versionID string) (secret.Version, error) {
	if r == nil || r.pool == nil {
		return secret.Version{}, ErrSecretNotFound
	}
	q := `SELECT v.id,v.secret_id,v.owner_id,v.project_id,v.environment_id,v.version_number,v.state,v.suite,v.aad_version,v.kek_id,v.ciphertext,v.wrapped_dek,v.envelope_revision,v.created_at FROM secret_heads h JOIN secret_versions v ON v.secret_id=h.id WHERE h.id=$1 AND h.owner_id=$2 AND h.project_id=$3 AND h.environment_id=$4 AND h.purpose=$5 AND h.delivery_scope=$6`
	args := []any{scope.SecretID, scope.OwnerID, scope.ProjectID, scope.EnvironmentID, scope.Purpose, scope.DeliveryScope}
	if versionID == "" {
		q += " AND v.id=h.current_version_id"
	} else {
		q += " AND v.id=$7"
		args = append(args, versionID)
	}
	var v secret.Version
	v.Scope = scope
	var state string
	var stored secret.Scope
	var envelopeRevision int64
	err := r.pool.QueryRow(ctx, q, args...).Scan(&v.ID, &stored.SecretID, &stored.OwnerID, &stored.ProjectID, &stored.EnvironmentID, &v.Number, &state, &v.Envelope.Suite, &v.Envelope.AADVersion, &v.Envelope.KEKID, &v.Envelope.Ciphertext, &v.Envelope.WrappedDEK, &envelopeRevision, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return secret.Version{}, ErrSecretNotFound
	}
	if err != nil {
		return secret.Version{}, fmt.Errorf("load secret version: %w", err)
	}
	stored.Purpose, stored.DeliveryScope = scope.Purpose, scope.DeliveryScope
	v.State = secret.State(state)
	v.EnvelopeRevision = uint64(envelopeRevision)
	if envelopeRevision < 1 || stored != scope || !validStoredVersion(v) {
		return secret.Version{}, secret.ErrCorrupt
	}
	return v, nil
}
func insertSecretVersion(ctx context.Context, tx pgx.Tx, v secret.Version) error {
	_, err := tx.Exec(ctx, `INSERT INTO secret_versions(id,secret_id,owner_id,project_id,environment_id,version_number,state,suite,aad_version,kek_id,ciphertext,wrapped_dek,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now())`, v.ID, v.Scope.SecretID, v.Scope.OwnerID, v.Scope.ProjectID, v.Scope.EnvironmentID, int64(v.Number), v.State, v.Envelope.Suite, v.Envelope.AADVersion, v.Envelope.KEKID, v.Envelope.Ciphertext, v.Envelope.WrappedDEK)
	if err != nil {
		return secretWriteErr("insert secret version", err)
	}
	return nil
}
func insertSecretAudit(ctx context.Context, tx pgx.Tx, a secret.Audit) error {
	result := a.Result
	if result == "" {
		result = "success"
	}
	command, err := tx.Exec(ctx, `INSERT INTO secret_audit_events(id,owner_id,secret_id,secret_version_id,action,actor_id,service_id,result,old_version_id,kek_id,project_id,environment_id,deployment_id,lease_id) SELECT $1,$2,h.id,$4,$5,$6,$7,$8,$9,$10,h.project_id,h.environment_id,$11,$12 FROM secret_heads h JOIN secret_versions v ON v.id=$4 AND v.secret_id=h.id WHERE h.id=$3 AND h.owner_id=$2`, a.ID, a.OwnerID, a.SecretID, a.VersionID, a.Action, a.ActorID, a.ServiceID, result, nullIfEmpty(a.OldVersionID), a.KEKID, a.DeploymentID, a.LeaseID)
	if err != nil {
		return secretWriteErr("append secret audit", err)
	}
	if command.RowsAffected() != 1 {
		return secret.ErrCorrupt
	}
	return nil
}
func (r *SecretRepository) AppendMaterializationAudit(ctx context.Context, audit secret.Audit) error {
	if r == nil || r.pool == nil {
		return secret.ErrInvalidInput
	}
	if audit.Action != "materialized" || (audit.Result != "success" && audit.Result != "denied" && audit.Result != "failed") || ((audit.ActorID == "") == (audit.ServiceID == "")) || audit.KEKID == "" {
		return secret.ErrInvalidInput
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin secret materialization audit: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = insertSecretAudit(ctx, tx, audit); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit secret materialization audit: %w", err)
	}
	return nil
}

// Rewrap is a narrow CAS persistence primitive for the later rotation service.
// It cannot alter value ciphertext, identity, scope, or AAD fields.
func (r *SecretRepository) Rewrap(ctx context.Context, scope secret.Scope, versionID string, expectedRevision uint64, envelope crypto.Envelope) error {
	if r == nil || r.pool == nil || expectedRevision > uint64(math.MaxInt64) || !validKEKID(envelope.KEKID) || envelope.Suite != crypto.SuiteAES256GCMRandomNonceV1 || envelope.AADVersion != crypto.AADFormatV1 || len(envelope.WrappedDEK) != 60 || expectedRevision == 0 {
		return secret.ErrInvalidInput
	}
	command, err := r.pool.Exec(ctx, `UPDATE secret_versions v SET kek_id=$8,wrapped_dek=$9,envelope_revision=envelope_revision+1 FROM secret_heads h WHERE v.id=$1 AND v.secret_id=h.id AND h.id=$2 AND h.owner_id=$3 AND h.project_id=$4 AND h.environment_id=$5 AND h.purpose=$6 AND h.delivery_scope=$7 AND v.state IN ('active','superseded') AND v.envelope_revision=$10 AND (v.kek_id<>$8 OR v.wrapped_dek<>$9)`, versionID, scope.SecretID, scope.OwnerID, scope.ProjectID, scope.EnvironmentID, scope.Purpose, scope.DeliveryScope, envelope.KEKID, envelope.WrappedDEK, int64(expectedRevision))
	if err != nil {
		return fmt.Errorf("rewrap secret version: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrRevisionConflict
	}
	return nil
}
func (r *SecretRepository) ListVersionsByKEK(ctx context.Context, kekID, cursor string, limit int) ([]secret.Version, error) {
	if r == nil || r.pool == nil || !validKEKID(kekID) || (cursor != "" && len(cursor) > 128) || limit < 1 || limit > secret.MaxRewrapBatchSize {
		return nil, secret.ErrInvalidInput
	}
	rows, err := r.pool.Query(ctx, `SELECT v.id,v.secret_id,v.owner_id,v.project_id,v.environment_id,v.version_number,v.state,v.suite,v.aad_version,v.kek_id,v.ciphertext,v.wrapped_dek,v.envelope_revision,v.created_at,h.purpose,h.delivery_scope FROM secret_versions v JOIN secret_heads h ON h.id=v.secret_id WHERE v.kek_id=$1 AND v.state IN ('active','superseded') AND v.id>$2 ORDER BY v.id ASC LIMIT $3`, kekID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list secret keyset: %w", err)
	}
	defer rows.Close()
	versions := make([]secret.Version, 0, limit)
	for rows.Next() {
		var v secret.Version
		var state string
		var revision int64
		if err := rows.Scan(&v.ID, &v.Scope.SecretID, &v.Scope.OwnerID, &v.Scope.ProjectID, &v.Scope.EnvironmentID, &v.Number, &state, &v.Envelope.Suite, &v.Envelope.AADVersion, &v.Envelope.KEKID, &v.Envelope.Ciphertext, &v.Envelope.WrappedDEK, &revision, &v.CreatedAt, &v.Scope.Purpose, &v.Scope.DeliveryScope); err != nil {
			return nil, fmt.Errorf("scan secret keyset: %w", err)
		}
		v.State, v.EnvelopeRevision = secret.State(state), uint64(revision)
		if revision < 1 || !validStoredVersion(v) {
			return nil, secret.ErrCorrupt
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secret keyset: %w", err)
	}
	return versions, nil
}
func (r *SecretRepository) CountVersionsByKEK(ctx context.Context, kekID string) (int64, error) {
	if r == nil || r.pool == nil || !validKEKID(kekID) {
		return 0, secret.ErrInvalidInput
	}
	var count int64
	// Revoked historical references intentionally remain counted: key removal
	// stays blocked until the explicit retention/cleanup policy permits it.
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM secret_versions WHERE kek_id=$1`, kekID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count secret key references: %w", err)
	}
	return count, nil
}
func validSecretMutation(m secret.Mutation, want uint64) error {
	v := m.Version
	if want > 0 && v.Number != want {
		return secret.ErrInvalidInput
	}
	if v.ID == "" || m.Audit.ID == "" || m.Audit.OwnerID != v.Scope.OwnerID || m.Audit.SecretID != v.Scope.SecretID || m.Audit.VersionID != v.ID || !validStoredVersion(v) {
		return secret.ErrInvalidInput
	}
	if m.Audit.Result != "success" || m.Audit.KEKID != v.Envelope.KEKID || ((m.Audit.ActorID == "") == (m.Audit.ServiceID == "")) || (want == 1 && m.Audit.OldVersionID != "") || (want == 0 && m.Audit.OldVersionID == "") || (want == 1 && (v.State != secret.StateActive || m.Audit.Action != "created")) || (want == 0 && (v.State != secret.StateActive || (m.Audit.Action != "replaced" && m.Audit.Action != "replaced_compromised"))) {
		return secret.ErrInvalidInput
	}
	return nil
}
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func validStoredVersion(v secret.Version) bool {
	e := v.Envelope
	return v.ID != "" && v.Number > 0 && (v.State == secret.StateActive || v.State == secret.StateSuperseded || v.State == secret.StateRevoked) && e.Suite == crypto.SuiteAES256GCMRandomNonceV1 && e.AADVersion == crypto.AADFormatV1 && validKEKID(e.KEKID) && len(e.Ciphertext) >= 28 && len(e.Ciphertext) <= crypto.MaxPlaintextBytes+28 && len(e.WrappedDEK) == 60
}
func secretWriteErr(op string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, op)
	}
	return fmt.Errorf("%s: %w", op, err)
}

var _ secret.Repository = (*SecretRepository)(nil)
var _ secret.RotationRepository = (*SecretRepository)(nil)
