//go:build integration

package postgres

import (
	"testing"

	"autodeploy/internal/auth"
)

// ManualDeployFixtureForTest is a bound project with a live owner session and
// the real webhook-job repository, exported to the external web test package.
type ManualDeployFixtureForTest struct {
	Jobs      *WebhookJobRepository
	Ops       *OperationsReadRepository
	Principal auth.Principal
	ProjectID string
}

func NewManualDeployFixtureForTest(t *testing.T, seed byte) ManualDeployFixtureForTest {
	t.Helper()
	githubRepo, jobs, principal := manualFixture(t, seed)
	return ManualDeployFixtureForTest{Jobs: jobs, Ops: NewOperationsReadRepository(githubRepo.pool), Principal: principal, ProjectID: "project"}
}
