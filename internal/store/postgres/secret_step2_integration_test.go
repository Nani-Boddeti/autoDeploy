//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"autodeploy/internal/crypto"
	"autodeploy/internal/secret"
)

type secretStep2Authorizer struct{ err error }

func (a secretStep2Authorizer) AuthorizeSecret(context.Context, secret.AuthorizationRequest) error {
	return a.err
}

func secretStep2Ring(t *testing.T, active string) crypto.KeyRing {
	t.Helper()
	ring, err := crypto.NewKeyRing(active, map[string][]byte{
		"kek-old": bytes.Repeat([]byte{1}, 32),
		"kek-new": bytes.Repeat([]byte{2}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func secretStep2Scope(id string) secret.Scope {
	return secret.Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "environment", SecretID: id, Purpose: "runtime", DeliveryScope: "runtime"}
}

func secretStep2Create(t *testing.T, service *secret.Service, scope secret.Scope, versionID, auditID string) {
	t.Helper()
	if _, err := service.CreateWithContext(context.Background(), scope, versionID, secret.OperationContext{AuditID: auditID, ActorID: "actor"}, []byte("step2 plaintext")); err != nil {
		t.Fatal(err)
	}
}

func TestSecretStep2MaterializationAuditsAuthorizedActorAndService(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	scope := secretStep2Scope("materialize")
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	secretStep2Create(t, service, scope, "version-1", "create-audit")

	actorRequest := secret.AuthorizationRequest{Scope: scope, VersionID: "version-1", ActorID: "actor-materialize", DeploymentID: "deployment", LeaseID: "lease"}
	plaintext, err := service.MaterializeAuthorized(context.Background(), actorRequest, "materialize-actor")
	if err != nil || string(plaintext) != "step2 plaintext" {
		t.Fatalf("actor materialization = %q, %v", plaintext, err)
	}
	serviceRequest := actorRequest
	serviceRequest.ActorID, serviceRequest.ServiceID = "", "worker-service"
	plaintext, err = service.MaterializeAuthorized(context.Background(), serviceRequest, "materialize-service")
	if err != nil || string(plaintext) != "step2 plaintext" {
		t.Fatalf("service materialization = %q, %v", plaintext, err)
	}

	var successes int
	var actorID, serviceID string
	err = repository.pool.QueryRow(context.Background(), `SELECT count(*), max(actor_id), max(service_id) FROM secret_audit_events WHERE secret_id=$1 AND action='materialized' AND result='success'`, scope.SecretID).Scan(&successes, &actorID, &serviceID)
	if err != nil || successes != 2 || actorID != "actor-materialize" || serviceID != "worker-service" {
		t.Fatalf("success audits = count:%d actor:%q service:%q err:%v", successes, actorID, serviceID, err)
	}
}

func TestSecretStep2MaterializationFailureAuditsAndSuppressesPlaintext(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	scope := secretStep2Scope("materialize-failure")
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	secretStep2Create(t, service, scope, "version-1", "create-audit")
	denied := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{err: errors.New("denied")})
	request := secret.AuthorizationRequest{Scope: scope, VersionID: "version-1", ActorID: "actor"}
	if plaintext, err := denied.MaterializeAuthorized(context.Background(), request, "denied-audit"); !errors.Is(err, secret.ErrUnauthorized) || plaintext != nil {
		t.Fatalf("denied materialization = %q, %v", plaintext, err)
	}
	wrongRing, err := crypto.NewKeyRing("kek-old", map[string][]byte{"kek-old": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	decryptFailed := secret.NewService(repository, wrongRing, secretStep2Authorizer{})
	if plaintext, err := decryptFailed.MaterializeAuthorized(context.Background(), request, "failed-audit"); !errors.Is(err, secret.ErrCorrupt) || plaintext != nil {
		t.Fatalf("decrypt failure = %q, %v", plaintext, err)
	}
	if _, err := repository.pool.Exec(context.Background(), `ALTER TABLE secret_audit_events ADD CONSTRAINT reject_step2_audit CHECK (id <> 'audit-write-failure')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repository.pool.Exec(context.Background(), `ALTER TABLE secret_audit_events DROP CONSTRAINT IF EXISTS reject_step2_audit`)
	})
	if plaintext, err := service.MaterializeAuthorized(context.Background(), request, "audit-write-failure"); !errors.Is(err, secret.ErrCorrupt) || plaintext != nil {
		t.Fatalf("audit failure = %q, %v", plaintext, err)
	}
	var deniedCount, failedCount int
	if err := repository.pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE result='denied'), count(*) FILTER (WHERE result='failed') FROM secret_audit_events WHERE secret_id=$1 AND action='materialized'`, scope.SecretID).Scan(&deniedCount, &failedCount); err != nil || deniedCount != 1 || failedCount != 1 {
		t.Fatalf("failure audits = denied:%d failed:%d err:%v", deniedCount, failedCount, err)
	}
	missing := secret.Audit{ID: "missing-audit", OwnerID: scope.OwnerID, SecretID: "missing-secret", VersionID: "missing-version", Action: "materialized", ActorID: "actor", Result: "failed", KEKID: "kek-old"}
	if err := repository.AppendMaterializationAudit(context.Background(), missing); !errors.Is(err, secret.ErrCorrupt) {
		t.Fatalf("missing materialization audit = %v, want RowsAffected corruption", err)
	}
}

func TestSecretStep2RewrapCASPreservesValueAndScope(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	scope := secretStep2Scope("rewrap")
	oldRing := secretStep2Ring(t, "kek-old")
	service := secret.NewService(repository, oldRing, secretStep2Authorizer{})
	secretStep2Create(t, service, scope, "version-1", "create-audit")
	before, err := repository.GetVersion(context.Background(), scope, "version-1")
	if err != nil {
		t.Fatal(err)
	}
	newEnvelope, err := crypto.Encrypt(secretStep2Ring(t, "kek-new"), repository, crypto.Scope{OwnerID: scope.OwnerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, VariableID: scope.SecretID, SecretVersionID: before.ID, Purpose: scope.Purpose, DeliveryScope: scope.DeliveryScope}, []byte("different value must not persist"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Rewrap(context.Background(), scope, before.ID, before.EnvelopeRevision, newEnvelope); err != nil {
		t.Fatal(err)
	}
	after, err := repository.GetVersion(context.Background(), scope, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Envelope.KEKID != "kek-new" || bytes.Equal(after.Envelope.WrappedDEK, before.Envelope.WrappedDEK) || after.EnvelopeRevision != before.EnvelopeRevision+1 || !bytes.Equal(after.Envelope.Ciphertext, before.Envelope.Ciphertext) || after.ID != before.ID || after.Number != before.Number || after.Scope != before.Scope {
		t.Fatalf("rewrap changed immutable fields: before=%#v after=%#v", before, after)
	}
	if err := repository.Rewrap(context.Background(), scope, before.ID, before.EnvelopeRevision, newEnvelope); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale rewrap = %v", err)
	}
	if err := repository.Rewrap(context.Background(), scope, before.ID, math.MaxUint64, newEnvelope); err == nil {
		t.Fatal("overflow rewrap accepted")
	}
	if err := repository.Rewrap(context.Background(), scope, before.ID, after.EnvelopeRevision, after.Envelope); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("no-op rewrap = %v", err)
	}
}

func TestSecretStep2ConcurrentReplaceHasOneWinnerAndLockedLineage(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	scope := secretStep2Scope("replace-race")
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	secretStep2Create(t, service, scope, "version-1", "create-audit")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := service.ReplaceWithContext(context.Background(), scope, "version-race-"+string(rune('a'+i)), secret.OperationContext{AuditID: "replace-audit-" + string(rune('a'+i)), ActorID: "actor"}, []byte("replacement"))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	var winners int
	for err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("replace winners = %d, want 1", winners)
	}
	var oldVersion string
	if err := repository.pool.QueryRow(context.Background(), `SELECT old_version_id FROM secret_audit_events WHERE secret_id=$1 AND action='replaced'`, scope.SecretID).Scan(&oldVersion); err != nil || oldVersion != "version-1" {
		t.Fatalf("replacement lineage = %q, %v", oldVersion, err)
	}
}

func TestSecretCompromiseReplacementCreatesNewCiphertextAndRevokesOld(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	scope := secretStep2Scope("compromised")
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	secretStep2Create(t, service, scope, "version-1", "create-audit")
	old, err := repository.GetVersion(context.Background(), scope, "version-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReplaceCompromisedWithContext(context.Background(), scope, "version-2", secret.OperationContext{AuditID: "compromise-audit", ActorID: "incident"}, []byte("new-upstream-value")); err != nil {
		t.Fatal(err)
	}
	var state, action, oldID, actor, result string
	var ciphertext []byte
	if err = repository.pool.QueryRow(context.Background(), `SELECT state FROM secret_versions WHERE id='version-1'`).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("old state=%s err=%v", state, err)
	}
	if err = repository.pool.QueryRow(context.Background(), `SELECT ciphertext FROM secret_versions WHERE id='version-2'`).Scan(&ciphertext); err != nil || bytes.Equal(ciphertext, old.Envelope.Ciphertext) {
		t.Fatalf("fresh ciphertext err=%v", err)
	}
	if err = repository.pool.QueryRow(context.Background(), `SELECT action,old_version_id,actor_id,result FROM secret_audit_events WHERE id='compromise-audit'`).Scan(&action, &oldID, &actor, &result); err != nil || action != "replaced_compromised" || oldID != "version-1" || actor != "incident" || result != "success" {
		t.Fatalf("audit=%q %q %q %q err=%v", action, oldID, actor, result, err)
	}
	freshEnvelope, err := crypto.Encrypt(secretStep2Ring(t, "kek-new"), repository, crypto.Scope{OwnerID: scope.OwnerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, VariableID: scope.SecretID, SecretVersionID: old.ID, Purpose: scope.Purpose, DeliveryScope: scope.DeliveryScope}, []byte("different"))
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.Rewrap(context.Background(), scope, "version-1", old.EnvelopeRevision, freshEnvelope); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("compromise path allowed rewrap=%v", err)
	}
	var kekID string
	var wrapped []byte
	if err = repository.pool.QueryRow(context.Background(), `SELECT kek_id,wrapped_dek FROM secret_versions WHERE id='version-1'`).Scan(&kekID, &wrapped); err != nil || kekID != old.Envelope.KEKID || !bytes.Equal(wrapped, old.Envelope.WrappedDEK) {
		t.Fatalf("revoked rewrap mutated id=%s err=%v", kekID, err)
	}
}

func TestSecretKeysetListAndCountAreStable(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	for _, id := range []string{"a", "c", "b"} {
		secretStep2Create(t, service, secretStep2Scope("keyset-"+id), "version-"+id, "audit-"+id)
	}
	count, err := repository.CountVersionsByKEK(context.Background(), "kek-old")
	if err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	first, err := repository.ListVersionsByKEK(context.Background(), "kek-old", "", 2)
	if err != nil || len(first) != 2 || first[0].ID != "version-a" || first[1].ID != "version-b" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := repository.ListVersionsByKEK(context.Background(), "kek-old", first[1].ID, 2)
	if err != nil || len(second) != 1 || second[0].ID != "version-c" {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if _, err = repository.ListVersionsByKEK(context.Background(), "kek-old", "", 0); !errors.Is(err, secret.ErrInvalidInput) {
		t.Fatalf("bad limit=%v", err)
	}
}

func TestSecretStep2PostEncryptionWriteFailureRetainsWrapReservation(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	if _, err := repository.pool.Exec(context.Background(), `ALTER TABLE secret_audit_events ADD CONSTRAINT reject_step2_create_audit CHECK (id <> 'failed-create-audit')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repository.pool.Exec(context.Background(), `ALTER TABLE secret_audit_events DROP CONSTRAINT IF EXISTS reject_step2_create_audit`)
	})
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	if _, err := service.CreateWithContext(context.Background(), secretStep2Scope("ledger-failure"), "version-1", secret.OperationContext{AuditID: "failed-create-audit", ActorID: "actor"}, []byte("value")); !errors.Is(err, secret.ErrCorrupt) {
		t.Fatalf("create after encryption audit failure = %v", err)
	}
	var reservations int
	if err := repository.pool.QueryRow(context.Background(), `SELECT reserved_invocations FROM secret_wrap_invocations WHERE kek_id='kek-old'`).Scan(&reservations); err != nil || reservations != 1 {
		t.Fatalf("durable reservation = %d, %v", reservations, err)
	}
}

func TestSecretStep2DatabaseIntegrityRejectsTampering(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	ctx := context.Background()
	first := secretMutation("integrity-a", "integrity-version-a", "integrity-audit-a")
	second := secretMutation("integrity-b", "integrity-version-b", "integrity-audit-b")
	if err := repository.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"fabricated old lineage": `INSERT INTO secret_audit_events(id,owner_id,secret_id,secret_version_id,action,actor_id,service_id,result,old_version_id,kek_id,project_id,environment_id) VALUES('bad-lineage','owner','integrity-a','integrity-version-a','replaced','actor','','success','integrity-version-b','kek-1','project','environment')`,
		"head scope mutation":    `UPDATE secret_heads SET project_id='other-project' WHERE id='integrity-a'`,
		"invalid revision":       `UPDATE secret_heads SET revision=revision+1 WHERE id='integrity-a'`,
		"cross-secret head link": `UPDATE secret_heads SET current_version_id='integrity-version-b',revision=revision+1 WHERE id='integrity-a'`,
		"audit update":           `UPDATE secret_audit_events SET action='revoked' WHERE id='integrity-audit-a'`,
		"audit delete":           `DELETE FROM secret_audit_events WHERE id='integrity-audit-a'`,
	} {
		if _, err := repository.pool.Exec(ctx, query); err == nil {
			t.Fatalf("database accepted %s", name)
		}
	}
}

func TestSecretStep2PersistedVersionHasCreationTime(t *testing.T) {
	repository, _ := freshSecretRepository(t)
	scope := secretStep2Scope("created-at")
	service := secret.NewService(repository, secretStep2Ring(t, "kek-old"), secretStep2Authorizer{})
	secretStep2Create(t, service, scope, "version-1", "create-audit")
	persisted, err := repository.GetVersion(context.Background(), scope, "version-1")
	if err != nil || persisted.CreatedAt.IsZero() {
		t.Fatalf("persisted created_at = %v, err=%v", persisted.CreatedAt, err)
	}
}
