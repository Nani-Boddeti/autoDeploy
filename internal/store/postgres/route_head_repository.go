package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"
	"autodeploy/internal/healthgate"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RouteHeadRepository is the web admin surface (Slice 10 Step 10a) for
// reading one environment's route head (migration 000018's route_heads) and
// assigning it a hostname. It ships no activation logic at all: Step 10a's
// approved scope is schema plus a minimal, audited hostname-assignment
// write, and active_release_id is never set by any method here -- Step 10c
// owns every write to that column.
//
// Lock order (ADR 0004 hierarchy, Decision D5): this repository locks
// authorization rows (admin session/user, hierarchy position 1) and then
// the route_heads row for environmentID, and nothing else. D5 slots
// route-head rows immediately after deployment aggregate heads (position 6)
// and before secret-version/frozen-reference rows (position 7) in that
// hierarchy; every method here only ever reaches position 1 and this new
// position, so it can never become a participant in a wait cycle with a
// Claim transaction (which never locks route_heads at all in this slice)
// or with a future Step 10c activation transaction (which will lock
// authorization rows, then deployment/attempt rows, then this same
// route_heads row, in that same relative order). This comment documents the
// slot against the ADR 0004 text as written; it does not amend the ADR
// itself (that is explicitly out of this step's scope per tasks/todo.md).
type RouteHeadRepository struct {
	pool      *pgxpool.Pool
	failAfter func(string) error // test-only deterministic rollback seam
}

func NewRouteHeadRepository(pool *pgxpool.Pool) *RouteHeadRepository {
	return &RouteHeadRepository{pool: pool}
}

func (r *RouteHeadRepository) inject(stage string) error {
	if r.failAfter == nil {
		return nil
	}
	return r.failAfter(stage)
}

// ErrRouteHeadNotFound reports that environmentID has no route_heads row yet
// (GetRouteHead, or an AssignRouteHostname call whose expectedRevision names
// a route head that never existed).
var ErrRouteHeadNotFound = errors.New("route head not found")

// ErrRouteHeadAlreadyExists reports that the caller tried to create a route
// head (expectedRevision == nil) for an environment that already has one.
var ErrRouteHeadAlreadyExists = errors.New("route head already exists")

// ErrRouteHeadConflict reports a stale-revision compare-and-swap: the
// caller's expectedRevision no longer matches the stored route_revision
// because another write landed first.
var ErrRouteHeadConflict = errors.New("route head changed underneath you")

// ErrRouteHeadHostnameTaken reports that hostname is already assigned to a
// different environment's route head. migration 000018's UNIQUE(hostname)
// is the actual authority; this translates that violation into a
// user-facing validation outcome instead of a bare 500, including when two
// concurrent AssignHostname calls race for the same hostname on different
// environments -- exactly one commits, and the other observes this error
// from the real unique-constraint violation, never a raw SQLSTATE 23505.
var ErrRouteHeadHostnameTaken = errors.New("hostname is already assigned to another environment")

// ErrRouteHeadHostnameImmutable reports that the caller tried to change the
// hostname on a route head whose active_release_id is already set. Changing
// a live route's hostname requires the agent to move traffic first (Step
// 10c/10d, deferred); this slice refuses the write outright rather than
// silently desynchronizing the database from what Traefik is still serving.
var ErrRouteHeadHostnameImmutable = errors.New("route head hostname cannot change while a release is active")

