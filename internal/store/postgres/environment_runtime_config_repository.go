package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"
	"autodeploy/internal/healthgate"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvironmentRuntimeConfigRepository is the web admin surface (Slice 9a Step
// 3d) for reading and mutating one environment's runtime configuration
// (environment_runtime_configs, migration 000014) and its variable bindings
// (environment_runtime_bindings, migrations 000014/000015). It never
// resolves or returns a secret value: a secret binding surfaces only the
// referenced head's identity and its current version's number, never
// ciphertext, per ADR 0003's "Secret APIs and UI are write-only for
// values" rule.
type EnvironmentRuntimeConfigRepository struct {
	pool      *pgxpool.Pool
	failAfter func(string) error // test-only deterministic rollback seam
}

func NewEnvironmentRuntimeConfigRepository(pool *pgxpool.Pool) *EnvironmentRuntimeConfigRepository {
	return &EnvironmentRuntimeConfigRepository{pool: pool}
}

func (r *EnvironmentRuntimeConfigRepository) inject(stage string) error {
	if r.failAfter == nil {
		return nil
	}
	return r.failAfter(stage)
}

// ErrEnvironmentRuntimeConfigNotFound reports that environmentID has no
// environment_runtime_configs row yet (GetEnvironmentRuntimeConfig, or an
// UpdateEnvironmentRuntimeConfig call whose expectedRevision names a
// config that never existed).
var ErrEnvironmentRuntimeConfigNotFound = errors.New("environment runtime config not found")

// ErrEnvironmentRuntimeConfigConflict reports a stale-revision compare-and-
// swap: the caller's expectedRevision no longer matches the stored
// revision because another write landed first. The operator must reload
// and reapply their edit -- this is deliberately a distinct outcome from a
// generic error so the web layer can say "changed underneath you" rather
// than a bare failure.
var ErrEnvironmentRuntimeConfigConflict = errors.New("environment runtime config changed underneath you")

// ErrEnvironmentRuntimeConfigAlreadyExists reports that the caller tried to
// create a config (expectedRevision == nil) for an environment that
// already has one.
var ErrEnvironmentRuntimeConfigAlreadyExists = errors.New("environment runtime config already exists")

// ErrEnvironmentRuntimeBindingInvalid reports a rejected binding: the
// schema's own CHECK constraints or migration 000014's bind-time scope
// trigger refused it (cross-environment secret head, delivery-scope
// mismatch, a provider-purpose head, a malformed literal, or an unknown
// secret head). This is a user-facing validation outcome, never a 500.
var ErrEnvironmentRuntimeBindingInvalid = errors.New("environment runtime binding rejected")

// ErrEnvironmentRuntimeBindingNotFound reports that no binding exists for
// the given environment/variable pair (RemoveEnvironmentRuntimeBinding).
var ErrEnvironmentRuntimeBindingNotFound = errors.New("environment runtime binding not found")

// variableNamePattern mirrors migration 000014/000015's
// environment_runtime_bindings.variable_name and frozen_*.variable_name
// CHECK exactly, so an invalid name is refused here before ever reaching
// the database.
var variableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validVariableName(name string) bool {
	return len(name) >= 1 && len(name) <= 128 && variableNamePattern.MatchString(name)
}

// validLiteralValue mirrors migration 000015's literal_value CHECK: 1..4096
// bytes, valid UTF-8, no control characters (0x00-0x1f, 0x7f).
func validLiteralValue(value string) bool {
	if len(value) < 1 || len(value) > 4096 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validDeliveryScope(scope string) bool { return scope == "build" || scope == "runtime" }

// RuntimeConfigInput is the operator-editable subset of
// environment_runtime_configs. health_policy_revision is not included: V1
// hard-codes ADR 0005's default health gate and migration 000014 pins it to
// the literal value 1, so there is nothing for an operator to choose yet.
type RuntimeConfigInput struct {
	ContainerPort   int
	ContainerScheme string
	ReleaseStrategy string
	DrainSeconds    int
	CPUMillis       int
	MemoryBytes     int64
	RestartPolicy   string
	// HealthGatePath and HealthGateHost are migration 000017's Slice 10
	// Step 0 additions: the exact configured health-gate probe path and
	// Host header (ADR 0005's health-gate contract). HealthGateHost is
	// required (decision D2): there is no sensible fallback once a failed
	// gate with the container running classifies permanent_configuration
	// (never retried).
	HealthGatePath string
	HealthGateHost string
}

// valid mirrors migration 000014/000017's environment_runtime_configs CHECK
// constraints exactly, so an out-of-bounds value is refused here, before
// ever reaching the database (and before any lock is taken).
func (in RuntimeConfigInput) valid() bool {
	return in.ContainerPort >= 1 && in.ContainerPort <= 65535 &&
		(in.ContainerScheme == "http" || in.ContainerScheme == "https") &&
		(in.ReleaseStrategy == "blue_green" || in.ReleaseStrategy == "stop_then_start") &&
		in.DrainSeconds >= 1 && in.DrainSeconds <= 300 &&
		in.CPUMillis > 0 && in.CPUMillis <= 256000 &&
		in.MemoryBytes > 0 && in.MemoryBytes <= 549755813888 &&
		(in.RestartPolicy == "unless-stopped" || in.RestartPolicy == "on-failure") &&
		healthgate.ValidPath(in.HealthGatePath) &&
		healthgate.ValidHost(in.HealthGateHost)
}

// RuntimeConfig is the full read view of one environment's runtime
// configuration.
type RuntimeConfig struct {
	EnvironmentID string
	RuntimeConfigInput
	HealthPolicyRevision int
	Revision             int64
	UpdatedAt            time.Time
}

// RuntimeBinding is the read view of one environment_runtime_bindings row.
// For binding_kind=="secret", SecretHeadID/SecretVersionNumber identify the
// bound head and its current version's number -- never a value.
// SecretVersionNumber is 0 when the head currently has no active version
// (an administrator must still prove up front that this fails Claim: 0
// here on the page means "not active", matching resolveRuntimeSecretBindings's
// own eligibility gap). For binding_kind=="literal_nonsecret", LiteralValue
// is the plain value itself -- ADR 0003 classifies it as non-secret
// protected metadata, not a secret, so displaying it is safe and expected.
type RuntimeBinding struct {
	VariableName        string
	BindingKind         string
	DeliveryScope       string
	SecretHeadID        string
	SecretVersionNumber int64
	LiteralValue        string
	CreatedAt           time.Time
}

// BindingInput is the operator-supplied binding to add.
type BindingInput struct {
	VariableName  string
	BindingKind   string // "secret" | "literal_nonsecret"
	DeliveryScope string // "build" | "runtime"
	SecretHeadID  string // required iff BindingKind == "secret"
	LiteralValue  string // required iff BindingKind == "literal_nonsecret"
}

func (in BindingInput) valid() bool {
	if !validVariableName(in.VariableName) || !validDeliveryScope(in.DeliveryScope) {
		return false
	}
	switch in.BindingKind {
	case "secret":
		return github.ValidOpaqueID(in.SecretHeadID) && in.LiteralValue == ""
	case "literal_nonsecret":
		return in.SecretHeadID == "" && validLiteralValue(in.LiteralValue)
	default:
		return false
	}
}

// environmentOwnedBy proves environmentID belongs to principal's owner
// without locking anything: environment_id carries no owner column of its
// own anywhere in this schema (migration 000014's comment: "this codebase
// has no projects/environments tables yet"), so ownership is derived the
// same way agent_lease_repository.go's MarkReconciled derives it -- from
// agent_jobs.owner_id, the one durable place an environment_id is ever
// paired with an owner independent of any config row. Unlike
// MarkReconciled, this is a plain SELECT with no FOR SHARE/FOR UPDATE: it
// only ever needs to observe that at least one such pairing exists, never
// to hold it stable across a multi-statement decision, and agent_jobs is
// on the explicit list of tables this write path must never lock (ADR 0004
// lock-hierarchy deadlock-freedom argument -- see
// lockEnvironmentRuntimeConfigForCAS below). A plain SELECT takes no row
// lock in PostgreSQL's MVCC model, so this cannot participate in a wait
// cycle with a concurrent Claim transaction.
//
// Known limitation (recorded rather than silently assumed away): an
// environment that has never had a single agent_jobs row under any owner
// cannot be configured through this page yet, because there is nowhere
// else in this schema an owner/environment pairing could come from. This
// codebase has no projects/environments table (000014's own words); adding
// one is out of Step 3d's scope.
func environmentOwnedBy(ctx context.Context, tx pgx.Tx, environmentID, ownerID string) (bool, error) {
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_jobs WHERE environment_id=$1 AND owner_id=$2)`, environmentID, ownerID).Scan(&owned); err != nil {
		return false, fmt.Errorf("check environment ownership: %w", err)
	}
	return owned, nil
}

// GetEnvironmentRuntimeConfig returns environmentID's runtime configuration
// (found=false if none exists yet) and every current binding, owner-scoped
// and re-authenticating principal's session live, matching every other
// admin page in this package.
func (r *EnvironmentRuntimeConfigRepository) GetEnvironmentRuntimeConfig(ctx context.Context, principal auth.Principal, environmentID string) (RuntimeConfig, []RuntimeBinding, bool, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) {
		return RuntimeConfig{}, nil, false, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RuntimeConfig{}, nil, false, fmt.Errorf("begin get environment runtime config: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return RuntimeConfig{}, nil, false, err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return RuntimeConfig{}, nil, false, err
	}
	if !owned {
		return RuntimeConfig{}, nil, false, ErrServerAuthorizationDenied
	}

	cfg, found, err := readRuntimeConfig(ctx, tx, environmentID)
	if err != nil {
		return RuntimeConfig{}, nil, false, err
	}
	bindings, err := readRuntimeBindings(ctx, tx, environmentID)
	if err != nil {
		return RuntimeConfig{}, nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeConfig{}, nil, false, fmt.Errorf("commit get environment runtime config: %w", err)
	}
	return cfg, bindings, found, nil
}

func readRuntimeConfig(ctx context.Context, tx pgx.Tx, environmentID string) (RuntimeConfig, bool, error) {
	var cfg RuntimeConfig
	err := tx.QueryRow(ctx, `SELECT environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_path, health_gate_host, revision, updated_at FROM environment_runtime_configs WHERE environment_id=$1`, environmentID).
		Scan(&cfg.EnvironmentID, &cfg.ContainerPort, &cfg.ContainerScheme, &cfg.ReleaseStrategy, &cfg.DrainSeconds, &cfg.CPUMillis, &cfg.MemoryBytes, &cfg.RestartPolicy, &cfg.HealthPolicyRevision, &cfg.HealthGatePath, &cfg.HealthGateHost, &cfg.Revision, &cfg.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeConfig{}, false, nil
	}
	if err != nil {
		return RuntimeConfig{}, false, fmt.Errorf("read environment runtime config: %w", err)
	}
	return cfg, true, nil
}

func readRuntimeBindings(ctx context.Context, tx pgx.Tx, environmentID string) ([]RuntimeBinding, error) {
	rows, err := tx.Query(ctx, `SELECT b.variable_name, b.binding_kind, b.delivery_scope, coalesce(b.secret_head_id,''), coalesce(sv.version_number,0), coalesce(b.literal_value,''), b.created_at
		FROM environment_runtime_bindings b
		LEFT JOIN secret_heads sh ON sh.id = b.secret_head_id
		LEFT JOIN secret_versions sv ON sv.id = sh.current_version_id AND sv.state='active'
		WHERE b.environment_id=$1 ORDER BY b.variable_name`, environmentID)
	if err != nil {
		return nil, fmt.Errorf("read environment runtime bindings: %w", err)
	}
	defer rows.Close()
	var bindings []RuntimeBinding
	for rows.Next() {
		var b RuntimeBinding
		if err := rows.Scan(&b.VariableName, &b.BindingKind, &b.DeliveryScope, &b.SecretHeadID, &b.SecretVersionNumber, &b.LiteralValue, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan environment runtime binding: %w", err)
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate environment runtime bindings: %w", err)
	}
	return bindings, nil
}

// CreateEnvironmentRuntimeConfig inserts the first environment_runtime_configs
// row for environmentID (revision starts at 0, health_policy_revision
// pinned to 1 -- the only value migration 000014 permits). Fails with
// ErrEnvironmentRuntimeConfigAlreadyExists if one already exists; callers
// that mean to edit an existing config must use
// UpdateEnvironmentRuntimeConfig instead.
func (r *EnvironmentRuntimeConfigRepository) CreateEnvironmentRuntimeConfig(ctx context.Context, principal auth.Principal, environmentID string, in RuntimeConfigInput) (RuntimeConfig, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) || !in.valid() {
		return RuntimeConfig{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("begin create environment runtime config: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock order (ADR 0004 hierarchy, matching Step 2's tryClaim exactly):
	// authorization rows (admin session/user) first, then the
	// environment_runtime_configs row itself, and nothing else -- see the
	// long comment on lockEnvironmentRuntimeConfig in
	// agent_lease_execution.go for the deadlock-freedom argument this
	// preserves. environmentOwnedBy below takes no lock at all (plain
	// SELECT), so it never becomes a third position in this order.
	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return RuntimeConfig{}, err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if !owned {
		return RuntimeConfig{}, ErrServerAuthorizationDenied
	}
	actor := administratorActor(principal)

	// Single statement: this INSERT is the entire write to
	// environment_runtime_configs. It takes and holds this row's exclusive
	// lock from execution to commit, and nothing else in this transaction
	// ever locks a row in environment_slots, agent_jobs, agent_job_attempts,
	// servers, or server_credentials -- so it can never be a participant in
	// a wait cycle with a Claim transaction, which locks this same row FOR
	// SHARE at hierarchy position 1b before locking anything later.
	var cfg RuntimeConfig
	cfg.EnvironmentID = environmentID
	cfg.RuntimeConfigInput = in
	cfg.HealthPolicyRevision = 1
	err = tx.QueryRow(ctx, `INSERT INTO environment_runtime_configs(environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision, health_gate_path, health_gate_host, revision) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,1,$9,$10,0) RETURNING revision, updated_at`,
		environmentID, in.ContainerPort, in.ContainerScheme, in.ReleaseStrategy, in.DrainSeconds, in.CPUMillis, in.MemoryBytes, in.RestartPolicy, in.HealthGatePath, in.HealthGateHost).Scan(&cfg.Revision, &cfg.UpdatedAt)
	if isUniqueViolation(err) {
		// The INSERT above already returned a genuine PostgreSQL error,
		// which poisons the rest of this transaction until rollback -- a
		// second statement in the same transaction (even an unrelated
		// INSERT into the audit table) would fail with "current
		// transaction is aborted". Roll back explicitly, then write the
		// failure audit row as its own separate operation against the
		// pool, matching every other DB-error branch in this file.
		_ = tx.Rollback(ctx)
		if auditErr := insertEnvironmentRuntimeConfigAuditEvent(ctx, r.pool, actor, principal.OwnerID, environmentID, "config_created", nil, nil, "failure", ""); auditErr != nil {
			return RuntimeConfig{}, auditErr
		}
		return RuntimeConfig{}, ErrEnvironmentRuntimeConfigAlreadyExists
	}
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("insert environment runtime config: %w", err)
	}
	if err = r.inject("config_insert"); err != nil {
		return RuntimeConfig{}, err
	}
	if err = insertEnvironmentRuntimeConfigAuditEvent(ctx, tx, actor, principal.OwnerID, environmentID, "config_created", nil, &cfg.Revision, "success", ""); err != nil {
		return RuntimeConfig{}, err
	}
	if err = r.inject("audit_insert"); err != nil {
		return RuntimeConfig{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeConfig{}, fmt.Errorf("commit create environment runtime config: %w", err)
	}
	return cfg, nil
}

// UpdateEnvironmentRuntimeConfig applies a compare-and-swap edit: the write
// only takes effect if expectedRevision still matches the stored revision.
// A stale expectedRevision affects zero rows and is reported distinctly
// (ErrEnvironmentRuntimeConfigConflict) from a config that never existed
// (ErrEnvironmentRuntimeConfigNotFound), so the web layer can tell an
// operator "this changed underneath you" rather than a generic failure.
func (r *EnvironmentRuntimeConfigRepository) UpdateEnvironmentRuntimeConfig(ctx context.Context, principal auth.Principal, environmentID string, in RuntimeConfigInput, expectedRevision int64) (RuntimeConfig, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) || !in.valid() || expectedRevision < 0 {
		return RuntimeConfig{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("begin update environment runtime config: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return RuntimeConfig{}, err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if !owned {
		return RuntimeConfig{}, ErrServerAuthorizationDenied
	}
	actor := administratorActor(principal)

	// Single statement, exactly like Step 2's raw-SQL race fixture
	// (TestAgentLeaseClaimRacesAdministratorConfigUpdateNoDeadlock) proved
	// safe: the CAS predicate and the mutation happen in one UPDATE, so the
	// row's exclusive lock is held for one round trip, not a read followed
	// by a second write statement. See CreateEnvironmentRuntimeConfig's
	// comment for the full lock-order argument; it applies unchanged here.
	var cfg RuntimeConfig
	cfg.EnvironmentID = environmentID
	cfg.RuntimeConfigInput = in
	cfg.HealthPolicyRevision = 1
	err = tx.QueryRow(ctx, `UPDATE environment_runtime_configs SET container_port=$2, container_scheme=$3, release_strategy=$4, drain_seconds=$5, cpu_millis=$6, memory_bytes=$7, restart_policy=$8, health_gate_path=$9, health_gate_host=$10, revision=revision+1, updated_at=clock_timestamp() WHERE environment_id=$1 AND revision=$11 RETURNING revision, updated_at`,
		environmentID, in.ContainerPort, in.ContainerScheme, in.ReleaseStrategy, in.DrainSeconds, in.CPUMillis, in.MemoryBytes, in.RestartPolicy, in.HealthGatePath, in.HealthGateHost, expectedRevision).Scan(&cfg.Revision, &cfg.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Zero rows affected: either the row does not exist at all, or it
		// exists but expectedRevision is stale. Distinguish with a second,
		// unlocked read of the very row this statement already targeted --
		// no new table enters the lock order, and by the time we reach here
		// the UPDATE's own lock attempt has already released (it matched no
		// row, so it held nothing).
		_, found, readErr := readRuntimeConfig(ctx, tx, environmentID)
		if readErr != nil {
			return RuntimeConfig{}, readErr
		}
		if auditErr := insertEnvironmentRuntimeConfigAuditEvent(ctx, tx, actor, principal.OwnerID, environmentID, "config_updated", nil, nil, "failure", ""); auditErr != nil {
			return RuntimeConfig{}, auditErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RuntimeConfig{}, fmt.Errorf("commit update environment runtime config (rejected): %w", commitErr)
		}
		if !found {
			return RuntimeConfig{}, ErrEnvironmentRuntimeConfigNotFound
		}
		return RuntimeConfig{}, ErrEnvironmentRuntimeConfigConflict
	}
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("update environment runtime config: %w", err)
	}
	if err = r.inject("config_update"); err != nil {
		return RuntimeConfig{}, err
	}
	if err = insertEnvironmentRuntimeConfigAuditEvent(ctx, tx, actor, principal.OwnerID, environmentID, "config_updated", nil, &cfg.Revision, "success", ""); err != nil {
		return RuntimeConfig{}, err
	}
	if err = r.inject("audit_insert"); err != nil {
		return RuntimeConfig{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeConfig{}, fmt.Errorf("commit update environment runtime config: %w", err)
	}
	return cfg, nil
}

// AddEnvironmentRuntimeBinding inserts one variable binding. The schema's
// own CHECK constraints (kind/head-presence agreement, literal bounds) and
// migration 000014's bind-time scope trigger (cross-environment reference,
// delivery-scope mismatch, provider-purpose head) are the actual
// authorities; a rejection from any of them is translated to
// ErrEnvironmentRuntimeBindingInvalid rather than surfacing as a bare
// PostgreSQL error, per the spec's "surface their rejections as user-facing
// validation errors rather than 500s."
//
// This never locks environment_runtime_configs (or anything else besides
// the authorization rows and the new binding row itself), so it carries no
// lock-order obligation at all -- Claim's freeze reads
// environment_runtime_bindings with a plain, unlocked SELECT (see
// resolveRuntimeSecretBindings/resolveRuntimeLiteralBindings in
// agent_lease_execution.go), never FOR SHARE/FOR UPDATE.
func (r *EnvironmentRuntimeConfigRepository) AddEnvironmentRuntimeBinding(ctx context.Context, principal auth.Principal, environmentID string, in BindingInput) (RuntimeBinding, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) || !in.valid() {
		return RuntimeBinding{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RuntimeBinding{}, fmt.Errorf("begin add environment runtime binding: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return RuntimeBinding{}, err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return RuntimeBinding{}, err
	}
	if !owned {
		return RuntimeBinding{}, ErrServerAuthorizationDenied
	}
	actor := administratorActor(principal)

	var secretHeadID, literalValue *string
	if in.BindingKind == "secret" {
		secretHeadID = &in.SecretHeadID
	} else {
		literalValue = &in.LiteralValue
	}
	_, err = tx.Exec(ctx, `INSERT INTO environment_runtime_bindings(environment_id, variable_name, binding_kind, secret_head_id, delivery_scope, literal_value) VALUES ($1,$2,$3,$4,$5,$6)`,
		environmentID, in.VariableName, in.BindingKind, secretHeadID, in.DeliveryScope, literalValue)
	if isCheckViolation(err) || isForeignKeyViolation(err) || isRaiseException(err) || isUniqueViolation(err) {
		// A genuine PostgreSQL error (CHECK/FK/trigger RAISE EXCEPTION/
		// unique-violation on the (environment_id, variable_name) PK)
		// poisons this transaction -- roll back and write the failure
		// audit row as its own separate operation, exactly like
		// CreateEnvironmentRuntimeConfig's isUniqueViolation branch above.
		_ = tx.Rollback(ctx)
		if auditErr := insertEnvironmentRuntimeConfigAuditEvent(ctx, r.pool, actor, principal.OwnerID, environmentID, "binding_added", &in.VariableName, nil, "failure", ""); auditErr != nil {
			return RuntimeBinding{}, auditErr
		}
		return RuntimeBinding{}, ErrEnvironmentRuntimeBindingInvalid
	}
	if err != nil {
		return RuntimeBinding{}, fmt.Errorf("insert environment runtime binding: %w", err)
	}
	if err = r.inject("binding_insert"); err != nil {
		return RuntimeBinding{}, err
	}
	if err = insertEnvironmentRuntimeConfigAuditEvent(ctx, tx, actor, principal.OwnerID, environmentID, "binding_added", &in.VariableName, nil, "success", ""); err != nil {
		return RuntimeBinding{}, err
	}
	if err = r.inject("audit_insert"); err != nil {
		return RuntimeBinding{}, err
	}
	bindings, err := readRuntimeBindings(ctx, tx, environmentID)
	if err != nil {
		return RuntimeBinding{}, err
	}
	var result RuntimeBinding
	for _, b := range bindings {
		if b.VariableName == in.VariableName {
			result = b
			break
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeBinding{}, fmt.Errorf("commit add environment runtime binding: %w", err)
	}
	return result, nil
}

// RemoveEnvironmentRuntimeBinding deletes one variable binding.
func (r *EnvironmentRuntimeConfigRepository) RemoveEnvironmentRuntimeBinding(ctx context.Context, principal auth.Principal, environmentID, variableName string) error {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) || !validVariableName(variableName) {
		return ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin remove environment runtime binding: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return err
	}
	if !owned {
		return ErrServerAuthorizationDenied
	}
	actor := administratorActor(principal)

	tag, err := tx.Exec(ctx, `DELETE FROM environment_runtime_bindings WHERE environment_id=$1 AND variable_name=$2`, environmentID, variableName)
	if err != nil {
		return fmt.Errorf("delete environment runtime binding: %w", err)
	}
	if tag.RowsAffected() != 1 {
		if auditErr := insertEnvironmentRuntimeConfigAuditEvent(ctx, tx, actor, principal.OwnerID, environmentID, "binding_removed", &variableName, nil, "failure", ""); auditErr != nil {
			return auditErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return fmt.Errorf("commit remove environment runtime binding (not found): %w", commitErr)
		}
		return ErrEnvironmentRuntimeBindingNotFound
	}
	if err = r.inject("binding_delete"); err != nil {
		return err
	}
	if err = insertEnvironmentRuntimeConfigAuditEvent(ctx, tx, actor, principal.OwnerID, environmentID, "binding_removed", &variableName, nil, "success", ""); err != nil {
		return err
	}
	if err = r.inject("audit_insert"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit remove environment runtime binding: %w", err)
	}
	return nil
}

// isRaiseException reports a plpgsql RAISE EXCEPTION with no explicit
// SQLSTATE, i.e. migration 000014's
// validate_environment_runtime_binding_scope trigger (cross-environment
// reference, delivery-scope mismatch, provider-purpose head): PostgreSQL
// assigns these SQLSTATE P0001 ("raise_exception").
func isRaiseException(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "P0001"
}

// pgxExecer is the common subset of pgx.Tx and *pgxpool.Pool this file
// needs for an audit insert: satisfied by either, so a failure audit row
// following a genuine PostgreSQL error (which has already aborted the
// enclosing transaction -- PostgreSQL poisons a transaction after any
// server-side error until rollback) can be written as its own, separate
// operation against the pool, while a success (or an application-level
// rejection that never touched the server, like a stale-revision
// zero-rows UPDATE) still writes its audit row atomically inside the same
// transaction as the mutation.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertEnvironmentRuntimeConfigAuditEvent(ctx context.Context, exec pgxExecer, actor AuditActor, ownerID, environmentID, action string, variableName *string, resultingRevision *int64, result, reason string) error {
	if _, err := exec.Exec(ctx, `INSERT INTO environment_runtime_config_audit_events(actor_type, actor_id, owner_id, environment_id, action, variable_name, resulting_revision, result, reason) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		string(actor.Type), actor.ID, ownerID, environmentID, action, variableName, resultingRevision, result, reason); err != nil {
		return fmt.Errorf("insert environment runtime config audit event: %w", err)
	}
	return nil
}
