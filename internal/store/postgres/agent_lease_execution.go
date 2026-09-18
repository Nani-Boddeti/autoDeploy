package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"autodeploy/internal/queue"

	"github.com/jackc/pgx/v5"
)

// ErrAgentLeaseCorruptState reports a durable inconsistency Claim discovers
// while resolving migration 000014's claim-time freeze that cannot be
// explained by an ordinary "not claimable" race -- concretely, a
// recomputed release config_digest that disagrees with the digest already
// stored for that deployment's release row. Spec (tasks/todo.md Slice 9a
// Step 2, "Also create the release row"): "a mismatch is a corrupt_state-
// class failure, not a silent overwrite." Unlike a missing config or a
// missing active secret version (which fail only their own candidate via
// ok=false), this aborts the whole Claim call: a release's config_digest
// disagreeing with what its own frozen inputs recompute means either this
// process or a previous one has a bug, and continuing to claim other
// candidates on the same corrupted assumption is not safe.
var ErrAgentLeaseCorruptState = errors.New("agent lease corrupt state")

// frozenExecutionSnapshot is the migration 000014 agent_job_attempt_execution
// row shape, shared by both the fresh-resolve path (attempt 1) and the
// copy-forward path (attempt N>1): the same Go value either comes from
// environment_runtime_configs (locked FOR SHARE) or is read back verbatim
// from the previous attempt's own frozen row.
type frozenExecutionSnapshot struct {
	ContainerPort        int
	ContainerScheme      string
	ReleaseStrategy      string
	DrainSeconds         int
	HealthPolicyRevision int
	CPUMillis            int
	MemoryBytes          int64
	RestartPolicy        string
	ConfigRevision       int64
}

// frozenBinding is one resolved (or copied-forward) secret-version
// reference: never a value, per ADR 0003.
type frozenBinding struct {
	VariableName    string
	DeliveryScope   string
	SecretVersionID string
}

// lockEnvironmentRuntimeConfig SHARE-locks environmentID's runtime config
// row -- ADR 0004 lock hierarchy, new position "1b": after the authorization
// rows (position 1, locked once in Claim before any candidate loop) and
// before the environment execution slot's UPDATE lock (position 3, the
// first statement tryClaim itself issues). FOR SHARE, never FOR UPDATE:
// freeze only reads this row, so an administrator's config CAS update
// (which takes only this row's ordinary exclusive lock, and no other row --
// see Step 3d's future admin-page write path) can never be blocked by a
// claim holding a *later* lock in the hierarchy, and a claim can never be
// stuck holding the slot's UPDATE lock while waiting on a concurrent config
// edit. Because this is the very first lock tryClaim acquires (before slot,
// job, attempt, or anything else), and an administrator's config update
// only ever locks this one row in its own single-statement transaction,
// neither side can be composing a wait cycle: at most one side blocks the
// other for the width of one short transaction, never a deadlock.
//
// environment_runtime_configs rows are permanently retained (migration
// 000014's no-delete trigger), so a row found missing here for a job whose
// attempt_count is already >0 would itself be a corrupt-state situation;
// in practice this can only be reached at all for a genuinely never-
// configured environment, because a prior attempt could only exist if this
// same check had already passed for it.
func lockEnvironmentRuntimeConfig(ctx context.Context, tx pgx.Tx, environmentID string) (frozenExecutionSnapshot, bool, error) {
	var cfg frozenExecutionSnapshot
	err := tx.QueryRow(ctx, `SELECT container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, revision FROM environment_runtime_configs WHERE environment_id=$1 FOR SHARE`, environmentID).
		Scan(&cfg.ContainerPort, &cfg.ContainerScheme, &cfg.ReleaseStrategy, &cfg.DrainSeconds, &cfg.HealthPolicyRevision, &cfg.CPUMillis, &cfg.MemoryBytes, &cfg.RestartPolicy, &cfg.ConfigRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozenExecutionSnapshot{}, false, nil
	}
	if err != nil {
		return frozenExecutionSnapshot{}, false, fmt.Errorf("lock environment runtime config: %w", err)
	}
	return cfg, true, nil
}

