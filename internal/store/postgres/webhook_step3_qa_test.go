//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/github"
)

func TestQALifecycleRepositorySetExactReplayAndSealing(t *testing.T) {
	githubRepo, _, _ := webhookAuthority(t, "webhook-step3-qa", 111, 112)
	resetWebhookJobs(t, githubRepo.pool)
	if _, err := githubRepo.pool.Exec(context.Background(), `INSERT INTO github_repositories(repository_id,installation_id,owner_id,github_owner_id,name,full_name,clone_url) SELECT 202,installation_id,owner_id,github_owner_id,'repo-two','owner/repo-two','https://github.com/owner/repo-two.git' FROM github_repositories WHERE repository_id=201`); err != nil {
		t.Fatal(err)
	}
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	delivery := lifecycleDelivery("lifecycle-multi-repository", 81, github.EventInstallationRepositories)
	lifecycle := github.Lifecycle{Event: github.EventInstallationRepositories, Action: "removed", InstallationID: 101, RepositoryID: 201, RepositoryIDs: []int64{201, 202}, OwnerID: 9}

	if err := repo.ProcessLifecycle(context.Background(), delivery, lifecycle); err != nil {
		t.Fatal(err)
	}
	if err := repo.ProcessLifecycle(context.Background(), delivery, lifecycle); err != nil {
		t.Fatalf("exact multi-repository replay: %v", err)
	}

	changed := lifecycle
	changed.RepositoryIDs = []int64{201}
	if err := repo.ProcessLifecycle(context.Background(), delivery, changed); !errors.Is(err, ErrWebhookIntegrity) {
		t.Fatalf("changed repository set error=%v", err)
	}

	var deliveryID int64
	if err := githubRepo.pool.QueryRow(context.Background(), `SELECT id FROM webhook_deliveries WHERE delivery_id=$1`, delivery.DeliveryID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE webhook_lifecycle_repositories SET repository_id=203 WHERE delivery_id=$1 AND repository_id=202`,
		`DELETE FROM webhook_lifecycle_repositories WHERE delivery_id=$1 AND repository_id=202`,
		`INSERT INTO webhook_lifecycle_repositories(delivery_id,repository_id) VALUES($1,203)`,
	} {
		if _, err := githubRepo.pool.Exec(context.Background(), query, deliveryID); err == nil {
			t.Fatalf("sealed repository set mutation succeeded: %s", query)
		}
	}
}
