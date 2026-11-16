//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/secret"
)

// TestSecretAdminPutBindClaimFreezeAndReplace proves the terminal tool's
// data path end to end: scope derived from the environment, a head created
// through the real service and repository is accepted by the binding add
// path, a claim freezes that version, and a later replace does not move the
// frozen one.
func TestSecretAdminPutBindClaimFreezeAndReplace(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	f := newERConfigFixture(t, 185)
	d := f.withCredential(t, 241)
	ctx := context.Background()
	service := secret.NewService(repository, secretStep2Ring(t, "kek-new"), nil)

	owner, project, err := repository.EnvironmentOwnerProject(ctx, f.envID)
	if err != nil || owner != f.ownerID || project == "" {
		t.Fatalf("scope owner=%q project=%q err=%v", owner, project, err)
	}
	if _, _, err := repository.EnvironmentOwnerProject(ctx, "no-such-environment"); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("unknown environment err=%v", err)
	}

	put := func(id, versionID, scope string) {
		t.Helper()
		s := secret.Scope{OwnerID: owner, ProjectID: project, EnvironmentID: f.envID, SecretID: id, Purpose: scope, DeliveryScope: scope}
		meta, err := service.CreateWithContext(ctx, s, versionID, secret.OperationContext{AuditID: "audit-" + id, ActorID: "terminal:tester"}, []byte("sentinel-value"))
		if err != nil || meta.Number != 1 {
			t.Fatalf("create %s: %+v %v", id, meta, err)
		}
	}
	put("head-runtime", "version-runtime-1", "runtime")
	put("head-build", "version-build-1", "build")

	for _, b := range []BindingInput{
		{VariableName: "RUNTIME_TOKEN", BindingKind: "secret", DeliveryScope: "runtime", SecretHeadID: "head-runtime"},
		{VariableName: "NPM_TOKEN", BindingKind: "secret", DeliveryScope: "build", SecretHeadID: "head-build"},
	} {
		if _, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, f.envID, b); err != nil {
			t.Fatalf("bind %s: %v", b.VariableName, err)
		}
	}
	// A head cannot be bound under the other delivery scope.
	if _, err := f.repo.AddEnvironmentRuntimeBinding(ctx, f.principal, f.envID, BindingInput{VariableName: "WRONG_SCOPE", BindingKind: "secret", DeliveryScope: "build", SecretHeadID: "head-runtime"}); !errors.Is(err, ErrEnvironmentRuntimeBindingInvalid) {
		t.Fatalf("scope mismatch err=%v", err)
	}

	head, err := repository.SecretHeadScope(ctx, "head-build")
	if err != nil || head.OwnerID != owner || head.ProjectID != project || head.EnvironmentID != f.envID || head.Purpose != "build" || head.DeliveryScope != "build" || head.SecretID != "head-build" {
		t.Fatalf("head scope=%+v err=%v", head, err)
	}
	if _, err := repository.SecretHeadScope(ctx, "no-such-head"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("unknown head err=%v", err)
	}

	leases, err := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool)).Claim(ctx, f.serverID, d, 5)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim leases=%+v err=%v", leases, err)
	}
	frozen := func() map[string]string {
		rows, err := f.pool.Query(ctx, `SELECT variable_name, secret_version_id FROM frozen_secret_versions WHERE attempt_id=$1`, leases[0].AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var n, v string
			if err := rows.Scan(&n, &v); err != nil {
				t.Fatal(err)
			}
			m[n] = v
		}
		return m
	}
	if m := frozen(); len(m) != 2 || m["RUNTIME_TOKEN"] != "version-runtime-1" || m["NPM_TOKEN"] != "version-build-1" {
		t.Fatalf("frozen=%v", m)
	}

	meta, err := service.ReplaceWithContext(ctx, head, "version-build-2", secret.OperationContext{AuditID: "audit-replace", ActorID: "terminal:tester"}, []byte("sentinel-value-2"))
	if err != nil || meta.Number != 2 {
		t.Fatalf("replace: %+v %v", meta, err)
	}
	if m := frozen(); m["NPM_TOKEN"] != "version-build-1" {
		t.Fatalf("replace moved the frozen version: %v", m)
	}
}
