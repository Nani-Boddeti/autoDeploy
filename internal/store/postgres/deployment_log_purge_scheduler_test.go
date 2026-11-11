package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePurger struct {
	mu      sync.Mutex
	results []int64 // per call; last repeats
	err     error
	block   chan struct{} // when set, calls park until closed (ignores ctx)
	calls   atomic.Int32
	nows    []time.Time
	limits  []int
}

func (f *fakePurger) PurgeLogChunks(ctx context.Context, now time.Time, _ LogRetention, limit int) (int64, error) {
	i := int(f.calls.Add(1)) - 1
	f.mu.Lock()
	f.nows = append(f.nows, now)
	f.limits = append(f.limits, limit)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return 0, f.err
	}
	if len(f.results) == 0 {
		return 0, nil
	}
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return f.results[i], nil
}

type fakeLocker struct {
	ok       bool
	err      error
	released atomic.Int32
}

func (l *fakeLocker) tryLock(context.Context) (func(), bool, error) {
	if l.err != nil || !l.ok {
		return nil, false, l.err
	}
	return func() { l.released.Add(1) }, true, nil
}

var validRetention = LogRetention{MaxAge: 14 * 24 * time.Hour, KeepNewestPerEnv: 20}

func newTestScheduler(t *testing.T, p logChunkPurger, l logPurgeLocker, o LogPurgeOptions) *LogPurgeScheduler {
	t.Helper()
	o.Retention = validRetention
	s, err := newLogPurgeScheduler(p, l, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLogPurgeSchedulerValidation(t *testing.T) {
	p := &fakePurger{}
	for name, r := range map[string]LogRetention{
		"zero":            {},
		"at window":       {MaxAge: LogPostCloseWindow, KeepNewestPerEnv: 20},
		"below window":    {MaxAge: time.Minute, KeepNewestPerEnv: 20},
		"keep zero":       {MaxAge: time.Hour, KeepNewestPerEnv: 0},
		"keep negative":   {MaxAge: time.Hour, KeepNewestPerEnv: -1},
		"negative maxage": {MaxAge: -time.Hour, KeepNewestPerEnv: 1},
	} {
		if _, err := newLogPurgeScheduler(p, nil, LogPurgeOptions{Retention: r}); !errors.Is(err, ErrLogPurgeConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := newLogPurgeScheduler(p, nil, LogPurgeOptions{Retention: validRetention, BatchSize: -1}); !errors.Is(err, ErrLogPurgeConfig) {
		t.Errorf("negative batch err = %v", err)
	}
	if _, err := NewLogPurgeScheduler(nil, nil, LogPurgeOptions{Retention: validRetention}); !errors.Is(err, ErrLogPurgeConfig) {
		t.Errorf("nil pool err = %v", err)
	}
	s, err := newLogPurgeScheduler(p, nil, LogPurgeOptions{Retention: LogRetention{MaxAge: time.Hour, KeepNewestPerEnv: 1}})
	if err != nil || s.opts.Every != DefaultLogPurgeEvery || s.opts.BatchSize != 500 || s.opts.MaxBatches != DefaultLogPurgeMaxBatches {
		t.Fatalf("defaults = %+v, %v", s.opts, err)
	}
}

func TestLogPurgeSchedulerDrainsUntilShortBatch(t *testing.T) {
	p := &fakePurger{results: []int64{5, 5, 5, 2}}
	s := newTestScheduler(t, p, &fakeLocker{ok: true}, LogPurgeOptions{BatchSize: 5, MaxBatches: 100})
	s.runOnce(context.Background(), time.Unix(1000, 0))
	if p.calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4", p.calls.Load())
	}
	for _, l := range p.limits {
		if l != 5 {
			t.Fatalf("batch limit %d", l)
		}
	}
	for _, n := range p.nows {
		if !n.Equal(time.Unix(1000, 0)) {
			t.Fatalf("now not constant per run: %v", p.nows)
		}
	}
}

func TestLogPurgeSchedulerPerRunCap(t *testing.T) {
	p := &fakePurger{results: []int64{5}} // always full
	l := &fakeLocker{ok: true}
	s := newTestScheduler(t, p, l, LogPurgeOptions{BatchSize: 5, MaxBatches: 3})
	s.runOnce(context.Background(), time.Now())
	if p.calls.Load() != 3 || l.released.Load() != 1 {
		t.Fatalf("calls = %d released = %d, want 3 and 1", p.calls.Load(), l.released.Load())
	}
}

func TestLogPurgeSchedulerBudgetStopsDrain(t *testing.T) {
	p := &fakePurger{results: []int64{5}}
	s := newTestScheduler(t, p, nil, LogPurgeOptions{BatchSize: 5, MaxBatches: 1 << 30, Budget: 30 * time.Millisecond})
	done := make(chan struct{})
	go func() { defer close(done); s.runOnce(context.Background(), time.Now()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run exceeded its budget")
	}
	if p.calls.Load() < 1 {
		t.Fatal("no batches ran")
	}
}

func TestLogPurgeSchedulerErrorIsReportedOnceAndStopsRun(t *testing.T) {
	p := &fakePurger{err: errors.New("boom")}
	var failures atomic.Int32
	s := newTestScheduler(t, p, nil, LogPurgeOptions{OnError: func() { failures.Add(1) }})
	s.runOnce(context.Background(), time.Now())
	if p.calls.Load() != 1 || failures.Load() != 1 {
		t.Fatalf("calls = %d failures = %d", p.calls.Load(), failures.Load())
	}
	l := &fakeLocker{err: errors.New("no conn")}
	s = newTestScheduler(t, p, l, LogPurgeOptions{OnError: func() { failures.Add(1) }})
	before := p.calls.Load()
	s.runOnce(context.Background(), time.Now())
	if p.calls.Load() != before || failures.Load() != 2 {
		t.Fatalf("lock error: calls %d failures %d", p.calls.Load(), failures.Load())
	}
}

func TestLogPurgeSchedulerSkipsWhenLockHeldElsewhere(t *testing.T) {
	p := &fakePurger{}
	var failures atomic.Int32
	s := newTestScheduler(t, p, &fakeLocker{ok: false}, LogPurgeOptions{OnError: func() { failures.Add(1) }})
	s.runOnce(context.Background(), time.Now())
	if p.calls.Load() != 0 || failures.Load() != 0 {
		t.Fatalf("calls = %d failures = %d", p.calls.Load(), failures.Load())
	}
}

func TestLogPurgeSchedulerCadence(t *testing.T) {
	p := &fakePurger{}
	s := newTestScheduler(t, p, nil, LogPurgeOptions{Every: 10 * time.Minute})
	clock := time.Unix(1_000_000, 0)
	s.nowFunc = func() time.Time { return clock }
	trigger := func() { s.Trigger(context.Background()); s.Wait(5 * time.Second) }
	trigger()
	if p.calls.Load() != 1 {
		t.Fatalf("first trigger calls = %d", p.calls.Load())
	}
	clock = clock.Add(9 * time.Minute)
	trigger()
	if p.calls.Load() != 1 {
		t.Fatalf("inside cadence calls = %d", p.calls.Load())
	}
	clock = clock.Add(time.Minute)
	trigger()
	if p.calls.Load() != 2 {
		t.Fatalf("after cadence calls = %d", p.calls.Load())
	}
}

func TestLogPurgeSchedulerTriggerNeverBlocksAndNeverOverlaps(t *testing.T) {
	p := &fakePurger{block: make(chan struct{})}
	s := newTestScheduler(t, p, nil, LogPurgeOptions{Every: time.Nanosecond})
	start := time.Now()
	s.Trigger(context.Background())
	for p.calls.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(time.Millisecond)
		s.Trigger(context.Background()) // in flight: must return at once, no second run
	}
	if time.Since(start) > 2*time.Second || p.calls.Load() != 1 {
		t.Fatalf("blocked or overlapped: calls = %d", p.calls.Load())
	}
	close(p.block)
	s.Wait(5 * time.Second)
}

// A hung or failing purger must not stop the sweeper from running its own
// duties every tick, and purge runs after them; Run still exits on cancel.
func TestSweeperTicksContinueWhenPurgeHangsOrErrors(t *testing.T) {
	for name, p := range map[string]*fakePurger{
		"hang":  {block: make(chan struct{})},
		"error": {err: errors.New("boom")},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestScheduler(t, p, nil, LogPurgeOptions{Every: time.Nanosecond})
			var sweeps, sweepFailures atomic.Int32
			sweeper := (&AgentLeaseSweeper{tick: 5 * time.Millisecond}).WithLogPurge(s).
				WithErrorHandler(func() { sweepFailures.Add(1) })
			sweeper.sweepFn = func(context.Context) (int, error) {
				if sweeps.Add(1)%2 == 0 {
					return 0, errors.New("sweep failed") // a failed sweep still triggers purge
				}
				return 1, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); sweeper.Run(ctx) }()
			deadline := time.After(5 * time.Second)
			for sweeps.Load() < 6 || p.calls.Load() < 1 {
				select {
				case <-deadline:
					cancel()
					t.Fatalf("sweeps = %d purge calls = %d", sweeps.Load(), p.calls.Load())
				case <-time.After(5 * time.Millisecond):
				}
			}
			if sweepFailures.Load() < 1 {
				t.Fatal("sweep error handler never ran")
			}
			cancel()
			if p.block != nil {
				close(p.block)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("sweeper did not stop")
			}
		})
	}
}
