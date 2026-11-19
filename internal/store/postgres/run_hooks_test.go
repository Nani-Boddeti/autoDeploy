package postgres

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestSweeperRunHandlerReportsCountsAndFailures(t *testing.T) {
	var okRuns, errRuns, expired atomic.Int32
	sweeper := (&AgentLeaseSweeper{tick: 2 * time.Millisecond}).WithRunHandler(func(n int, ok bool) {
		if ok {
			okRuns.Add(1)
			expired.Add(int32(n))
		} else {
			errRuns.Add(1)
		}
	})
	var calls atomic.Int32
	sweeper.sweepFn = func(context.Context) (int, error) {
		if calls.Add(1)%2 == 0 {
			return 0, errors.New("boom")
		}
		return 3, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sweeper.Run(ctx) }()
	waitFor(t, func() bool { return okRuns.Load() >= 2 && errRuns.Load() >= 2 })
	cancel()
	<-done
	if expired.Load() != 3*okRuns.Load() {
		t.Fatalf("expired=%d ok runs=%d", expired.Load(), okRuns.Load())
	}
}

func TestSweeperWithoutRunHandlerIsNilSafe(t *testing.T) {
	sweeper := &AgentLeaseSweeper{tick: time.Millisecond}
	var calls atomic.Int32
	sweeper.sweepFn = func(context.Context) (int, error) { calls.Add(1); return 1, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sweeper.Run(ctx) }()
	waitFor(t, func() bool { return calls.Load() >= 2 })
	cancel()
	<-done
}

func TestLogPurgeOnRun(t *testing.T) {
	for name, tc := range map[string]struct {
		purger *fakePurger
		locker *fakeLocker
		want   []bool
	}{
		"ok":         {purger: &fakePurger{}, locker: nil, want: []bool{true}},
		"purge fail": {purger: &fakePurger{err: errors.New("boom")}, want: []bool{false}},
		"lock error": {purger: &fakePurger{}, locker: &fakeLocker{err: errors.New("boom")}, want: []bool{false}},
		"lock lost":  {purger: &fakePurger{}, locker: &fakeLocker{ok: false}, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			var got []bool
			opts := LogPurgeOptions{OnRun: func(ok bool) { got = append(got, ok) }}
			var l logPurgeLocker
			if tc.locker != nil {
				l = tc.locker
			}
			s := newTestScheduler(t, tc.purger, l, opts)
			s.runOnce(context.Background(), time.Now())
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("OnRun calls = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogPurgeOnRunSkippedOnShutdown(t *testing.T) {
	var calls int
	s := newTestScheduler(t, &fakePurger{err: errors.New("boom")}, nil, LogPurgeOptions{OnRun: func(bool) { calls++ }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.runOnce(ctx, time.Now())
	if calls != 0 {
		t.Fatalf("OnRun called %d times for a cancelled parent", calls)
	}
}

func TestLogPurgeNilOnRunIsSafe(t *testing.T) {
	s := newTestScheduler(t, &fakePurger{}, nil, LogPurgeOptions{})
	s.runOnce(context.Background(), time.Now())
}
