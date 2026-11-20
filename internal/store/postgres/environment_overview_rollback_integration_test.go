//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"autodeploy/internal/auth"

	"github.com/jackc/pgx/v5"
)

const ovOwnerC = "ov-owner-c"

// ovSecondOwner creates owner C with a live session and returns its principal.
func ovSecondOwner(t *testing.T, f leaseFixture) auth.Principal {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO owners(id) VALUES ('` + ovOwnerC + `') ON CONFLICT DO NOTHING`,
		`INSERT INTO users(id, owner_id, username, role, password_phc, password_policy_revision) VALUES ('ov-user-c','` + ovOwnerC + `','ov-user-c','owner',repeat('x',70),1) ON CONFLICT DO NOTHING`,
		`INSERT INTO admin_sessions(id, token_digest, user_id, owner_id, auth_revision, last_seen_at, idle_expires_at, absolute_expires_at)
		 VALUES ('ov-sess-c', decode(repeat('5c',32),'hex'), 'ov-user-c', '` + ovOwnerC + `', 1, clock_timestamp(), clock_timestamp()+interval '1 hour', clock_timestamp()+interval '2 hours') ON CONFLICT DO NOTHING`,
	} {
		ovExec(t, f.pool, q)
	}
	return auth.Principal{UserID: "ov-user-c", OwnerID: ovOwnerC, Role: auth.RoleOwner, AuthRevision: 1, SessionID: "ov-sess-c"}
}

func (w rollbackWorld) overviewRow(t *testing.T, p auth.Principal) EnvironmentOverview {
	t.Helper()
	rows, err := NewOperationsReadRepository(w.pool).ListEnvironmentOverview(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return ovRow(t, rows, w.envID)
}

// By construction: whatever the overview offers as rollback target, the real
// RollbackEligible must accept; and every ineligible variant is never offered.
func TestEnvironmentOverviewRollbackOfferMatchesRollbackEligibility(t *testing.T) {
	ctx := context.Background()
	w := newRollbackWorld(t, 190)
	r := w.overviewRow(t, w.principal)
	if r.RollbackTargetReleaseID != w.relA || r.RollbackTargetCommit != "abcdefabcdefabcdefabcdefabcdefabcdefabcd" {
		t.Fatalf("offer = %q %q, want %q", r.RollbackTargetReleaseID, r.RollbackTargetCommit, w.relA)
	}
	if ok, err := w.repo.RollbackEligible(ctx, w.principal, r.RollbackTargetReleaseID); err != nil || !ok {
		t.Fatalf("offered target not eligible: %v %v", ok, err)
	}
	// Converse: every release the real path accepts is the one offered.
	for _, id := range []string{w.relA, w.relB} {
		ok, err := w.repo.RollbackEligible(ctx, w.principal, id)
		if err != nil {
			t.Fatal(err)
		}
		if ok != (r.RollbackTargetReleaseID == id) {
			t.Fatalf("RollbackEligible(%q)=%v but offer=%q", id, ok, r.RollbackTargetReleaseID)
		}
	}
	if r.ActiveReleaseID != w.relB || r.ActiveCommit == "" || r.ActiveSince.IsZero() {
		t.Fatalf("active = %+v", r)
	}

	// Newer ineligible releases are never preferred over the eligible one.
	sfx := releaseSuffix(t)
	other := w.leaseFixture
	other.ownerID = ovOwnerC
	ovSecondOwner(t, w.leaseFixture)
	w.insertServer(t, "server2")
	otherServer := w.leaseFixture
	otherServer.serverID = "server2"
	for name, mk := range map[string]func() string{
		"ready": func() string {
			id := "v-ready-" + sfx
			w.insertHistoricRelease(t, id, "ready", "r", true, "blue_green", 9090, 5)
			return id
		},
		"candidate": func() string {
			id := "v-cand-" + sfx
			w.insertHistoricRelease(t, id, "candidate", "c", true, "blue_green", 9090, 5)
			return id
		},
		"removed": func() string {
			id := "v-rem-" + sfx
			w.insertHistoricRelease(t, id, "removed", "m", true, "blue_green", 9090, 5)
			return id
		},
		"never activated": func() string {
			id := "v-never-" + sfx
			w.insertHistoricRelease(t, id, "retained", "n", false, "blue_green", 9090, 5)
			return id
		},
		"other server": func() string {
			id := "v-srv-" + sfx
			otherServer.insertHistoricRelease(t, id, "retained", "s", true, "blue_green", 9090, 5)
			return id
		},
		"retained release of another environment": func() string {
			id := "v-env-" + sfx
			g := w.leaseFixture
			g.envID = "environment-2"
			g.insertRuntimeConfig(t, g.envID)
			g.insertHistoricRelease(t, id, "retained", "e", true, "blue_green", 9090, 5)
			return id
		},
		"release row of another environment but job in this one": func() string {
			id := "v-re-" + sfx
			w.insertHistoricRelease(t, id, "retained", "k", true, "blue_green", 9090, 5)
			asReplica(t, w.pool, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `UPDATE releases SET environment_id='environment-2' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			})
			return id
		},
		"target job in another environment": func() string {
			id := "v-tj-" + sfx
			g := w.leaseFixture
			g.envID = "environment-3"
			g.insertRuntimeConfig(t, g.envID)
			g.insertHistoricRelease(t, id, "retained", "j", true, "blue_green", 9090, 5)
			asReplica(t, w.pool, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `UPDATE releases SET environment_id=$2 WHERE id=$1`, id, w.envID); err != nil {
					t.Fatal(err)
				}
			})
			return id
		},
		"other owner": func() string {
			id := "v-own-" + sfx
			other.insertHistoricRelease(t, id, "retained", "o", true, "blue_green", 9090, 5)
			return id
		},
	} {
		id := mk()
		got := w.overviewRow(t, w.principal).RollbackTargetReleaseID
		if got == id {
			t.Fatalf("%s variant %q offered as rollback target", name, id)
		}
		if got != w.relA {
			t.Fatalf("%s: offer = %q, want %q", name, got, w.relA)
		}
		if ok, err := w.repo.RollbackEligible(ctx, w.principal, id); err != nil || ok {
			t.Fatalf("%s: RollbackEligible = %v, %v", name, ok, err)
		}
	}
}

