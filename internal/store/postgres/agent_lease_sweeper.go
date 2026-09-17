package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// agentLeaseSweeperAdvisoryLockKey is this sweeper's fixed
// pg_try_advisory_xact_lock key. It must never collide with any other
// advisory lock key used elsewhere in this codebase; there are none today.
const agentLeaseSweeperAdvisoryLockKey = int64(0x61676e745f6c7332) // "agnt_ls2"

// AgentLeaseSweeper periodically calls AgentLeaseRepository.ExpireBatch so
// authority-expired leases are recovered even when no agent ever renews or
// completes them (ADR 0004 recovery; Step 4 sweeper). Exactly one replica's
// tick does useful work at a time: each tick opens a transaction, tries
// pg_try_advisory_xact_lock, and only calls ExpireBatch while holding it,
// releasing it by committing/rolling back that transaction when the tick
// ends -- a replica that loses the race simply skips that tick.
type AgentLeaseSweeper struct {
	pool  *pgxpool.Pool
	lease *AgentLeaseRepository
	tick  time.Duration
	batch int
}

// NewAgentLeaseSweeper constructs a sweeper with the ADR 0004 V1 defaults:
// a 15-second tick and a 100-attempt batch per tick.
func NewAgentLeaseSweeper(pool *pgxpool.Pool, lease *AgentLeaseRepository) *AgentLeaseSweeper {
	return &AgentLeaseSweeper{pool: pool, lease: lease, tick: 15 * time.Second, batch: expireBatchMaxLimit}
}

// WithTick overrides the poll interval. Production keeps the default;
// tests inject a short tick so they do not have to wait 15 real seconds.
func (s *AgentLeaseSweeper) WithTick(d time.Duration) *AgentLeaseSweeper {
	if d > 0 {
		s.tick = d
	}
	return s
}

// WithBatch overrides the per-tick batch size (tests only; production keeps
// the ADR 0004 default of 100, which ExpireBatch itself also enforces as a
// ceiling regardless of what is requested here).
func (s *AgentLeaseSweeper) WithBatch(n int) *AgentLeaseSweeper {
	if n > 0 {
		s.batch = n
	}
	return s
}

// Run polls until ctx is cancelled. It never returns an error; a failed tick
// (lock contention, a transient database error) is silently retried on the
// next tick rather than stopping the sweeper.
func (s *AgentLeaseSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs exactly one sweep attempt, exported so integration tests
// can drive the sweeper deterministically instead of waiting out real
// ticks: it acquires the sweeper's
// session-scoped advisory lock (held for the duration of this call via its
// own transaction) and, only while holding it, calls ExpireBatch. It
// returns (0, nil) without doing any work if another sweeper currently
// holds the lock.
func (s *AgentLeaseSweeper) SweepOnce(ctx context.Context) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var acquired bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, agentLeaseSweeperAdvisoryLockKey).Scan(&acquired); err != nil {
		return 0, err
	}
	if !acquired {
		return 0, nil
	}

	// ExpireBatch opens its own short, independently committed transactions
	// per candidate on the pool directly; this outer transaction's sole job
	// is to hold the advisory lock open for that entire duration, so a
	// second concurrent sweeper's pg_try_advisory_xact_lock fails until this
	// one commits below.
	processed, err := s.lease.ExpireBatch(ctx, s.batch)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return processed, nil
}
