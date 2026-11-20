package postgres

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRetentionLocker struct{ ok bool }

func (l fakeRetentionLocker) tryLock(context.Context) (func(), bool, error) {
	return func() {}, l.ok, nil
}

func TestEventRetentionStagesAndCutoff(t *testing.T) {
	var stages []string
	var cutoffs []time.Time
	batch := func(_ context.Context, cutoff time.Time, sql string, limit int) (int64, error) {
		for _, s := range eventRetentionStages {
			if s.sql == sql {
				stages = append(stages, s.name)
			}
		}
		cutoffs = append(cutoffs, cutoff)
		if limit != 7 {
			t.Fatalf("limit %d", limit)
		}
		return 0, nil
	}
	p, err := newEventRetentionPurger(batch, fakeRetentionLocker{ok: true}, EventRetentionOptions{BatchSize: 7})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	p.runOnce(context.Background(), now)
	// Webhook deliveries (replay protection) must never get a purge stage.
	want := []string{"agent_job_events", "route_events"}
	for _, st := range eventRetentionStages {
		if strings.Contains(st.name, "webhook") || strings.Contains(st.sql, "webhook_") {
			t.Fatalf("webhook table in retention stage %q", st.name)
		}
	}
	if len(stages) != len(want) {
		t.Fatalf("stages %v", stages)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Fatalf("order %v", stages)
		}
		if !cutoffs[i].Equal(now.Add(-90 * 24 * time.Hour)) {
			t.Fatalf("cutoff %v", cutoffs[i])
		}
	}
}

func TestEventRetentionSkipsWhenLockHeldAndReportsErrors(t *testing.T) {
	var calls, errs atomic.Int32
	p, _ := newEventRetentionPurger(func(context.Context, time.Time, string, int) (int64, error) {
		calls.Add(1)
		return 0, nil
	}, fakeRetentionLocker{ok: false}, EventRetentionOptions{})
	p.runOnce(context.Background(), time.Now())
	if calls.Load() != 0 {
		t.Fatal("ran without the lock")
	}
	p, _ = newEventRetentionPurger(func(context.Context, time.Time, string, int) (int64, error) {
		return 0, errors.New("boom")
	}, nil, EventRetentionOptions{OnError: func() { errs.Add(1) }})
	p.runOnce(context.Background(), time.Now())
	if errs.Load() != 1 {
		t.Fatalf("OnError calls %d", errs.Load())
	}
}

func TestEventRetentionDrainsFullBatchesAndStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	p, _ := newEventRetentionPurger(func(context.Context, time.Time, string, int) (int64, error) {
		if calls.Add(1) == 3 {
			cancel()
		}
		return 5, nil // always a full batch
	}, nil, EventRetentionOptions{BatchSize: 5, MaxBatches: 100, OnError: func() { t.Error("cancel is not an error") }})
	p.runOnce(ctx, time.Now())
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestEventRetentionRunStopsOnContextCancel(t *testing.T) {
	p, _ := newEventRetentionPurger(func(context.Context, time.Time, string, int) (int64, error) { return 0, nil }, nil, EventRetentionOptions{Every: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestEventRetentionRejectsBadConfig(t *testing.T) {
	if _, err := newEventRetentionPurger(nil, nil, EventRetentionOptions{}); !errors.Is(err, ErrEventRetentionConfig) {
		t.Fatal(err)
	}
	if _, err := NewEventRetentionPurger(nil, EventRetentionOptions{}); !errors.Is(err, ErrEventRetentionConfig) {
		t.Fatal(err)
	}
}
