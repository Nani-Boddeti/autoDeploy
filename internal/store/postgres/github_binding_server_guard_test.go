//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"
)

// bindingGuardFixture returns a GitHub repository with one completed
// installation (repository 201) and the session principal.
func bindingGuardFixture(t *testing.T) (*GitHubRepository, auth.Principal, githubapp.BindingInput, func(owner, id, status string)) {
	t.Helper()
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(81), digest(82)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	seed := func(owner, id, status string) {
		t.Helper()
		if _, err := repo.pool.Exec(ctx, `INSERT INTO servers(id,owner_id,name,protocol_version,status) VALUES($1,$2,$1,1,'active')`, id, owner); err != nil {
			t.Fatal(err)
		}
		if status == "revoked" {
			if _, err := repo.pool.Exec(ctx, `UPDATE servers SET status='revoked',revoked_at=clock_timestamp() WHERE id=$1 AND status='active'`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	return repo, p, binding("guard_project"), seed
}

func TestBindRepositoryRefusesUnusableTargetServer(t *testing.T) {
	repo, p, in, seed := bindingGuardFixture(t)
	ctx := context.Background()
	if _, err := repo.pool.Exec(ctx, `INSERT INTO owners(id) VALUES('guard-foreign-owner') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	seed(p.OwnerID, "guard_ok", "active")
	seed(p.OwnerID, "guard_ok_2", "active")
	seed(p.OwnerID, "guard_revoked", "revoked")
	seed("guard-foreign-owner", "guard_foreign", "active")

	count := func() (bindings, audits int) {
		if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM github_project_bindings),(SELECT count(*) FROM github_audit_events WHERE action='repository_bound')`).Scan(&bindings, &audits); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Valid target: unchanged behaviour.
	in.ServerID = "guard_ok"
	if err := repo.BindRepository(ctx, p, in, "guard-bind-1"); err != nil {
		t.Fatalf("valid bind: %v", err)
	}
	b0, a0 := count()

	// Re-pointing an existing binding at an unusable server is refused and
	// leaves the binding (server, revision) and the audit trail untouched.
	for name, target := range map[string]string{"missing": "guard_missing", "revoked": "guard_revoked", "foreign owner": "guard_foreign"} {
		bad := in
		bad.ServerID = target
		if err := repo.BindRepository(ctx, p, bad, "guard-bad-"+target); !errors.Is(err, ErrBindingServerUnavailable) {
			t.Fatalf("%s target: err=%v, want ErrBindingServerUnavailable", name, err)
		}
		var server string
		var revision int64
		if err := repo.pool.QueryRow(ctx, `SELECT server_id,authorization_revision FROM github_project_bindings WHERE owner_id=$1 AND project_id=$2`, p.OwnerID, in.ProjectID).Scan(&server, &revision); err != nil || server != "guard_ok" || revision != 1 {
			t.Fatalf("%s: binding changed to %q rev=%d err=%v", name, server, revision, err)
		}
		if b, a := count(); b != b0 || a != a0 {
			t.Fatalf("%s: counts changed %d/%d -> %d/%d", name, b0, a0, b, a)
		}
	}

	// A valid re-point still works and bumps the revision.
	in.ServerID = "guard_ok_2"
	if err := repo.BindRepository(ctx, p, in, "guard-bind-2"); err != nil {
		t.Fatalf("valid re-point: %v", err)
	}
	var server string
	var revision int64
	if err := repo.pool.QueryRow(ctx, `SELECT server_id,authorization_revision FROM github_project_bindings WHERE owner_id=$1 AND project_id=$2`, p.OwnerID, in.ProjectID).Scan(&server, &revision); err != nil || server != "guard_ok_2" || revision != 2 {
		t.Fatalf("after valid re-point: %q rev=%d err=%v", server, revision, err)
	}
}

func TestBindRepositoryRefusesFreshBindToUnusableServer(t *testing.T) {
	repo, p, in, seed := bindingGuardFixture(t)
	ctx := context.Background()
	seed(p.OwnerID, "guard_revoked", "revoked")
	for _, target := range []string{"guard_missing", "guard_revoked"} {
		in.ServerID = target
		if err := repo.BindRepository(ctx, p, in, "guard-fresh-"+target); !errors.Is(err, ErrBindingServerUnavailable) {
			t.Fatalf("fresh bind to %s: %v", target, err)
		}
	}
	var bindings, audits int
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM github_project_bindings),(SELECT count(*) FROM github_audit_events WHERE action='repository_bound')`).Scan(&bindings, &audits); err != nil || bindings != 0 || audits != 0 {
		t.Fatalf("refused binds left rows: bindings=%d audits=%d err=%v", bindings, audits, err)
	}
}
