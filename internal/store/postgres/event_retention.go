package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// eventRetentionAdvisoryLockKey is the retention purger's own session-level
// pg_try_advisory_lock key, distinct from the log purge and lease sweeper keys
// so a long retention run never blocks either. Never reuse it elsewhere.
const eventRetentionAdvisoryLockKey = int64(0x61676e745f727433) // "agnt_rt3"

// Retention policy (22-7 F13, owner decision D3a). Audit tables and
// release_operations are deliberately kept forever.
const (
	// EventRetentionMaxAge is the age past which append-only operational
	// history is purged.
	EventRetentionMaxAge = 90 * 24 * time.Hour

	DefaultEventRetentionEvery      = time.Hour
	DefaultEventRetentionBatchSize  = 500
	DefaultEventRetentionMaxBatches = 100 // per table per run, <= 50,000 rows
	DefaultEventRetentionBudget     = 2 * time.Minute
	eventRetentionJitterDivisor     = 10 // jitter <= Every/10
)

// ErrEventRetentionConfig reports an invalid retention schedule.
var ErrEventRetentionConfig = errors.New("invalid event retention configuration")

// The migration's DELETE triggers only accept rows inside a transaction that
// ran this (accident guard, not an access-control boundary).
const retentionGUCStatement = `SELECT set_config('autodeploy.retention_purge','on',true)`

// Each stage deletes at most $2 rows older than $1. ctid subselects keep every
// batch a bounded index-range scan plus a TID scan. webhook_deliveries and
// webhook_lifecycle_repositories are deliberately NOT purged (owner decision):
// delivery ids are replay protection and are kept forever; their 000010
// triggers reject DELETE even with the retention GUC.
var eventRetentionStages = []struct {
	name string
	sql  string
}{
	// Only events of terminal jobs (state is final: agent_jobs rejects any
	// transition out of succeeded/failed/cancelled/dead). Two kinds are kept
	// forever because live code reads them for jobs of ANY age:
	//  - 'reconciled': confirmResolvedOutcome / ReconcileActivation answer a
	//    repeat resolution call only if the attempt has a 'reconciled' event
	//    (the latest attempt of an idle environment can be years old), and
	//    traceDeclaredContradiction keys on it. Purging it would turn a
	//    legitimate repeat into a denial.
	//  - 'activation_late_report': its dedupe is "exists with sequence >
	//    reconciled's sequence"; purging it would allow a duplicate append.
	// Keeping both also keeps MAX(sequence)+1 (the next-event sequence) above
	// the reconciled event, so sequences never regress on a job that can still
	// receive a late report. operations_read only displays the newest N events
	// and tolerates gaps.
	{"agent_job_events", `DELETE FROM agent_job_events WHERE ctid = ANY(ARRAY(
		SELECT e.ctid FROM agent_job_events e JOIN agent_jobs j ON j.id = e.job_id
		WHERE e.occurred_at < $1 AND j.state IN ('succeeded','failed','cancelled','dead')
		AND e.event_type NOT IN ('reconciled','activation_late_report') LIMIT $2))`},
	// route_events is write-only in the store (grep: its single reference is
	// the INSERT in route_head_repository.go). Current routing state lives in
	// route_heads (hostname, revision, active release), never derived from
	// the events, and nothing references route_events by FK. Deleting old
	// rows therefore cannot change any current-state answer.
	{"route_events", `DELETE FROM route_events WHERE ctid = ANY(ARRAY(
		SELECT ctid FROM route_events WHERE created_at < $1 LIMIT $2))`},
}

// EventRetentionOptions configures an EventRetentionPurger; zero fields take
// defaults.
type EventRetentionOptions struct {
	Every      time.Duration
	BatchSize  int
	MaxBatches int
	Budget     time.Duration
	// OnError receives no error value (it may carry driver/connection text).
	OnError func()
	// OnRun (optional, metrics) is called once per run that completed or
	// failed, with ok=false on failure. Not called when another replica holds
	// the lock or once the parent context is cancelled.
	OnRun func(ok bool)
}

type retentionBatchFunc func(ctx context.Context, cutoff time.Time, stageSQL string, limit int) (int64, error)

// EventRetentionPurger periodically deletes old, no-longer-needed history.
// One run drains each stage in short per-batch transactions inside a time
// budget, single-flight across replicas via an advisory try-lock.
type EventRetentionPurger struct {
	locker  logPurgeLocker
	batch   retentionBatchFunc
	opts    EventRetentionOptions
	nowFunc func() time.Time
	jitter  func(max time.Duration) time.Duration

	mu sync.Mutex
	wg sync.WaitGroup
}

