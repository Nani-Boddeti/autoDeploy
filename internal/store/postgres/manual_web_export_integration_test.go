//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
)

// ManualDeployFixtureForTest is a bound project with a live owner session and
// the real webhook-job repository, exported to the external web test package.
type ManualDeployFixtureForTest struct {
	Jobs      *WebhookJobRepository
	Ops       *OperationsReadRepository
	Principal auth.Principal
	ProjectID string

	deployments *DeploymentRepository
}

func NewManualDeployFixtureForTest(t *testing.T, seed byte) ManualDeployFixtureForTest {
	t.Helper()
	githubRepo, jobs, principal := manualFixture(t, seed)
	return ManualDeployFixtureForTest{Jobs: jobs, Ops: NewOperationsReadRepository(githubRepo.pool), Principal: principal, ProjectID: "project", deployments: NewDeploymentRepository(githubRepo.pool)}
}

// FailDeploymentForTest drives a queued deployment through assigned to failed
// via the real aggregate and repository.
func (f ManualDeployFixtureForTest) FailDeploymentForTest(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	d, err := f.deployments.GetByID(ctx, deployment.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	at := d.UpdatedAt().Add(time.Second)
	for _, to := range []deployment.Status{deployment.StatusAssigned, deployment.StatusFailed} {
		if d, err = d.Transition(to, at); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
	}
	if err = f.deployments.Save(ctx, d); err != nil {
		t.Fatal(err)
	}
}
