package secret

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/crypto"
)

// A service assembled without its durable repository must fail closed rather
// than panic after authorization succeeds.
func TestQAMaterializeWithoutRepositoryFailsClosed(t *testing.T) {
	service := NewService(nil, testProvider{key: []byte("01234567890123456789012345678901")}, &testAuthorizer{})

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Materialize panicked instead of failing closed: %v", recovered)
		}
	}()
	if _, err := service.MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: testScope(), VersionID: "version", ActorID: "actor"}, "audit"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("MaterializeAuthorized error = %v, want %v", err, ErrInvalidInput)
	}
}

// Authorization is for the caller's requested immutable scope. A repository
// result must not be allowed to substitute another scope before decryption.
func TestQAMaterializeRejectsRepositoryScopeSubstitution(t *testing.T) {
	requested := testScope()
	foreign := requested
	foreign.OwnerID = "other-owner"
	provider := testProvider{key: []byte("01234567890123456789012345678901")}
	seed := &testRepo{}
	envelope, err := crypto.Encrypt(provider, seed, crypto.Scope{
		OwnerID: foreign.OwnerID, ProjectID: foreign.ProjectID, EnvironmentID: foreign.EnvironmentID,
		VariableID: foreign.SecretID, SecretVersionID: "foreign-version", Purpose: foreign.Purpose,
		DeliveryScope: foreign.DeliveryScope,
	}, []byte("foreign-value"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &testRepo{version: Version{
		ID: "foreign-version", Scope: foreign, Number: 1, State: StateActive, Envelope: envelope,
	}}

	plaintext, err := NewService(repository, provider, &testAuthorizer{}).
		MaterializeAuthorized(context.Background(), AuthorizationRequest{Scope: requested, VersionID: "foreign-version", ActorID: "actor"}, "audit")
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("scope substitution returned plaintext %q with error %v", plaintext, err)
	}
}
