package agentclient

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"autodeploy/internal/server"
)

// newTLSFixture starts an httptest.NewTLSServer running handler and returns
// an *http.Client whose RootCAs trust that server's certificate, matching
// the deliverable's "httptest TLS server with its client cert pool
// injected" requirement.
func newTLSFixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv, srv.Client()
}

func newEnrollmentToken(t *testing.T) server.EnrollmentToken {
	t.Helper()
	tok, err := server.NewEnrollmentToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// enrollWithHTTPClient runs exactly Enroll's write-credential /
// digest-only-POST / remove-on-failure sequence, but with an injected
// *http.Client so a test's httptest.NewTLSServer certificate is trusted
// (Enroll itself only ever builds a default client via NewClient(...,
// nil, nil)).
func enrollWithHTTPClient(t *testing.T, stateDir, baseURL string, enrollmentToken server.EnrollmentToken, httpClient *http.Client) (int, error) {
	t.Helper()
	token, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		return 0, err
	}
	if err := writeCredentialAtomic(stateDir, token); err != nil {
		return 0, err
	}
	digest, err := server.CredentialTokenDigest(token)
	if err != nil {
		removeUnusedCredential(stateDir)
		return 0, err
	}
	client, err := NewClient(baseURL, "server_test", "", httpClient, nil)
	if err != nil {
		removeUnusedCredential(stateDir)
		return 0, err
	}
	version, err := client.Enroll(context.Background(), enrollmentToken, digest)
	if err != nil {
		removeUnusedCredential(stateDir)
		return 0, err
	}
	return version, nil
}

func TestEnrollSendsOnlyDigestNoAuthorizationHeader(t *testing.T) {
	var gotAuthPresent bool
	var gotBody string
	srv, httpClient := newTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuthPresent = len(r.Header.Values("Authorization")) > 0
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"credential_version": 1})
	})

	dir := t.TempDir()
	version, err := enrollWithHTTPClient(t, dir, srv.URL, newEnrollmentToken(t), httpClient)
	if err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if gotAuthPresent {
		t.Fatal("enroll must never send an Authorization header")
	}

	credentialBytes, err := os.ReadFile(credentialPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	rawCredential := string(credentialBytes)
	if strings.Contains(gotBody, rawCredential) {
		t.Fatal("request body must never contain the raw generated credential")
	}
	if !strings.HasPrefix(rawCredential, "srv.v1.") {
		t.Fatalf("unexpected credential shape: %q", rawCredential)
	}
	if info, err := os.Stat(credentialPath(dir)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file mode: %v %v", info, err)
	}
}

func TestEnrollRefusesOverwriteExistingCredential(t *testing.T) {
	dir := t.TempDir()
	existing, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialAtomic(dir, existing); err != nil {
		t.Fatal(err)
	}
	srv, httpClient := newTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("enroll must not contact the network when the credential file already exists")
	})
	if _, err := enrollWithHTTPClient(t, dir, srv.URL, newEnrollmentToken(t), httpClient); err == nil {
		t.Fatal("expected an error refusing to overwrite the existing credential")
	}
	contents, err := os.ReadFile(credentialPath(dir))
	if err != nil || string(contents) != existing.String() {
		t.Fatalf("existing credential must be untouched: %v %q", err, contents)
	}
}

func TestEnrollRemovesCredentialOnRejection(t *testing.T) {
	var calls int32
	srv, httpClient := newTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request"})
	})
	dir := t.TempDir()
	if _, err := enrollWithHTTPClient(t, dir, srv.URL, newEnrollmentToken(t), httpClient); err == nil {
		t.Fatal("expected enroll to fail")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected exactly one enroll call, got %d", calls)
	}
	if _, err := os.Stat(credentialPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("credential file must be removed after a rejected enroll, stat err=%v", err)
	}
}

func TestEnrollRejectsHTTPBaseURL(t *testing.T) {
	dir := t.TempDir()
	if _, err := Enroll(context.Background(), EnrollInput{
		BaseURL: "http://example.invalid", ServerID: "server_test",
		EnrollmentToken: newEnrollmentToken(t), StateDir: dir,
	}, rand.Reader); err == nil {
		t.Fatal("expected http:// base URL to be rejected")
	}
	if _, err := os.Stat(credentialPath(dir)); !os.IsNotExist(err) {
		t.Fatal("no credential should be written when the base URL is rejected before any network call")
	}
}

func TestEnrollRequiresStateDir(t *testing.T) {
	if _, err := Enroll(context.Background(), EnrollInput{
		BaseURL: "https://example.invalid", ServerID: "server_test",
		EnrollmentToken: newEnrollmentToken(t), StateDir: "",
	}, rand.Reader); err == nil {
		t.Fatal("expected an error for an empty state directory")
	}
}