// RouteHead is the read view of one environment_id's route_heads row.
type RouteHead struct {
	EnvironmentID          string
	Hostname               string
	RouteRevision          int64
	ActiveReleaseID        string // "" if NULL
	ReconciliationRequired bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func readRouteHead(ctx context.Context, tx pgx.Tx, environmentID string) (RouteHead, bool, error) {
	var head RouteHead
	var activeReleaseID *string
	err := tx.QueryRow(ctx, `SELECT environment_id, hostname, route_revision, active_release_id, reconciliation_required, created_at, updated_at FROM route_heads WHERE environment_id=$1`, environmentID).
		Scan(&head.EnvironmentID, &head.Hostname, &head.RouteRevision, &activeReleaseID, &head.ReconciliationRequired, &head.CreatedAt, &head.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RouteHead{}, false, nil
	}
	if err != nil {
		return RouteHead{}, false, fmt.Errorf("read route head: %w", err)
	}
	if activeReleaseID != nil {
		head.ActiveReleaseID = *activeReleaseID
	}
	return head, true, nil
}

// GetRouteHead returns environmentID's route head (found=false if none
// exists yet), owner-scoped and re-authenticating principal's session live,
// matching EnvironmentRuntimeConfigRepository.GetEnvironmentRuntimeConfig
// exactly.
func (r *RouteHeadRepository) GetRouteHead(ctx context.Context, principal auth.Principal, environmentID string) (RouteHead, bool, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) {
		return RouteHead{}, false, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RouteHead{}, false, fmt.Errorf("begin get route head: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return RouteHead{}, false, err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return RouteHead{}, false, err
	}
	if !owned {
		return RouteHead{}, false, ErrServerAuthorizationDenied
	}
	head, found, err := readRouteHead(ctx, tx, environmentID)
	if err != nil {
		return RouteHead{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RouteHead{}, false, fmt.Errorf("commit get route head: %w", err)
	}
	return head, found, nil
}

// normalizeRouteHostname lowercases hostname (ASCII only -- a route
// hostname is a DNS name, which migration 000018's CHECK already restricts
// to [a-z0-9.-] once normalized, so a Unicode-aware fold is neither needed
// nor safe: it must not fold any byte outside ASCII into something the
// CHECK would then accept).
func normalizeRouteHostname(hostname string) string {
	return strings.ToLower(hostname)
}

// AssignRouteHostname creates environmentID's route head (expectedRevision ==
// nil, starting at route_revision 0, matching
// CreateEnvironmentRuntimeConfig's first-write convention) or CAS-updates
// its hostname (expectedRevision != nil). hostname is normalized to
// lowercase before validation and before it ever reaches the database, so
// the UNIQUE(hostname) constraint and ValidRouteHostname's lowercase-only
// grammar both see the same canonical form the operator intended.
//
// Policy for this slice (documented on ErrRouteHeadHostnameImmutable): once
// active_release_id is non-NULL, a hostname change is refused with a typed
// error, both here (before any lock is taken, for the common case) and
// again by migration 000018's validate_route_head_update trigger (defense
// in depth against a future direct-SQL path that bypasses this method).
func (r *RouteHeadRepository) AssignRouteHostname(ctx context.Context, principal auth.Principal, environmentID, hostname string, expectedRevision *int64) (RouteHead, error) {
	hostname = normalizeRouteHostname(hostname)
	if r == nil || r.pool == nil || principal.Validate() != nil || !github.ValidOpaqueID(environmentID) || !healthgate.ValidRouteHostname(hostname) {
		return RouteHead{}, ErrServerAuthorizationDenied
	}
	if expectedRevision != nil && *expectedRevision < 0 {
		return RouteHead{}, ErrServerAuthorizationDenied
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RouteHead{}, fmt.Errorf("begin assign route head hostname: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock order: authorization rows (admin session/user), then the
	// route_heads row itself via the single INSERT/UPDATE statement below --
	// see the type-level comment for the full ADR 0004/D5 argument.
	// environmentOwnedBy takes no lock at all (plain SELECT), exactly as in
	// environment_runtime_config_repository.go.
	if err = lockAdministratorSession(ctx, tx, principal); err != nil {
		return RouteHead{}, err
	}
	owned, err := environmentOwnedBy(ctx, tx, environmentID, principal.OwnerID)
	if err != nil {
		return RouteHead{}, err
	}
	if !owned {
		return RouteHead{}, ErrServerAuthorizationDenied
	}
	actor := administratorActor(principal)

	if expectedRevision == nil {
		return r.createRouteHead(ctx, tx, actor, principal.OwnerID, environmentID, hostname)
	}
	return r.updateRouteHeadHostname(ctx, tx, actor, principal.OwnerID, environmentID, hostname, *expectedRevision)
}

func (r *RouteHeadRepository) createRouteHead(ctx context.Context, tx pgx.Tx, actor AuditActor, ownerID, environmentID, hostname string) (RouteHead, error) {
	var head RouteHead
	head.EnvironmentID = environmentID
	head.Hostname = hostname
	err := tx.QueryRow(ctx, `INSERT INTO route_heads(environment_id, hostname, route_revision) VALUES ($1,$2,0) RETURNING route_revision, created_at, updated_at`,
		environmentID, hostname).Scan(&head.RouteRevision, &head.CreatedAt, &head.UpdatedAt)
	if isUniqueViolation(err) {
		// Poisons the transaction (a genuine PostgreSQL error) -- roll back
		// explicitly, then write the failure audit row as its own separate
		// operation against the pool, exactly like
		// CreateEnvironmentRuntimeConfig's isUniqueViolation branch.
		_ = tx.Rollback(ctx)
		typed := ErrRouteHeadHostnameTaken
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "route_heads_pkey" {
			typed = ErrRouteHeadAlreadyExists
		}
		if auditErr := insertRouteHeadAuditEvent(ctx, r.pool, actor, ownerID, environmentID, "hostname_assigned", &hostname, nil, "failure", ""); auditErr != nil {
			return RouteHead{}, auditErr
		}
		return RouteHead{}, typed
	}
	if err != nil {
		return RouteHead{}, fmt.Errorf("insert route head: %w", err)
	}
	if err = r.inject("route_head_insert"); err != nil {
		return RouteHead{}, err
	}
	if err = insertRouteHeadAuditEvent(ctx, tx, actor, ownerID, environmentID, "hostname_assigned", &hostname, &head.RouteRevision, "success", ""); err != nil {
		return RouteHead{}, err
	}
	if err = insertRouteEvent(ctx, tx, environmentID, "hostname_assigned", head.RouteRevision, &hostname, nil); err != nil {
		return RouteHead{}, err
	}
	if err = r.inject("audit_insert"); err != nil {
		return RouteHead{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RouteHead{}, fmt.Errorf("commit create route head: %w", err)
	}
	return head, nil
}

// updateRouteHeadHostname locks the target row first (SELECT ... FOR
// UPDATE), then decides not-found / immutable (active_release_id set) /
// stale-revision entirely from that locked snapshot, before ever issuing
// the mutating UPDATE. This is deliberate: an earlier version issued the
// CAS predicate (revision + active_release_id IS NULL) directly in the
// UPDATE's WHERE clause and, on zero rows affected, ran a second *unlocked*
// SELECT afterward to classify the failure. Under Read Committed that
// second read can observe a row a concurrent transaction changed (or
// committed) between the failed UPDATE and the classifying SELECT -- e.g.
// another transaction could set active_release_id in the gap, making a
// genuinely stale-revision failure misreport as
// ErrRouteHeadHostnameImmutable, or vice versa. Locking the row up front
// and holding that lock across both the decision and the eventual UPDATE
// removes the gap entirely: nothing else can change this row between the
// SELECT FOR UPDATE and the UPDATE in the same transaction. The
// validate_route_head_update trigger's own hostname-immutability check
// remains as defense in depth against any future direct-SQL path that
// bypasses this method, unchanged.
func (r *RouteHeadRepository) updateRouteHeadHostname(ctx context.Context, tx pgx.Tx, actor AuditActor, ownerID, environmentID, hostname string, expectedRevision int64) (RouteHead, error) {
	var head RouteHead
	head.EnvironmentID = environmentID
	head.Hostname = hostname

	var current RouteHead
	var activeReleaseID *string
	err := tx.QueryRow(ctx, `SELECT environment_id, hostname, route_revision, active_release_id, reconciliation_required, created_at, updated_at FROM route_heads WHERE environment_id=$1 FOR UPDATE`, environmentID).
		Scan(&current.EnvironmentID, &current.Hostname, &current.RouteRevision, &activeReleaseID, &current.ReconciliationRequired, &current.CreatedAt, &current.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if auditErr := insertRouteHeadAuditEvent(ctx, tx, actor, ownerID, environmentID, "hostname_assigned", &hostname, nil, "failure", ""); auditErr != nil {
			return RouteHead{}, auditErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RouteHead{}, fmt.Errorf("commit assign route head hostname (not found): %w", commitErr)
		}
		return RouteHead{}, ErrRouteHeadNotFound
	}
	if err != nil {
		return RouteHead{}, fmt.Errorf("lock route head: %w", err)
	}
	if activeReleaseID != nil {
		current.ActiveReleaseID = *activeReleaseID
	}
	if current.ActiveReleaseID != "" {
		if auditErr := insertRouteHeadAuditEvent(ctx, tx, actor, ownerID, environmentID, "hostname_assigned", &hostname, nil, "failure", ""); auditErr != nil {
			return RouteHead{}, auditErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RouteHead{}, fmt.Errorf("commit assign route head hostname (immutable): %w", commitErr)
		}
		return RouteHead{}, ErrRouteHeadHostnameImmutable
	}
	if current.RouteRevision != expectedRevision {
		if auditErr := insertRouteHeadAuditEvent(ctx, tx, actor, ownerID, environmentID, "hostname_assigned", &hostname, nil, "failure", ""); auditErr != nil {
			return RouteHead{}, auditErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RouteHead{}, fmt.Errorf("commit assign route head hostname (stale revision): %w", commitErr)
		}
		return RouteHead{}, ErrRouteHeadConflict
	}

	// The row is locked (held since the SELECT FOR UPDATE above), not
	// immutable, and the revision matches: the UPDATE below can no longer
	// race with a concurrent writer of this same row.
	err = tx.QueryRow(ctx, `UPDATE route_heads SET hostname=$2, route_revision=route_revision+1, updated_at=clock_timestamp() WHERE environment_id=$1 RETURNING route_revision, created_at, updated_at`,
		environmentID, hostname).Scan(&head.RouteRevision, &head.CreatedAt, &head.UpdatedAt)
	if isUniqueViolation(err) {
		_ = tx.Rollback(ctx)
		if auditErr := insertRouteHeadAuditEvent(ctx, r.pool, actor, ownerID, environmentID, "hostname_assigned", &hostname, nil, "failure", ""); auditErr != nil {
			return RouteHead{}, auditErr
		}
		return RouteHead{}, ErrRouteHeadHostnameTaken
	}
	if err != nil {
		return RouteHead{}, fmt.Errorf("update route head: %w", err)
	}
	if err = r.inject("route_head_update"); err != nil {
		return RouteHead{}, err
	}
	if err = insertRouteHeadAuditEvent(ctx, tx, actor, ownerID, environmentID, "hostname_assigned", &hostname, &head.RouteRevision, "success", ""); err != nil {
		return RouteHead{}, err
	}
	if err = insertRouteEvent(ctx, tx, environmentID, "hostname_assigned", head.RouteRevision, &hostname, nil); err != nil {
		return RouteHead{}, err
	}
	if err = r.inject("audit_insert"); err != nil {
		return RouteHead{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RouteHead{}, fmt.Errorf("commit update route head: %w", err)
	}
	return head, nil
}

func insertRouteEvent(ctx context.Context, exec pgxExecer, environmentID, eventKind string, routeRevision int64, hostname *string, activeReleaseID *string) error {
	if _, err := exec.Exec(ctx, `INSERT INTO route_events(environment_id, event_kind, route_revision, hostname, active_release_id) VALUES ($1,$2,$3,$4,$5)`,
		environmentID, eventKind, routeRevision, hostname, activeReleaseID); err != nil {
		return fmt.Errorf("insert route event: %w", err)
	}
	return nil
}

func insertRouteHeadAuditEvent(ctx context.Context, exec pgxExecer, actor AuditActor, ownerID, environmentID, action string, hostname *string, resultingRevision *int64, result, reason string) error {
	if _, err := exec.Exec(ctx, `INSERT INTO route_head_audit_events(actor_type, actor_id, owner_id, environment_id, action, hostname, resulting_revision, result, reason) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		string(actor.Type), actor.ID, ownerID, environmentID, action, hostname, resultingRevision, result, reason); err != nil {
		return fmt.Errorf("insert route head audit event: %w", err)
	}
	return nil
}