// NewEventRetentionPurger builds the purger on pool (runtime role suffices:
// DELETE on agent_job_events and route_events plus SELECT on agent_jobs).
func NewEventRetentionPurger(pool *pgxpool.Pool, opts EventRetentionOptions) (*EventRetentionPurger, error) {
	if pool == nil {
		return nil, ErrEventRetentionConfig
	}
	return newEventRetentionPurger(poolRetentionBatch(pool), pgAdvisoryKeyLocker{pool: pool, key: eventRetentionAdvisoryLockKey}, opts)
}

func newEventRetentionPurger(batch retentionBatchFunc, locker logPurgeLocker, opts EventRetentionOptions) (*EventRetentionPurger, error) {
	if opts.Every == 0 {
		opts.Every = DefaultEventRetentionEvery
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = DefaultEventRetentionBatchSize
	}
	if opts.MaxBatches == 0 {
		opts.MaxBatches = DefaultEventRetentionMaxBatches
	}
	if opts.Budget == 0 {
		opts.Budget = DefaultEventRetentionBudget
	}
	if batch == nil || opts.Every < 0 || opts.BatchSize < 0 || opts.MaxBatches < 0 || opts.Budget < 0 {
		return nil, ErrEventRetentionConfig
	}
	return &EventRetentionPurger{
		locker: locker, batch: batch, opts: opts, nowFunc: time.Now,
		jitter: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return rand.N(max)
		},
	}, nil
}

func poolRetentionBatch(pool *pgxpool.Pool) retentionBatchFunc {
	return func(ctx context.Context, cutoff time.Time, stageSQL string, limit int) (int64, error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err = tx.Exec(ctx, retentionGUCStatement); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, stageSQL, cutoff, limit)
		if err != nil {
			return 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
}

// Run loops until ctx is cancelled: a jittered first run, then every
// Every plus jitter (so replicas started together do not align). It returns
// only after any in-flight run has stopped.
func (p *EventRetentionPurger) Run(ctx context.Context) {
	if p == nil {
		return
	}
	defer p.wg.Wait()
	wait := p.jitter(p.opts.Every / eventRetentionJitterDivisor)
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		p.RunOnce(ctx)
		wait = p.opts.Every + p.jitter(p.opts.Every/eventRetentionJitterDivisor)
	}
}

// RunOnce performs one purge run synchronously.
func (p *EventRetentionPurger) RunOnce(parent context.Context) {
	p.wg.Add(1)
	defer p.wg.Done()
	p.runOnce(parent, p.nowFunc())
}

func (p *EventRetentionPurger) runOnce(parent context.Context, now time.Time) {
	ctx, cancel := context.WithTimeout(parent, p.opts.Budget)
	defer cancel()
	if p.locker != nil {
		release, ok, err := p.locker.tryLock(ctx)
		if err != nil {
			p.fail(parent)
			return
		}
		if !ok {
			return
		}
		defer release()
	}
	cutoff := now.Add(-EventRetentionMaxAge)
	for _, stage := range eventRetentionStages {
		for i := 0; i < p.opts.MaxBatches && ctx.Err() == nil; i++ {
			n, err := p.batch(ctx, cutoff, stage.sql, p.opts.BatchSize)
			if err != nil {
				if ctx.Err() != nil {
					return // budget spent or shutdown: not a failure; next run resumes
				}
				p.fail(parent)
				return
			}
			if n < int64(p.opts.BatchSize) {
				break
			}
		}
	}
	// A budget overrun is not a failure: the next run continues the backlog.
	if parent.Err() == nil && p.opts.OnRun != nil {
		p.opts.OnRun(true)
	}
}

func (p *EventRetentionPurger) fail(parent context.Context) {
	if parent.Err() != nil {
		return
	}
	if p.opts.OnError != nil {
		p.opts.OnError()
	}
	if p.opts.OnRun != nil {
		p.opts.OnRun(false)
	}
}

// pgAdvisoryKeyLocker is pgAdvisoryLogPurgeLocker with a caller-chosen key:
// a session-level advisory lock on one dedicated connection; a failed unlock
// destroys the connection, which releases the lock server-side.
type pgAdvisoryKeyLocker struct {
	pool *pgxpool.Pool
	key  int64
}

func (l pgAdvisoryKeyLocker) tryLock(ctx context.Context) (func(), bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&acquired); err != nil || !acquired {
		if err != nil {
			closeHijacked(conn)
		} else {
			conn.Release()
		}
		return nil, false, err
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), logPurgeUnlockTimeout)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&unlocked); err != nil || !unlocked {
			closeHijacked(conn)
			return
		}
		conn.Release()
	}, true, nil
}
