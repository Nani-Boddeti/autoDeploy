package secret

import (
	"context"
	"errors"
	"testing"
)

func setAuthorizerScope(environment, secretID string) Scope {
	return Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: environment, SecretID: secretID, Purpose: "runtime", DeliveryScope: "runtime"}
}

func TestSetAuthorizerAdmitsOnlyItsOwnKeys(t *testing.T) {
	allowed := AuthorizationKey{Scope: setAuthorizerScope("env", "head1"), VersionID: "v1"}
	authorizer := NewSetAuthorizer([]AuthorizationKey{allowed})
	ctx := context.Background()

	if err := authorizer.AuthorizeSecret(ctx, AuthorizationRequest{Scope: allowed.Scope, VersionID: "v1"}); err != nil {
		t.Fatalf("authorized key refused: %v", err)
	}

	// Every field is part of the identity: a near-miss must not be admitted.
	for name, request := range map[string]AuthorizationRequest{
		"other version":     {Scope: allowed.Scope, VersionID: "v2"},
		"other environment": {Scope: setAuthorizerScope("other-env", "head1"), VersionID: "v1"},
		"other secret":      {Scope: setAuthorizerScope("env", "head2"), VersionID: "v1"},
		"other project": {Scope: Scope{OwnerID: "owner", ProjectID: "other", EnvironmentID: "env",
			SecretID: "head1", Purpose: "runtime", DeliveryScope: "runtime"}, VersionID: "v1"},
		"other owner": {Scope: Scope{OwnerID: "intruder", ProjectID: "project", EnvironmentID: "env",
			SecretID: "head1", Purpose: "runtime", DeliveryScope: "runtime"}, VersionID: "v1"},
		"other purpose": {Scope: Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "env",
			SecretID: "head1", Purpose: "build", DeliveryScope: "runtime"}, VersionID: "v1"},
		"other delivery scope": {Scope: Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "env",
			SecretID: "head1", Purpose: "runtime", DeliveryScope: "build"}, VersionID: "v1"},
	} {
		if err := authorizer.AuthorizeSecret(ctx, request); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: err=%v, want ErrUnauthorized", name, err)
		}
	}
}

// TestSetAuthorizerAdmitsNothingWhenEmpty is the fail-closed default: a
// deployment with no frozen secrets must decrypt nothing, never everything.
func TestSetAuthorizerAdmitsNothingWhenEmpty(t *testing.T) {
	ctx := context.Background()
	for name, authorizer := range map[string]*SetAuthorizer{
		"nil":   nil,
		"empty": NewSetAuthorizer(nil),
		"zero":  {},
	} {
		if err := authorizer.AuthorizeSecret(ctx, AuthorizationRequest{Scope: setAuthorizerScope("env", "head1"), VersionID: "v1"}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s authorizer admitted a request: %v", name, err)
		}
	}
}

// TestSetAuthorizerIgnoresFrozenFlag proves a caller cannot reach a different
// secret by flipping Frozen: whether a superseded version may be read is
// Service's rule, not part of which secret is addressable.
func TestSetAuthorizerIgnoresFrozenFlag(t *testing.T) {
	allowed := AuthorizationKey{Scope: setAuthorizerScope("env", "head1"), VersionID: "v1"}
	authorizer := NewSetAuthorizer([]AuthorizationKey{allowed})
	ctx := context.Background()
	for _, frozen := range []bool{false, true} {
		if err := authorizer.AuthorizeSecret(ctx, AuthorizationRequest{Scope: allowed.Scope, VersionID: "v1", Frozen: frozen}); err != nil {
			t.Fatalf("frozen=%v refused: %v", frozen, err)
		}
	}
	if err := authorizer.AuthorizeSecret(ctx, AuthorizationRequest{Scope: allowed.Scope, VersionID: "v2", Frozen: true}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("Frozen must not widen which secret is addressable")
	}
}
