//go:build integration

package postgres

import (
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
