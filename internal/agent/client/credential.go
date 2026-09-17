package agentclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"autodeploy/internal/server"
)

const credentialFileName = "credential"

// ErrCredentialExists is returned when enroll would have to overwrite an
// existing credential file. Enroll never overwrites; see
// writeCredentialAtomic's doc comment for the operator recovery path.
var ErrCredentialExists = errors.New("agent credential already exists")

func credentialPath(stateDir string) string { return filepath.Join(stateDir, credentialFileName) }

// writeCredentialAtomic durably writes token to <stateDir>/credential with
// mode 0600: write a temp file in the same directory, fsync it, atomically
// link it into place only if no credential exists yet, remove the temp
// name, then fsync the directory so the new entry survives a crash.
//
// Linking (create-if-absent) rather than renaming is deliberate: a rename
// would silently clobber a credential a running `agent run` instance may
// already be using, and the deliverable requires enroll to refuse an
// overwrite outright.
//
// Crash/idempotency note: if this process crashes or is killed after this
// function returns successfully but before the enroll HTTP call completes
// (or before its response is observed), the credential file is left on
// disk. A later `agent enroll` against the same state directory then fails
// closed with ErrCredentialExists instead of silently overwriting it --
// this function cannot tell, from local state alone, whether the control
// plane already accepted that exact credential digest. An operator must
// resolve the ambiguity out of band: check whether the server shows an
// active credential for this server ID (the web /servers admin page). If it
// does, the leftover file is exactly the right credential and `agent run`
// can proceed with no re-enroll. If it does not, the enrollment token was
// very likely also consumed or has expired (it is single-use, short-lived);
// the operator must remove the stale local file and retry with a freshly
// issued enrollment token.
func writeCredentialAtomic(stateDir string, token server.CredentialToken) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("create agent state directory: %w", err)
	}
	path := credentialPath(stateDir)
	tmp, err := os.CreateTemp(stateDir, ".credential-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrCredentialExists
		}
		return err
	}
	removeTemp = false
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	d, err := os.Open(stateDir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// removeUnusedCredential deletes a credential this same process instance
// just wrote after the enroll HTTP call failed within this same run (not
// after a crash/restart, where writeCredentialAtomic's ambiguity note
// applies): the digest presented was never accepted by the control plane,
// so nothing durable references it and discarding it is always safe.
func removeUnusedCredential(stateDir string) {
	_ = os.Remove(credentialPath(stateDir))
}

// LoadCredential reads and strictly validates the durable credential file
// at <stateDir>/credential: it must be exactly the file this package writes
// (mode 0600, owned by the current effective user). Anything else -- a
// world/group-readable file, a file owned by another user, a symlink, or a
// corrupt/truncated token -- fails closed rather than run with a credential
// of uncertain provenance.
func LoadCredential(stateDir string) (server.CredentialToken, error) {
	contents, err := readProtectedFile(credentialPath(stateDir))
	if err != nil {
		return "", err
	}
	token, err := server.ParseCredentialToken(string(contents))
	if err != nil {
		return "", ErrProtectedFileRejected
	}
	return token, nil
}