// resolveRuntimeSecretBindings resolves every binding_kind='secret' row of
// environmentID's environment_runtime_bindings to its exact
// secret_versions.id, via secret_heads.current_version_id, filtered to
// state='active'. literal_nonsecret bindings carry nothing to freeze here
// and are intentionally excluded -- they have no secret_version_id, and
// this slice never invents one.
//
// ok=false (nil error) means at least one binding's secret head has no
// current version, or that version is not active (a target-eligibility gap:
// missing config, a revoked/superseded reference the bind-time scope
// trigger did not and cannot catch, or any other unusable binding) -- the
// caller must fail only this one candidate, exactly like an occupied slot,
// never the whole Claim call and never with a default. Comparing the
// resolved row count against the total secret-binding count (rather than
// trusting an empty result) is what lets a single bad binding be detected
// even when other bindings for the same environment resolve fine.
func resolveRuntimeSecretBindings(ctx context.Context, tx pgx.Tx, environmentID string) ([]frozenBinding, bool, error) {
	var totalSecretBindings int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM environment_runtime_bindings WHERE environment_id=$1 AND binding_kind='secret'`, environmentID).Scan(&totalSecretBindings); err != nil {
		return nil, false, fmt.Errorf("count environment runtime secret bindings: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT b.variable_name, b.delivery_scope, sv.id FROM environment_runtime_bindings b JOIN secret_heads sh ON sh.id=b.secret_head_id JOIN secret_versions sv ON sv.id=sh.current_version_id AND sv.state='active' WHERE b.environment_id=$1 AND b.binding_kind='secret' ORDER BY b.variable_name`, environmentID)
	if err != nil {
		return nil, false, fmt.Errorf("resolve environment runtime secret bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]frozenBinding, 0, totalSecretBindings)
	for rows.Next() {
		var b frozenBinding
		if err := rows.Scan(&b.VariableName, &b.DeliveryScope, &b.SecretVersionID); err != nil {
			return nil, false, fmt.Errorf("scan environment runtime secret binding: %w", err)
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("resolve environment runtime secret bindings: %w", err)
	}
	if len(bindings) != totalSecretBindings {
		return nil, false, nil
	}
	return bindings, true, nil
}

// insertAttemptExecution durably freezes cfg as attemptID's
// agent_job_attempt_execution row (insert-once per migration 000014).
func insertAttemptExecution(ctx context.Context, tx pgx.Tx, attemptID int64, cfg frozenExecutionSnapshot) error {
	if _, err := tx.Exec(ctx, `INSERT INTO agent_job_attempt_execution(attempt_id, container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		attemptID, cfg.ContainerPort, cfg.ContainerScheme, cfg.ReleaseStrategy, cfg.DrainSeconds, cfg.HealthPolicyRevision, cfg.CPUMillis, cfg.MemoryBytes, cfg.RestartPolicy, cfg.ConfigRevision); err != nil {
		return fmt.Errorf("insert agent job attempt execution: %w", err)
	}
	return nil
}

// insertFrozenSecretVersions durably freezes bindings as attemptID's
// frozen_secret_versions rows (append-only per migration 000013/000014).
// This is the real implementation the claim-time freeze always needed;
// it replaces the previous freezeSecretVersions(ctx, tx, attemptID, nil)
// call site, which could never write a row because it was always invoked
// with a nil version list.
func insertFrozenSecretVersions(ctx context.Context, tx pgx.Tx, attemptID int64, bindings []frozenBinding) error {
	for _, b := range bindings {
		if _, err := tx.Exec(ctx, `INSERT INTO frozen_secret_versions(attempt_id, secret_version_id, variable_name, delivery_scope) VALUES ($1,$2,$3,$4)`, attemptID, b.SecretVersionID, b.VariableName, b.DeliveryScope); err != nil {
			return fmt.Errorf("freeze secret version: %w", err)
		}
	}
	return nil
}

// copyPreviousAttemptExecution implements the ADR 0003 retry rule: attempt
// N>1 of the same job copies attempt N-1's agent_job_attempt_execution row
// verbatim into its own attempt_id, rather than re-locking
// environment_runtime_configs and re-resolving it. No lock is taken on the
// source row: agent_job_attempt_execution is insert-once (migration 000014's
// no-update/no-delete triggers), so nothing can be concurrently mutating it
// for this unlocked read to race against.
func copyPreviousAttemptExecution(ctx context.Context, tx pgx.Tx, newAttemptID, prevAttemptID int64) (frozenExecutionSnapshot, error) {
	var cfg frozenExecutionSnapshot
	if err := tx.QueryRow(ctx, `SELECT container_port, container_scheme, release_strategy, drain_seconds, health_policy_revision, cpu_millis, memory_bytes, restart_policy, environment_runtime_config_revision FROM agent_job_attempt_execution WHERE attempt_id=$1`, prevAttemptID).
		Scan(&cfg.ContainerPort, &cfg.ContainerScheme, &cfg.ReleaseStrategy, &cfg.DrainSeconds, &cfg.HealthPolicyRevision, &cfg.CPUMillis, &cfg.MemoryBytes, &cfg.RestartPolicy, &cfg.ConfigRevision); err != nil {
		return frozenExecutionSnapshot{}, fmt.Errorf("load previous attempt %d execution for copy-forward: %w", prevAttemptID, err)
	}
	if err := insertAttemptExecution(ctx, tx, newAttemptID, cfg); err != nil {
		return frozenExecutionSnapshot{}, fmt.Errorf("copy agent job attempt execution forward: %w", err)
	}
	return cfg, nil
}

// copyPreviousFrozenSecretVersions implements the same ADR 0003 retry rule
// for frozen_secret_versions: copy attempt N-1's rows verbatim rather than
// re-resolving environment_runtime_bindings against (possibly rotated)
// current secret_heads.current_version_id values. No lock is needed on the
// source rows for the same reason as copyPreviousAttemptExecution:
// frozen_secret_versions is append-only.
func copyPreviousFrozenSecretVersions(ctx context.Context, tx pgx.Tx, newAttemptID, prevAttemptID int64) ([]frozenBinding, error) {
	rows, err := tx.Query(ctx, `SELECT secret_version_id, variable_name, delivery_scope FROM frozen_secret_versions WHERE attempt_id=$1 ORDER BY variable_name`, prevAttemptID)
	if err != nil {
		return nil, fmt.Errorf("load previous frozen secret versions for copy-forward: %w", err)
	}
	defer rows.Close()
	var bindings []frozenBinding
	for rows.Next() {
		var b frozenBinding
		if err := rows.Scan(&b.SecretVersionID, &b.VariableName, &b.DeliveryScope); err != nil {
			return nil, fmt.Errorf("scan previous frozen secret version: %w", err)
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load previous frozen secret versions for copy-forward: %w", err)
	}
	if err := insertFrozenSecretVersions(ctx, tx, newAttemptID, bindings); err != nil {
		return nil, fmt.Errorf("copy frozen secret versions forward: %w", err)
	}
	return bindings, nil
}

// computeConfigDigest is the canonical, stable encoding of a release's
// config_digest: sha256 over a length-prefixed concatenation of the frozen
// execution configuration's fields in a fixed order, followed by the frozen
// secret-version reference set sorted by variable_name (already the query
// order both resolveRuntimeSecretBindings and
// copyPreviousFrozenSecretVersions use, but sorted again here so the digest
// never depends on caller ordering). This mirrors
// internal/queue/job.go's payloadDigest exactly -- length-prefix each part
// with its byte length and a ':' separator before concatenating, so no
// field's content can ever be reinterpreted as a boundary between fields --
// rather than inventing a second canonicalization scheme in the same
// codebase. Bumping the leading "1" tag is how a future incompatible change
// to this encoding would be distinguished from today's.
func computeConfigDigest(cfg frozenExecutionSnapshot, bindings []frozenBinding) [32]byte {
	sorted := append([]frozenBinding(nil), bindings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].VariableName < sorted[j].VariableName })
	parts := []string{
		"1",
		strconv.Itoa(cfg.ContainerPort),
		cfg.ContainerScheme,
		cfg.ReleaseStrategy,
		strconv.Itoa(cfg.DrainSeconds),
		strconv.Itoa(cfg.HealthPolicyRevision),
		strconv.Itoa(cfg.CPUMillis),
		strconv.FormatInt(cfg.MemoryBytes, 10),
		cfg.RestartPolicy,
		strconv.FormatInt(cfg.ConfigRevision, 10),
		strconv.Itoa(len(sorted)),
	}
	for _, b := range sorted {
		parts = append(parts, b.VariableName, b.DeliveryScope, b.SecretVersionID)
	}
	buf := make([]byte, 0, 512)
	for _, part := range parts {
		buf = strconv.AppendInt(buf, int64(len(part)), 10)
		buf = append(buf, ':')
		buf = append(buf, part...)
	}
	return sha256.Sum256(buf)
}

// ensureRelease creates or reuses the one release row a deployment ever has
// (releases.UNIQUE(deployment_id)). On attempt 1 it inserts a new
// 'candidate' release with a NULL (write-once) image_digest -- the build
// step of a later slice sets it once BuildKit finishes -- and the given
// configDigest. On a retry the release already exists from attempt 1: this
// reuses it and asserts the freshly recomputed configDigest matches what is
// stored, per spec ("a mismatch is a corrupt_state-class failure, not a
// silent overwrite") -- never overwriting a stored digest, and never
// creating a second release row for the same deployment.
//
// releaseID reuses deploymentID directly rather than minting a new
// identifier: releases are already 1:1 with deployments (UNIQUE
// (deployment_id)), deployment_id already satisfies releases.id's octet-
// length CHECK, and it keeps this claim-time freeze free of any new ID-
// generation dependency.
func ensureRelease(ctx context.Context, tx pgx.Tx, deploymentID, environmentID, commitSHA, strategy string, configDigest [32]byte) (string, error) {
	releaseID := deploymentID
	var existingDigest []byte
	err := tx.QueryRow(ctx, `SELECT config_digest FROM releases WHERE deployment_id=$1`, deploymentID).Scan(&existingDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `INSERT INTO releases(id, deployment_id, environment_id, config_digest, commit_sha, strategy, state) VALUES ($1,$2,$3,$4,$5,$6,'candidate')`,
			releaseID, deploymentID, environmentID, configDigest[:], commitSHA, strategy); err != nil {
			return "", fmt.Errorf("insert release: %w", err)
		}
		return releaseID, nil
	}
	if err != nil {
		return "", fmt.Errorf("load release for deployment %s: %w", deploymentID, err)
	}
	if len(existingDigest) != 32 || [32]byte(existingDigest) != configDigest {
		return "", fmt.Errorf("%w: release %s config_digest disagrees with its own frozen inputs", ErrAgentLeaseCorruptState, releaseID)
	}
	return releaseID, nil
}

// toQueueSecretBindings converts the internal frozenBinding shape into the
// exported queue.SecretBinding the ClaimedLease wire path carries.
func toQueueSecretBindings(bindings []frozenBinding) []queue.SecretBinding {
	out := make([]queue.SecretBinding, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, queue.SecretBinding{VariableName: b.VariableName, DeliveryScope: b.DeliveryScope, SecretVersionID: b.SecretVersionID})
	}
	return out
}
