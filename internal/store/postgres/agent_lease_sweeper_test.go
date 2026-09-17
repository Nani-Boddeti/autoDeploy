package postgres

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAgentLeaseSweeperReportsFailedTicksWithoutStopping(t *testing.T) {
	config, err := pgxpool.ParseConfig("postgres://sweeper@127.0.0.1:1/unreachable?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var failures atomic.Int32
	sweeper := NewAgentLeaseSweeper(pool, nil).WithTick(10 * time.Millisecond).WithErrorHandler(func() { failures.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sweeper.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for failures.Load() < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("failed ticks reported = %d, want at least 2", failures.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not stop after cancellation")
	}
}
