package metrics_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autodeploy/internal/metrics"
)

func TestCacheHonoursTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	var calls atomic.Int32
	c := metrics.NewCache(15*time.Second, 5*time.Second, 2*time.Second, func(context.Context) (int, error) { return int(calls.Add(1)), nil }, nil)
	c.SetClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if v, ok := c.Get(); !ok || v != 1 {
			t.Fatalf("v=%d ok=%v", v, ok)
		}
	}
	now = now.Add(16 * time.Second)
	if v, _ := c.Get(); v != 2 || calls.Load() != 2 {
		t.Fatalf("v=%d calls=%d", v, calls.Load())
	}
}

func TestCacheSingleInFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	c := metrics.NewCache(time.Minute, time.Minute, 2*time.Second, func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, ok := c.Get(); !ok || v != 7 {
				t.Errorf("v=%d ok=%v", v, ok)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls.Load())
	}
}

func TestCacheTimeoutReportsFailureWithoutHanging(t *testing.T) {
	var reported atomic.Int32
	c := metrics.NewCache(time.Minute, time.Minute, 100*time.Millisecond, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, func(err error) {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err=%v", err)
		}
		reported.Add(1)
	})
	start := time.Now()
	if _, ok := c.Get(); ok {
		t.Fatal("want failure")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("scrape hung")
	}
	// The failure is cached for the TTL: no second fetch, no second report.
	if _, ok := c.Get(); ok || reported.Load() != 1 {
		t.Fatalf("reported=%d", reported.Load())
	}
}

func TestCacheRetriesFailureAfterFailureTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	var calls atomic.Int32
	c := metrics.NewCache(15*time.Second, 5*time.Second, time.Second, func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("down")
		}
		return 9, nil
	}, nil)
	c.SetClock(func() time.Time { return now })
	if _, ok := c.Get(); ok {
		t.Fatal("first fetch must fail")
	}
	now = now.Add(4 * time.Second)
	if _, ok := c.Get(); ok || calls.Load() != 1 {
		t.Fatalf("failure must be cached within the failure TTL, calls=%d", calls.Load())
	}
	now = now.Add(2 * time.Second) // 6s > 5s failure TTL, < 15s success TTL
	if v, ok := c.Get(); !ok || v != 9 || calls.Load() != 2 {
		t.Fatalf("v=%d ok=%v calls=%d", v, ok, calls.Load())
	}
	now = now.Add(10 * time.Second) // success is still cached at 10s
	if _, _ = c.Get(); calls.Load() != 2 {
		t.Fatalf("success TTL must stay 15s, calls=%d", calls.Load())
	}
}
