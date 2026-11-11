package postgres

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// deploymentLogPurgeAdvisoryLockKey is the purge scheduler's own session-level
// pg_try_advisory_lock key, distinct from agentLeaseSweeperAdvisoryLockKey so
// a long purge never makes another replica skip its lease/activation sweep.
// Never reuse it elsewhere.
const deploymentLogPurgeAdvisoryLockKey = int64(0x61676e745f6c7033) // "agnt_lp3"

// Defaults for the 15g L4 purge schedule. Retention itself is days-scale, so
// a run every 10 minutes is ample; each run is bounded by rows and wall time.
const (
	DefaultLogPurgeEvery      = 10 * time.Minute
	DefaultLogPurgeBatchSize  = 500
	DefaultLogPurgeMaxBatches = 40 // <= 20,000 chunk rows per run
	DefaultLogPurgeBudget     = 30 * time.Second
	logPurgeUnlockTimeout     = 5 * time.Second
)

// ErrLogPurgeConfig reports an invalid purge schedule or retention policy.
var ErrLogPurgeConfig = errors.New("invalid deployment log purge configuration")

type logChunkPurger interface {
	PurgeLogChunks(ctx context.Context, now time.Time, retention LogRetention, batchLimit int) (int64, error)
}

// logPurgeLocker is a non-blocking single-runner gate. ok=false means another
// replica is purging; release must be called exactly once when ok.
type logPurgeLocker interface {
	tryLock(ctx context.Context) (release func(), ok bool, err error)
}

// LogPurgeOptions configures a LogPurgeScheduler; zero fields take defaults
// except Retention, which is validated as given.
type LogPurgeOptions struct {
	Retention  LogRetention
	Every      time.Duration
	BatchSize  int
	MaxBatches int
	Budget     time.Duration
	// OnError receives no error value (it may carry driver/connection text).
	OnError func()
}

// LogPurgeScheduler runs PurgeLogChunks at a modest cadence in its own
// goroutine, off the sweeper's tick path: Trigger never blocks, so a slow or
// hung purge can never delay lease/activation duties. A run drains batches
// (each its own short transaction; none is held across batches) until a short
// batch, the per-run batch cap, or the time budget. Single runner across
// replicas via a try-lock; a replica that loses it simply skips the run.
type LogPurgeScheduler struct {
	purger  logChunkPurger
	locker  logPurgeLocker
	opts    LogPurgeOptions
	nowFunc func() time.Time

	mu      sync.Mutex
	lastRun time.Time
	running bool
	wg      sync.WaitGroup
}

// NewLogPurgeScheduler validates the policy: MaxAge must exceed the append
// post-close window (otherwise a purge could race live appends) and
// KeepNewestPerEnv must be at least 1.
func NewLogPurgeScheduler(pool *pgxpool.Pool, repo *DeploymentLogRepository, opts LogPurgeOptions) (*LogPurgeScheduler, error) {
	if pool == nil || repo == nil {
		return nil, ErrLogPurgeConfig
	}
	return newLogPurgeScheduler(repo, pgAdvisoryLogPurgeLocker{pool: pool}, opts)
}

func newLogPurgeScheduler(purger logChunkPurger, locker logPurgeLocker, opts LogPurgeOptions) (*LogPurgeScheduler, error) {
	if opts.Every == 0 {
		opts.Every = DefaultLogPurgeEvery
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = DefaultLogPurgeBatchSize
	}
	if opts.MaxBatches == 0 {
		opts.MaxBatches = DefaultLogPurgeMaxBatches
	}
	if opts.Budget == 0 {
		opts.Budget = DefaultLogPurgeBudget
	}
	if purger == nil || opts.Retention.MaxAge <= LogPostCloseWindow || opts.Retention.KeepNewestPerEnv < 1 ||
		opts.Every < 0 || opts.BatchSize < 0 || opts.MaxBatches < 0 || opts.Budget < 0 {
		return nil, ErrLogPurgeConfig
	}
	return &LogPurgeScheduler{purger: purger, locker: locker, opts: opts, nowFunc: time.Now}, nil
}

// Trigger starts a purge run in the background if the cadence has elapsed and
// none is in flight. It returns immediately. ctx bounds the run's lifetime
// (cancel on shutdown); the per-run budget applies on top.
func (s *LogPurgeScheduler) Trigger(ctx context.Context) {
	if s == nil {
		return
	}
	now := s.nowFunc()
	s.mu.Lock()
	if s.running || ctx.Err() != nil || (!s.lastRun.IsZero() && now.Sub(s.lastRun) < s.opts.Every) {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.lastRun = now
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		s.runOnce(ctx, now)
	}()
}

// Wait blocks until an in-flight run ends or timeout elapses (shutdown aid).
func (s *LogPurgeScheduler) Wait(timeout time.Duration) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// runOnce drains up to MaxBatches batches inside the budget. now is the app
// clock used for the cutoff, evaluated once per run (the retention horizon is
// days; the sub-minute skew against the DB-set closed_at is immaterial).
func (s *LogPurgeScheduler) runOnce(parent context.Context, now time.Time) {
	ctx, cancel := context.WithTimeout(parent, s.opts.Budget)
	defer cancel()
	if s.locker != nil {
		release, ok, err := s.locker.tryLock(ctx)
		if err != nil {
			s.fail(parent)
			return
		}
		if !ok {
			return
		}
		defer release()
	}
	for i := 0; i < s.opts.MaxBatches && ctx.Err() == nil; i++ {
		n, err := s.purger.PurgeLogChunks(ctx, now, s.opts.Retention, s.opts.BatchSize)
		if err != nil {
			s.fail(parent)
			return
		}
		if n < int64(s.opts.BatchSize) {
			return
		}
	}
}

func (s *LogPurgeScheduler) fail(parent context.Context) {
	if parent.Err() == nil && s.opts.OnError != nil {
		s.opts.OnError()
	}
}

type pgAdvisoryLogPurgeLocker struct{ pool *pgxpool.Pool }

// tryLock holds a session-level advisory lock on one dedicated connection (no
// open transaction). If the unlock fails the connection is destroyed, which
// releases the lock server-side, so it can never leak on a pooled connection.
func (l pgAdvisoryLogPurgeLocker) tryLock(ctx context.Context) (func(), bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, deploymentLogPurgeAdvisoryLockKey).Scan(&acquired); err != nil || !acquired {
		if err != nil {
			_ = conn.Hijack().Close(context.Background())
		} else {
			conn.Release()
		}
		return nil, false, err
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), logPurgeUnlockTimeout)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, deploymentLogPurgeAdvisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Hijack().Close(context.Background())
			return
		}
		conn.Release()
	}, true, nil
}
