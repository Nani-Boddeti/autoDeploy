package agentclient

import (
	"context"
	"crypto/rand"
	"errors"
	"io"

	"autodeploy/internal/server"
)

// EnrollInput bundles what `agent enroll` needs beyond an already-parsed
// enrollment token (see ReadEnrollmentToken).
type EnrollInput struct {
	BaseURL         string
	ServerID        string
	EnrollmentToken server.EnrollmentToken
	StateDir        string
}

// Enroll implements Decision 2/Decision 4 (tasks/todo.md): it generates the
// durable bearer credential locally from randomness -- never received from
// or invented by the control plane -- writes it to <StateDir>/credential
// before any network call, and presents only that credential's digest, plus
// the one-time enrollment token an administrator already issued out of
// band, to POST /agent/v1/enroll. See writeCredentialAtomic's doc comment
// for the exact crash/idempotency behavior if this process dies between the
// write and this function returning.
//
// On any failure of the network call itself, the just-written credential is
// removed (removeUnusedCredential) so a subsequent enroll in this same
// state directory is not blocked by a credential the control plane never
// accepted.
func Enroll(ctx context.Context, in EnrollInput, randomness io.Reader) (int, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	if in.StateDir == "" {
		return 0, errors.New("state directory required")
	}
	token, err := server.NewCredentialToken(randomness)
	if err != nil {
		return 0, err
	}
	if err := writeCredentialAtomic(in.StateDir, token); err != nil {
		return 0, err
	}
	digest, err := server.CredentialTokenDigest(token)
	if err != nil {
		removeUnusedCredential(in.StateDir)
		return 0, err
	}
	client, err := NewClient(in.BaseURL, in.ServerID, "", nil, nil)
	if err != nil {
		removeUnusedCredential(in.StateDir)
		return 0, err
	}
	version, err := client.Enroll(ctx, in.EnrollmentToken, digest)
	if err != nil {
		removeUnusedCredential(in.StateDir)
		return 0, err
	}
	return version, nil
}