func TestEnvironmentOverviewRollbackNotOfferedWhenPathWouldRefuse(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		seed   byte
		mutate func(t *testing.T, w rollbackWorld)
	}{
		"slot reconciling": {191, func(t *testing.T, w rollbackWorld) {
			ovExec(t, w.pool, `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, w.envID, w.attemptB)
		}},
		"head flagged": {192, func(t *testing.T, w rollbackWorld) {
			ovExec(t, w.pool, `UPDATE route_heads SET reconciliation_required=true WHERE environment_id=$1`, w.envID)
		}},
		"in flight": {193, func(t *testing.T, w rollbackWorld) {
			w.insertJobAt(t, w.serverID, w.envID, "ov-inflight", 0, "clock_timestamp()")
		}},
		"no runtime config": {194, func(t *testing.T, w rollbackWorld) {
			asReplica(t, w.pool, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `DELETE FROM environment_runtime_configs WHERE environment_id=$1`, w.envID); err != nil {
					t.Fatal(err)
				}
			})
		}},
		"source not active while head points at it": {197, func(t *testing.T, w rollbackWorld) {
			asReplica(t, w.pool, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `UPDATE releases SET state='retained' WHERE id=$1`, w.relB); err != nil {
					t.Fatal(err)
				}
			})
		}},
		"slot occupied": {198, func(t *testing.T, w rollbackWorld) {
			asReplica(t, w.pool, func(tx pgx.Tx) {
				var a int64
				if err := tx.QueryRow(context.Background(), `WITH t AS (SELECT clock_timestamp() + interval '1 hour' AS d)
					INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
					SELECT $1,1,$2,$3,50,'leased',t.d,t.d FROM t RETURNING id`, w.jobID, w.serverID, w.envID).Scan(&a); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(context.Background(), `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, w.envID, a); err != nil {
					t.Fatal(err)
				}
			})
		}},
		"missing slot row": {199, func(t *testing.T, w rollbackWorld) {
			asReplica(t, w.pool, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `DELETE FROM environment_slots WHERE environment_id=$1`, w.envID); err != nil {
					t.Fatal(err)
				}
			})
		}},
		"revoked frozen secret": {195, func(t *testing.T, w rollbackWorld) {
			sfx := fmt.Sprintf("%s-%d", releaseSuffix(t), time.Now().UnixNano())
			w.insertSecretHeadWithActiveVersion(t, "rb-head-1-"+sfx, w.envID, "TOKEN", "runtime", "rb-v1-"+sfx, 1)
			ovExec(t, w.pool, `INSERT INTO frozen_secret_versions(attempt_id, secret_version_id, variable_name, delivery_scope) VALUES ($1,$2,'TOKEN','runtime')`, w.attemptA, "rb-v1-"+sfx)
			w.rotateSecretVersion(t, "rb-head-1-"+sfx, w.envID, "rb-v2-"+sfx, 2)
			ovExec(t, w.pool, `UPDATE secret_versions SET state='revoked' WHERE id=$1`, "rb-v1-"+sfx)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newRollbackWorld(t, c.seed)
			if got := w.overviewRow(t, w.principal).RollbackTargetReleaseID; got != w.relA {
				t.Fatalf("baseline offer = %q", got)
			}
			c.mutate(t, w)
			if ok, err := w.repo.RollbackEligible(ctx, w.principal, w.relA); err != nil || ok {
				t.Fatalf("path still eligible: %v %v", ok, err)
			}
			if got := w.overviewRow(t, w.principal).RollbackTargetReleaseID; got != "" {
				t.Fatalf("offered %q though the rollback path refuses", got)
			}
		})
	}
}

// M1/H2: A and C both have jobs on the environment; every release belongs to
// A. C sees the environment-level route fields (same as GetRouteHead) but no
// release-level fields and no rollback target.
func TestEnvironmentOverviewBothOwnersOnSameEnvironment(t *testing.T) {
	w := newRollbackWorld(t, 196)
	c := ovSecondOwner(t, w.leaseFixture)
	seedOpsDeployment(t, w.pool, ovOwnerC, "project", w.envID, "c-env-dep", time.Now().UTC())

	a := w.overviewRow(t, w.principal)
	if !a.Owned || a.ActiveReleaseID != w.relB || a.ActiveCommit == "" || a.ActiveSince.IsZero() || a.RollbackTargetReleaseID != w.relA {
		t.Fatalf("A (release owner) row = %+v", a)
	}
	cr := w.overviewRow(t, c)
	if !cr.Owned || cr.Hostname != "app.example.com" || cr.ActiveReleaseID != w.relB {
		t.Fatalf("C environment-level fields = %+v", cr)
	}
	if cr.ActiveCommit != "" || !cr.ActiveSince.IsZero() || cr.RollbackTargetReleaseID != "" || cr.RollbackTargetCommit != "" {
		t.Fatalf("RELEASE-LEVEL LEAK to non-owner of the releases: %+v", cr)
	}
}
