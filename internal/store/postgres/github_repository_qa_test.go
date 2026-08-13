//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/githubapp"
)

// These QA tests exercise authority invalidation and lifecycle cleanup paths
// that are distinct from the happy-path persistence tests.
func TestQAGitHubBindingRejectsRevokedSession(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(41), digest(42)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE admin_sessions SET revoked_at=clock_timestamp(),revoked_reason='logout' WHERE id=$1`, p.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(ctx, p, "revoked_session_project", 201, "qa-revoked-session"); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("revoked session retained binding authority: %v", err)
	}
}

func TestQAGitHubRediscoveryDisablesRemovedRepositoryAndBinding(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	firstState, firstHandoff := digest(51), digest(52)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: firstState, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, firstState, firstHandoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, firstHandoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(ctx, p, "removed_repo_project", 201, "qa-bind-before-removal"); err != nil {
		t.Fatal(err)
	}

	secondState, secondHandoff := digest(53), digest(54)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: secondState, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, secondState, secondHandoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, secondHandoff, p, installation(), nil); err != nil {
		t.Fatal(err)
	}
	var repositoryActive, bindingActive bool
	var repositoryRevision, bindingRevision int64
	if err := repo.pool.QueryRow(ctx, `SELECT r.active,r.authorization_revision,b.active,b.authorization_revision FROM github_repositories r JOIN github_project_bindings b ON b.repository_id=r.repository_id AND b.owner_id=r.owner_id WHERE r.repository_id=201 AND r.owner_id=$1`, p.OwnerID).Scan(&repositoryActive, &repositoryRevision, &bindingActive, &bindingRevision); err != nil {
		t.Fatal(err)
	}
	if repositoryActive || bindingActive || repositoryRevision < 2 || bindingRevision < 2 {
		t.Fatalf("removed repository remained authorized: repository active=%t revision=%d binding active=%t revision=%d", repositoryActive, repositoryRevision, bindingActive, bindingRevision)
	}
}

func TestQAGitHubExpiredHandoffCleanupDoesNotBlockNewSetup(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(61), digest(62)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE github_pending_handoffs SET created_at=clock_timestamp()-interval '22 minutes', expires_at=clock_timestamp()-interval '12 minutes' WHERE digest=$1`, handoff[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE github_setup_states SET created_at=clock_timestamp()-interval '22 minutes', expires_at=clock_timestamp()-interval '12 minutes', consumed_at=clock_timestamp()-interval '11 minutes' WHERE digest=$1`, state[:]); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: digest(63), Principal: p}); err != nil {
		t.Fatalf("expired handoff blocked setup-state cleanup: %v", err)
	}
}
