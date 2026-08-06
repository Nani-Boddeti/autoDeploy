package postgres

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"autodeploy/internal/auth"
)

func TestAuthInputValidationRejectsUnsafeValuesBeforeDatabaseUse(t *testing.T) {
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	verifier, err := auth.HashPassword(strings.Repeat("x", 15), policy, bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	valid := BootstrapInput{OwnerID: "owner-1", UserID: "user-1", Username: "administrator", AuditID: "audit-1", Verifier: verifier, Policy: policy}
	if err := validateBootstrap(valid); err != nil {
		t.Fatalf("valid bootstrap rejected: %v", err)
	}
	for _, input := range []BootstrapInput{
		{OwnerID: "", UserID: valid.UserID, Username: valid.Username, AuditID: valid.AuditID, Verifier: verifier, Policy: policy},
		{OwnerID: valid.OwnerID, UserID: strings.Repeat("u", 129), Username: valid.Username, AuditID: valid.AuditID, Verifier: verifier, Policy: policy},
		{OwnerID: valid.OwnerID, UserID: valid.UserID, Username: "Admin", AuditID: valid.AuditID, Verifier: verifier, Policy: policy},
		{OwnerID: valid.OwnerID, UserID: valid.UserID, Username: valid.Username, AuditID: valid.AuditID, Verifier: auth.PasswordVerifier{PHC: "bad", PolicyRevision: 1}, Policy: policy},
		{OwnerID: valid.OwnerID, UserID: valid.UserID, Username: valid.Username, AuditID: valid.AuditID, Verifier: verifier, Policy: auth.Argon2Policy{}},
	} {
		if err := validateBootstrap(input); err == nil {
			t.Errorf("accepted invalid bootstrap: %+v", input)
		}
	}
	if err := userInputValid(LoginUser{ID: "user", OwnerID: "owner", AuthRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := userInputValid(LoginUser{ID: "", OwnerID: "owner", AuthRevision: 1}); err == nil {
		t.Fatal("accepted empty session user ID")
	}
	if err := validID(strings.Repeat("x", 129)); err == nil {
		t.Fatal("accepted oversized ID")
	}
	if !errors.Is(ErrUnauthenticated, ErrUnauthenticated) || ErrUnauthenticated.Error() == "" {
		t.Fatal("typed unauthenticated error is not stable")
	}
}

func TestThrottleInputValidation(t *testing.T) {
	valid := ThrottleIdentity{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{1}}
	if err := validateThrottleIdentities([]ThrottleIdentity{valid}, 10); err != nil {
		t.Fatal(err)
	}
	knownPair := valid
	knownPair.RecoveryUserID = "user-1"
	if err := validateThrottleIdentities([]ThrottleIdentity{knownPair}, 10); err != nil {
		t.Fatalf("known-user pair rejected: %v", err)
	}
	for _, identities := range [][]ThrottleIdentity{
		nil,
		{{Kind: "raw_ip", KeyVersion: "v1"}},
		{{Kind: ThrottleIP, KeyVersion: ""}},
		{{Kind: ThrottleIP, KeyVersion: "overflow"}},
		{{Kind: ThrottleIP, KeyVersion: "v 1"}},
		{{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{2}, Aliases: []ThrottleAlias{{KeyVersion: "overflow", Digest: [32]byte{3}}}}},
		{{Kind: ThrottleIP, KeyVersion: "v1", Digest: [32]byte{2}, RecoveryUserID: "user-1"}},
		{{Kind: ThrottlePair, KeyVersion: "v1", Digest: [32]byte{2}, RecoveryUserID: strings.Repeat("u", 129)}},
		{valid, valid},
		{{Kind: ThrottleUsername, KeyVersion: "v1", Aliases: make([]ThrottleAlias, maxThrottleAliases+1)}},
	} {
		if err := validateThrottleIdentities(identities, 10); err == nil {
			t.Errorf("accepted invalid throttle identities: %#v", identities)
		}
	}
}
