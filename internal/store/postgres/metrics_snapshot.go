package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// States mirrored from CHECK constraints (agent_jobs_state_check,
// releases_state_check, webhook_deliveries_processing_state_check). A
// metrics integration test compares them with the live constraints so a
// migration cannot change a state list without this file following. They exist
// so a state with no rows is exported as 0 instead of vanishing (rate and
// alert expressions need the series to exist).
var (
	AgentJobStates      = []string{"queued", "leased", "retry_wait", "succeeded", "failed", "cancelled", "dead"}
	ReleaseStates       = []string{"candidate", "ready", "failed", "removing", "removed", "active", "retained"}
	AllWebhookJobStates = []string{"pending", "processed", "rejected"}
	// WebhookJobStates is the subset exported. webhook_deliveries only ever
	// grows (one row per delivery), and only 'pending' is covered by a partial
	// index, so counting the terminal states would be a full scan every
	// refresh; the backlog is what an operator alerts on.
	WebhookJobStates = []string{"pending"}
)

// metricsStatementTimeout bounds the snapshot query server-side; the caller's
// context deadline is only the backstop.
const metricsStatementTimeout = 2 * time.Second

// MetricsSnapshot is a point-in-time, read-only view of operational state.
type MetricsSnapshot struct {
	// AgentJobs, Releases and WebhookJobs are zero-filled for every known state.
	AgentJobs   map[string]int64
	Releases    map[string]int64
	WebhookJobs map[string]int64
	// HeartbeatAgeSeconds covers active servers that have heartbeated at
	// least once, keyed by server id. A server that has never heartbeated is
	// omitted (there is no age to report); it is visible in the UI.
	HeartbeatAgeSeconds map[string]float64
	// EnvironmentsReconciliationRequired counts environment_slots flagged for
	// operator reconciliation.
	EnvironmentsReconciliationRequired int64
	// ReleaseOperationsOpen counts release_operations that are not closed
	// (closed_at IS NULL: states started and switch_authorized).
	ReleaseOperationsOpen int64
}

// MetricsRepository reads MetricsSnapshot.
type MetricsRepository struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
}

// NewMetricsRepository wraps pool.
func NewMetricsRepository(pool *pgxpool.Pool) *MetricsRepository {
	return &MetricsRepository{pool: pool, statementTimeout: metricsStatementTimeout}
}

// One statement, hence one consistent snapshot. The second
// column is the state / server id (empty for scalar metrics).
const metricsSnapshotSQL = `
SELECT 'agent_jobs', state, count(*)::float8 FROM agent_jobs GROUP BY state
UNION ALL
SELECT 'heartbeat_age', id, extract(epoch FROM clock_timestamp() - last_heartbeat_at)::float8 FROM servers WHERE status = 'active' AND last_heartbeat_at IS NOT NULL
UNION ALL
SELECT 'reconciliation_required', '', count(*)::float8 FROM environment_slots WHERE reconciliation_required
UNION ALL
SELECT 'release_operations_open', '', count(*)::float8 FROM release_operations WHERE closed_at IS NULL
UNION ALL
SELECT 'webhook_jobs', processing_state, count(*)::float8 FROM webhook_deliveries WHERE processing_state = 'pending' GROUP BY processing_state
UNION ALL
SELECT 'releases', state, count(*)::float8 FROM releases GROUP BY state`

// MetricsSnapshot reads the snapshot in a READ ONLY transaction with a
// server-side statement_timeout.
func (r *MetricsRepository) MetricsSnapshot(ctx context.Context) (MetricsSnapshot, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return MetricsSnapshot{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Set before the query is even prepared: a table lock held by a
	// migration would otherwise block the prepare step outside the timeout.
	// The value is an integer we format ourselves (SET takes no parameters).
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(r.statementTimeout.Milliseconds(), 10)); err != nil {
		return MetricsSnapshot{}, err
	}
	rows, err := tx.Query(ctx, metricsSnapshotSQL)
	if err != nil {
		return MetricsSnapshot{}, err
	}
	defer rows.Close()
	snap := MetricsSnapshot{
		AgentJobs:           zeroFilled(AgentJobStates),
		Releases:            zeroFilled(ReleaseStates),
		WebhookJobs:         zeroFilled(WebhookJobStates),
		HeartbeatAgeSeconds: map[string]float64{},
	}
	for rows.Next() {
		var kind, key string
		var value float64
		if err = rows.Scan(&kind, &key, &value); err != nil {
			return MetricsSnapshot{}, err
		}
		switch kind {
		case "agent_jobs":
			snap.AgentJobs[key] = int64(value)
		case "releases":
			snap.Releases[key] = int64(value)
		case "webhook_jobs":
			snap.WebhookJobs[key] = int64(value)
		case "heartbeat_age":
			snap.HeartbeatAgeSeconds[key] = max(value, 0) // clock skew must not export a negative age
		case "reconciliation_required":
			snap.EnvironmentsReconciliationRequired = int64(value)
		case "release_operations_open":
			snap.ReleaseOperationsOpen = int64(value)
		}
	}
	if err = rows.Err(); err != nil {
		return MetricsSnapshot{}, err
	}
	rows.Close()
	return snap, nil
}

func zeroFilled(states []string) map[string]int64 {
	m := make(map[string]int64, len(states))
	for _, s := range states {
		m[s] = 0
	}
	return m
}
