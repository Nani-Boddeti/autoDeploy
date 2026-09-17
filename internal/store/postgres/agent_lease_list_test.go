//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"autodeploy/internal/auth"
)

// TestAgentLeaseListReconcilingEnvironmentsOwnerScoped exercises the web
// admin /servers page's reconciling-environments read: an environment only
// appears once its leased attempt has actually expired and the slot flagged
// reconciliation_required, ownership is derived from the pending attempt's
// job (mirroring MarkReconciled), a different owner sees none of it, and
// resolving the reconciliation removes it from the list.
func TestAgentLeaseListReconcilingEnvironmentsOwnerScoped(t *testing.T) {
	f := newLeaseFixture(t, 250)
	attemptID := f.leaseJob(t, 1, 1)
	time.Sleep(1100 * time.Millisecond)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()
	if _, err := repo.ExpireBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}

	environments, err := repo.ListReconcilingEnvironments(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(environments) != 1 {
		t.Fatalf("expected 1 reconciling environment, got %d: %+v", len(environments), environments)
	}
	got := environments[0]
	if got.EnvironmentID != f.envID || got.AttemptID != attemptID || got.JobID != f.jobID || got.ServerID != f.serverID || got.Since.IsZero() {
		t.Fatalf("environment=%+v attemptID=%d jobID=%d serverID=%s", got, attemptID, f.jobID, f.serverID)
	}

	// A different owner's principal must never see this owner's reconciling
	// environments -- cross-owner isolation.
	otherPrincipal := secondOwnerPrincipal(t, f.pool, NewAuthRepository(f.pool), 251)
	otherEnvironments, err := repo.ListReconcilingEnvironments(ctx, otherPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherEnvironments) != 0 {
		t.Fatalf("cross-owner leak: %+v", otherEnvironments)
	}

	// Resolving the reconciliation removes it from the list.
	if err = repo.MarkReconciled(ctx, f.principal, f.envID, "operator break-glass: verified clean"); err != nil {
		t.Fatal(err)
	}
	cleared, err := repo.ListReconcilingEnvironments(ctx, f.principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Fatalf("expected no reconciling environments after mark-reconciled, got %+v", cleared)
	}
}

func TestAgentLeaseListReconcilingEnvironmentsInvalidPrincipalDenied(t *testing.T) {
	f := newLeaseFixture(t, 252)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.ListReconcilingEnvironments(context.Background(), auth.Principal{}); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("invalid principal err=%v", err)
	}
}
