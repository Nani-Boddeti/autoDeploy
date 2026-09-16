//go:build integration

package postgres

import (
	"context"
	"testing"

	"autodeploy/internal/githubapp"
)

func qaCheckoutBinding(t *testing.T) (*GitHubRepository, githubapp.CheckoutBinding) {
	t.Helper()
	repo, _, principal := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(81), digest(82)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: principal}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, principal, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(ctx, principal, binding("qa_checkout_project"), "qa-checkout-bind"); err != nil {
		t.Fatal(err)
	}
	binding, err := repo.CaptureCheckoutBinding(ctx, principal.OwnerID, "qa_checkout_project")
	if err != nil {
		t.Fatal(err)
	}
	return repo, binding
}

func TestQACheckoutRevalidationRejectsEveryAuthorityRevision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"binding", `UPDATE github_project_bindings SET authorization_revision=authorization_revision+1 WHERE owner_id=$1 AND project_id='qa_checkout_project'`},
		{"repository", `UPDATE github_repositories SET authorization_revision=authorization_revision+1 WHERE owner_id=$1 AND repository_id=201`},
		{"installation", `UPDATE github_installations SET authorization_revision=authorization_revision+1 WHERE owner_id=$1 AND installation_id=101`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, binding := qaCheckoutBinding(t)
			if _, err := repo.pool.Exec(context.Background(), tc.query, binding.OwnerID); err != nil {
				t.Fatal(err)
			}
			if err := repo.RevalidateCheckoutBinding(context.Background(), binding); err == nil {
				t.Fatalf("stale %s authorization revision remained valid", tc.name)
			}
		})
	}
}

func TestQAActiveCheckoutReservationCannotBeReactivated(t *testing.T) {
	repo, binding := qaCheckoutBinding(t)
	ctx := context.Background()
	reservation, err := repo.ReserveCheckout(ctx, binding.OwnerID, binding.ProjectID, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.BeginMintAttempt(ctx, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ActivateMintAttempt(ctx, reservation.ID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BeginMintAttempt(ctx, reservation.ID); err == nil {
		t.Fatal("active reservation was replayed to begin a second token mint")
	}
}
