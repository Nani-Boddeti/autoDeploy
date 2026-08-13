package postgres

import (
	"errors"
	"testing"

	"autodeploy/internal/crypto"
	"autodeploy/internal/secret"
)

func qaMutation() secret.Mutation {
	scope := secret.Scope{
		OwnerID: "owner", ProjectID: "project", EnvironmentID: "environment",
		SecretID: "secret", Purpose: "runtime", DeliveryScope: "runtime",
	}
	return secret.Mutation{
		Version: secret.Version{
			ID: "version", Scope: scope, Number: 1, State: secret.StateActive,
			Envelope: crypto.Envelope{
				Suite: crypto.SuiteAES256GCMRandomNonceV1, AADVersion: crypto.AADFormatV1,
				KEKID: "kek-1", Ciphertext: make([]byte, 28), WrappedDEK: make([]byte, 60),
			},
		},
		Audit: secret.Audit{
			ID: "audit", OwnerID: scope.OwnerID, SecretID: scope.SecretID,
			VersionID: "version", Action: "created", ActorID: "actor", Result: "success", KEKID: "kek-1",
		},
	}
}

func TestQACreateMutationRequiresActiveStateAndCreatedAudit(t *testing.T) {
	for name, mutate := range map[string]func(*secret.Mutation){
		"state": func(m *secret.Mutation) { m.Version.State = secret.StateRevoked },
		"audit": func(m *secret.Mutation) { m.Audit.Action = "replaced" },
	} {
		t.Run(name, func(t *testing.T) {
			mutation := qaMutation()
			mutate(&mutation)
			if err := validSecretMutation(mutation, 1); !errors.Is(err, secret.ErrInvalidInput) {
				t.Fatalf("Create mutation validation error = %v, want %v", err, secret.ErrInvalidInput)
			}
		})
	}
}

func TestQAReplaceMutationRequiresActiveStateAndReplacedAudit(t *testing.T) {
	for name, mutate := range map[string]func(*secret.Mutation){
		"state": func(m *secret.Mutation) { m.Version.State = secret.StateSuperseded },
		"audit": func(m *secret.Mutation) { m.Audit.Action = "created" },
	} {
		t.Run(name, func(t *testing.T) {
			mutation := qaMutation()
			mutation.Version.Number = 2
			mutation.Audit.Action = "replaced"
			mutate(&mutation)
			if err := validSecretMutation(mutation, 0); !errors.Is(err, secret.ErrInvalidInput) {
				t.Fatalf("Replace mutation validation error = %v, want %v", err, secret.ErrInvalidInput)
			}
		})
	}
}

func TestInitialSecretHeadAdvanceRequiresExactlyOneRow(t *testing.T) {
	if err := requireOneSecretHeadAdvance(0); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("zero-row initial head advance = %v, want revision conflict", err)
	}
	if err := requireOneSecretHeadAdvance(2); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("multi-row initial head advance = %v, want revision conflict", err)
	}
	if err := requireOneSecretHeadAdvance(1); err != nil {
		t.Fatalf("single-row initial head advance = %v", err)
	}
}
