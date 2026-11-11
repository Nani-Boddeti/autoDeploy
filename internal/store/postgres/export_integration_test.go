//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"autodeploy/internal/auth"
)

// BlockedEnvironmentForTest is a blocked environment built by the package's
// own fixtures, exported to the external test package (which can import
// internal/web, something package postgres cannot without an import cycle).
type BlockedEnvironmentForTest struct {
	Pool          *pgxpool.Pool
	Leases        *AgentLeaseRepository
	Principal     auth.Principal
	EnvironmentID string
}

// NewActivationBlockForTest blocks an environment through the real Activate
// and the real sweeper diversion: the activation is left reconciliation_required.
// withPredecessor gives the environment an active release (which makes its
// route hostname immutable).
func NewActivationBlockForTest(t *testing.T, seed byte, withPredecessor bool) BlockedEnvironmentForTest {
	t.Helper()
	a := newActivatedFixture(t, seed, withPredecessor)
	a.blockViaSweeperDiversion(t)
	return BlockedEnvironmentForTest{Pool: a.pool, Leases: a.repo, Principal: a.principal, EnvironmentID: a.envID}
}

// NewPlainBlockForTest blocks an environment through an ordinary expiry close
// with no activation.
func NewPlainBlockForTest(t *testing.T, seed byte) BlockedEnvironmentForTest {
	t.Helper()
	a := plainBlock(t, seed)
	return BlockedEnvironmentForTest{Pool: a.pool, Leases: a.repo, Principal: a.principal, EnvironmentID: a.envID}
}

// CancelFixtureForTest is a queued deployment/job for owner A plus a second
// owner B with a live session (opsFixture), exported to the external web
// test package for the 15d cancel-action end-to-end tests.
type CancelFixtureForTest struct {
	Pool         *pgxpool.Pool
	Leases       *AgentLeaseRepository
	Ops          *OperationsReadRepository
	Principal    auth.Principal
	PrincipalB   auth.Principal
	DeploymentID string
	JobID        int64

	f    opsFixture
	cred [32]byte
}

func NewCancelFixtureForTest(t *testing.T, seed byte) *CancelFixtureForTest {
	t.Helper()
	f := newOpsFixture(t, seed)
	var depID string
	if err := f.pool.QueryRow(context.Background(), `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&depID); err != nil {
		t.Fatal(err)
	}
	return &CancelFixtureForTest{
		Pool: f.pool, Leases: NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool)), Ops: f.repo,
		Principal: f.principal, PrincipalB: f.principalB, DeploymentID: depID, JobID: f.jobID,
		f: f, cred: f.withCredential(t, seed+50),
	}
}

// Lease moves the job to leased through the fixture's direct setup.
func (c *CancelFixtureForTest) Lease(t *testing.T) int64 {
	t.Helper()
	return c.f.leaseJob(t, 1, 90)
}

// Claim runs the real Claim for the fixture's server; it returns how many
// leases were granted.
func (c *CancelFixtureForTest) Claim(ctx context.Context) (int, error) {
	leases, err := c.Leases.Claim(ctx, c.f.serverID, c.cred, 5)
	return len(leases), err
}

// Activate leases the job and drives it through the real Activate, past the
// activating linearization point.
func (c *CancelFixtureForTest) Activate(t *testing.T) {
	t.Helper()
	attemptID := c.f.leaseJob(t, 1, 90)
	c.f.advanceDeploymentToHealthChecking(t)
	c.f.insertRouteHead(t, "app.example.com")
	c.f.insertCandidateRelease(t, attemptID, "release-web-cancel")
	if _, err := c.Leases.Activate(context.Background(), c.f.serverID, c.cred, attemptID, 1, digest(13)); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}

// State is the persisted state a cancel could change.
type CancelStateForTest struct {
	JobState         string
	CancelRequested  bool
	DeploymentStatus string
	Events           int
}

func (c *CancelFixtureForTest) State(t *testing.T) CancelStateForTest {
	t.Helper()
	var s CancelStateForTest
	if err := c.Pool.QueryRow(context.Background(), `SELECT j.state, j.cancel_requested_at IS NOT NULL, d.status, (SELECT count(*) FROM agent_job_events e WHERE e.job_id=j.id)
		FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id WHERE j.id=$1`, c.JobID).
		Scan(&s.JobState, &s.CancelRequested, &s.DeploymentStatus, &s.Events); err != nil {
		t.Fatal(err)
	}
	return s
}
