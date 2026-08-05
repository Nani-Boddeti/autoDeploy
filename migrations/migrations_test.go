//go:build integration

package migrations

import (
	"context"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

func TestApplyConcurrentAndChecksum(t *testing.T) {
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	connections := make([]*pgx.Conn, 2)
	for index := range connections {
		var err error
		connections[index], err = pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		conn := connections[index]
		t.Cleanup(func() { conn.Close(ctx) })
	}
	var wg sync.WaitGroup
	results := make(chan error, len(connections))
	for _, conn := range connections {
		wg.Add(1)
		go func(c *pgx.Conn) { defer wg.Done(); results <- Apply(ctx, c) }(conn)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	original := migrationFiles
	migrationFiles = fstest.MapFS{"000001_create_deployments.sql": &fstest.MapFile{Data: []byte("-- modified")}}
	t.Cleanup(func() { migrationFiles = original })
	if err := Apply(ctx, connections[0]); err == nil {
		t.Fatal("modified migration was accepted")
	}
}
