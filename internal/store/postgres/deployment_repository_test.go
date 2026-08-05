//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	store "autodeploy/internal/store/postgres"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeploymentRepositoryIntegration(t *testing.T) {
	pool := integrationPool(t)
	repo := store.NewDeploymentRepository(pool)
	ctx := context.Background()
	t.Run("create queued round trip", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-queued")
		want, err := value.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		loaded, err := repo.GetByID(ctx, value.Identity().DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := loaded.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("queued round trip snapshot = %#v, want %#v", got, want)
		}
	})
	t.Run("create load lifecycle and append-only events", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-lifecycle")
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		for _, status := range []deployment.Status{deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting, deployment.StatusHealthChecking, deployment.StatusActivating, deployment.StatusHealthy, deployment.StatusRolledBack} {
			var err error
			value, err = value.Transition(status, value.UpdatedAt())
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Save(ctx, value); err != nil {
				t.Fatal(err)
			}
			loaded, err := repo.GetByID(ctx, value.Identity().DeploymentID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := loaded.Revision(); got != value.Revision() || got != deployment.Revision(len(loaded.Events())) {
				t.Fatalf("loaded revision = %d", got)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE deployment_events SET to_status='failed' WHERE deployment_id=$1`, "deployment-lifecycle"); err == nil {
			t.Fatal("event update unexpectedly succeeded")
		}
		if _, err := pool.Exec(ctx, `DELETE FROM deployment_events WHERE deployment_id=$1`, "deployment-lifecycle"); err == nil {
			t.Fatal("event delete unexpectedly succeeded")
		}
	})
	t.Run("terminal lifecycle variants", func(t *testing.T) {
		for _, target := range []deployment.Status{deployment.StatusCancelled, deployment.StatusSuperseded, deployment.StatusFailed} {
			reset(t, pool)
			value := newDeployment(t, deployment.ID("deployment-"+string(target)))
			if err := repo.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			if target == deployment.StatusFailed {
				value, _ = value.Transition(deployment.StatusAssigned, value.UpdatedAt())
			}
			value, _ = value.Transition(target, value.UpdatedAt())
			if err := repo.Save(ctx, value); err != nil {
				t.Fatal(err)
			}
			loaded, err := repo.GetByID(ctx, value.Identity().DeploymentID)
			if err != nil || loaded.Status() != target {
				t.Fatalf("%s = %s, %v", target, loaded.Status(), err)
			}
		}
	})
	t.Run("multiple transitions save and immutable identity", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-multiple")
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		value, _ = value.Transition(deployment.StatusAssigned, value.UpdatedAt())
		value, _ = value.Transition(deployment.StatusFetching, value.UpdatedAt())
		if err := repo.Save(ctx, value); err != nil {
			t.Fatal(err)
		}
		loaded, err := repo.GetByID(ctx, value.Identity().DeploymentID)
		if err != nil || loaded.Revision() != 3 || len(loaded.Events()) != 3 {
			t.Fatalf("multi-transition load = %#v, %v", loaded, err)
		}
		snapshot, err := value.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Identity.ProjectID = "other-project"
		changedIdentity, err := deployment.Rehydrate(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(ctx, changedIdentity); !errors.Is(err, store.ErrRevisionConflict) {
			t.Fatalf("identity mutation = %v", err)
		}
	})
	t.Run("save missing", func(t *testing.T) {
		if err := repo.Save(ctx, newDeployment(t, "deployment-missing")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("save missing = %v", err)
		}
	})
	t.Run("duplicate missing stale and concurrent saves", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-cas")
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		if err := repo.Create(ctx, value); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate = %v", err)
		}
		if _, err := repo.GetByID(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing = %v", err)
		}
		a, err := repo.GetByID(ctx, value.Identity().DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := repo.GetByID(ctx, value.Identity().DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		a, _ = a.Transition(deployment.StatusAssigned, a.UpdatedAt())
		b, _ = b.Transition(deployment.StatusAssigned, b.UpdatedAt())
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, candidate := range []deployment.Deployment{a, b} {
			wg.Add(1)
			go func(v deployment.Deployment) { defer wg.Done(); results <- repo.Save(ctx, v) }(candidate)
		}
		wg.Wait()
		close(results)
		var successes, conflicts int
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, store.ErrRevisionConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
		}
		if err := repo.Save(ctx, b); !errors.Is(err, store.ErrRevisionConflict) {
			t.Fatalf("stale save = %v", err)
		}
	})
	t.Run("event insertion failure rolls back head update and corrupt data is rejected", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-rollback")
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		value, _ = value.Transition(deployment.StatusAssigned, value.UpdatedAt())
		if _, err := pool.Exec(ctx, `ALTER TABLE deployment_events ADD CONSTRAINT reject_assigned CHECK (to_status <> 'assigned')`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `ALTER TABLE deployment_events DROP CONSTRAINT IF EXISTS reject_assigned`)
		})
		if err := repo.Save(ctx, value); err == nil {
			t.Fatal("save unexpectedly succeeded")
		}
		loaded, err := repo.GetByID(ctx, value.Identity().DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Status() != deployment.StatusQueued || loaded.Revision() != 1 {
			t.Fatalf("head was not rolled back: %#v", loaded)
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE deployment_events DROP CONSTRAINT reject_assigned`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE deployments SET revision=2 WHERE id=$1`, value.Identity().DeploymentID); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.GetByID(ctx, value.Identity().DeploymentID); !errors.Is(err, deployment.ErrInvalidSnapshot) {
			t.Fatalf("corruption = %v", err)
		}
	})
	t.Run("out-of-order event history is rejected", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-event-order")
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		value, _ = value.Transition(deployment.StatusAssigned, value.UpdatedAt().Add(time.Microsecond))
		value, _ = value.Transition(deployment.StatusFetching, value.UpdatedAt().Add(time.Microsecond))
		if err := repo.Save(ctx, value); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE deployment_events DISABLE TRIGGER ALL`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `ALTER TABLE deployment_events ENABLE TRIGGER ALL`) })
		if _, err := pool.Exec(ctx, `UPDATE deployment_events SET occurred_at = occurred_at + interval '1 second' WHERE deployment_id=$1 AND revision=2`, value.Identity().DeploymentID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE deployment_events ENABLE TRIGGER ALL`); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.GetByID(ctx, value.Identity().DeploymentID); !errors.Is(err, deployment.ErrInvalidSnapshot) {
			t.Fatalf("out-of-order event = %v", err)
		}
	})
	t.Run("concurrent reads observe complete aggregates", func(t *testing.T) {
		reset(t, pool)
		value := newDeployment(t, "deployment-read-consistency")
		if err := repo.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
		id := value.Identity().DeploymentID
		done := make(chan error, 1)
		go func() {
			for _, status := range []deployment.Status{deployment.StatusAssigned, deployment.StatusFetching, deployment.StatusBuilding} {
				var err error
				value, err = value.Transition(status, value.UpdatedAt())
				if err == nil {
					err = repo.Save(ctx, value)
				}
				if err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		for index := 0; index < 100; index++ {
			loaded, err := repo.GetByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Revision() != deployment.Revision(len(loaded.Events())) {
				t.Fatalf("mixed aggregate: revision=%d events=%d", loaded.Revision(), len(loaded.Events()))
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := migrations.Apply(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE deployment_events, deployments`); err != nil {
		t.Fatal(err)
	}
}
func newDeployment(t *testing.T, id deployment.ID) deployment.Deployment {
	t.Helper()
	value, err := deployment.New(deployment.Identity{DeploymentID: id, ProjectID: "project", EnvironmentID: "production", ServerID: "server", CommitSHA: "commit"}, time.Date(2026, 8, 5, 12, 0, 0, 123456789, time.FixedZone("offset", 19800)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
