//go:build integration

package postgres

import (
	"context"
	"testing"
)

func TestQASecretHeadCannotReferenceAnotherSecretsVersion(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	ctx := context.Background()
	if err := repository.Create(ctx, secretMutation("secret-a", "version-a", "audit-a")); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, secretMutation("secret-b", "version-b", "audit-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx,
		`UPDATE secret_heads SET current_version_id='version-b' WHERE id='secret-a'`); err == nil {
		t.Fatal("database accepted a current version belonging to another secret")
	}
}

func TestQASecretVersionScopeMustMatchHead(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	ctx := context.Background()
	mutation := secretMutation("secret-a", "version-a", "audit-a")
	if err := repository.Create(ctx, mutation); err != nil {
		t.Fatal(err)
	}
	v := mutation.Version
	if _, err := repository.pool.Exec(ctx, `INSERT INTO secret_versions(
		id,secret_id,owner_id,project_id,environment_id,version_number,state,
		suite,aad_version,kek_id,ciphertext,wrapped_dek
	) VALUES('version-corrupt',$1,'other-owner',$2,$3,2,'active',$4,$5,$6,$7,$8)`,
		v.Scope.SecretID, v.Scope.ProjectID, v.Scope.EnvironmentID, v.Envelope.Suite,
		v.Envelope.AADVersion, v.Envelope.KEKID, v.Envelope.Ciphertext, v.Envelope.WrappedDEK); err == nil {
		t.Fatal("database accepted a secret version whose owner scope differs from its head")
	}
}

func TestQASecretAuditScopeMustMatchSecret(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	ctx := context.Background()
	if err := repository.Create(ctx, secretMutation("secret-a", "version-a", "audit-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO secret_audit_events(
		id,owner_id,secret_id,secret_version_id,action
	) VALUES('audit-corrupt','other-owner','secret-a','version-a','materialized')`); err == nil {
		t.Fatal("database accepted an audit owner that differs from its secret")
	}
}
