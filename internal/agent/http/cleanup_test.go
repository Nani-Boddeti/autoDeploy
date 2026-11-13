package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

type fakeCleanupStore struct {
	claimErr, resultErr error
	claims              []postgres.CleanupClaim
	serverID            string
	limit               int
	claimCalls          int
	resultCalls         int
	id                  int64
	result, class, summ string
	kept                bool
}

func (f *fakeCleanupStore) ClaimCleanups(_ context.Context, serverID string, _ [32]byte, limit int) ([]postgres.CleanupClaim, error) {
	f.claimCalls++
	f.serverID, f.limit = serverID, limit
	return f.claims, f.claimErr
}

func (f *fakeCleanupStore) RecordCleanupResult(_ context.Context, serverID string, _ [32]byte, id int64, result, class string, kept bool, summary string) error {
	f.resultCalls++
	f.serverID, f.id, f.result, f.class, f.kept, f.summ = serverID, id, result, class, kept, summary
	return f.resultErr
}

func postCleanup(t *testing.T, h Handler, path, body string) (int, string, []byte) {
	t.Helper()
	tok, _ := server.NewCredentialToken(rand.Reader)
	w := do(h, newRequestTLS(newRequest(t, path, body, bearerPrefix+string(tok), "server_1")))
	var got errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got.Error, w.Body.Bytes()
}

func TestCleanupClaimReturnsServerDerivedItemsForTheCredentialsServer(t *testing.T) {
	fs := &fakeCleanupStore{claims: []postgres.CleanupClaim{{CleanupID: 5, ReleaseID: "rel_a", EnvironmentID: "env_1", RuntimeInstanceID: "42", ImageDigest: strings.Repeat("ab", 32)}}}
	// A body naming another server is an unknown field: the server is never a request field.
	if code, msg, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":3,"server_id":"server_2"}`); code != http.StatusBadRequest || msg != "invalid_request" || fs.claimCalls != 0 {
		t.Fatalf("server_id in body: %d %q calls=%d", code, msg, fs.claimCalls)
	}
	code, _, body := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":3}`)
	if code != http.StatusOK || fs.serverID != "server_1" || fs.limit != 3 {
		t.Fatalf("status=%d server=%q limit=%d", code, fs.serverID, fs.limit)
	}
	want := fmt.Sprintf(`{"cleanups":[{"cleanup_id":5,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":%q}]}`, strings.Repeat("ab", 32))
	if string(body) != want {
		t.Fatalf("body = %s", body)
	}
	fs.claims = nil
	if _, _, body = postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":1}`); string(body) != `{"cleanups":[]}` {
		t.Fatalf("empty body = %s", body)
	}
}

func TestCleanupClaimValidationAndErrors(t *testing.T) {
	for name, body := range map[string]string{"zero": `{"limit":0}`, "negative": `{"limit":-1}`, "too many": `{"limit":17}`, "missing": `{}`, "string": `{"limit":"3"}`} {
		fs := &fakeCleanupStore{}
		if code, msg, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, body); code != http.StatusBadRequest || msg != "invalid_request" || fs.claimCalls != 0 {
			t.Errorf("%s: %d %q calls=%d", name, code, msg, fs.claimCalls)
		}
	}
	if code, _, _ := postCleanup(t, Handler{}, pathCleanupClaim, `{"limit":1}`); code != http.StatusServiceUnavailable {
		t.Fatalf("nil store: %d", code)
	}
	w := do(Handler{CleanupStore: &fakeCleanupStore{}}, newRequestTLS(newRequest(t, pathCleanupClaim, `{"limit":1}`, "Bearer nope", "server_1")))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad credential: %d", w.Code)
	}
	for name, tc := range map[string]struct {
		err  error
		code int
		msg  string
	}{
		"denied":    {postgres.ErrServerAuthorizationDenied, http.StatusUnauthorized, "unauthorized"},
		"transient": {context.DeadlineExceeded, http.StatusServiceUnavailable, "service_unavailable"},
	} {
		if code, msg, _ := postCleanup(t, Handler{CleanupStore: &fakeCleanupStore{claimErr: tc.err}}, pathCleanupClaim, `{"limit":1}`); code != tc.code || msg != tc.msg {
			t.Errorf("%s: %d %q", name, code, msg)
		}
	}
}

func cleanupResultBody(result, class string, kept bool, summary string) string {
	return fmt.Sprintf(`{"cleanup_id":5,"result":%q,"error_class":%q,"image_kept":%t,"summary":%q}`, result, class, kept, summary)
}

func TestCleanupResultAcceptsValidReports(t *testing.T) {
	for name, body := range map[string]string{
		"removed":      cleanupResultBody("removed", "", false, "ok"),
		"removed kept": cleanupResultBody("removed", "", true, "ok"),
		"failed":       cleanupResultBody("failed", "engine", false, "x"),
		"skipped":      cleanupResultBody("skipped", "identity", false, "x"),
	} {
		fs := &fakeCleanupStore{}
		if code, _, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupResult, body); code != http.StatusOK || fs.resultCalls != 1 || fs.id != 5 || fs.serverID != "server_1" {
			t.Errorf("%s: status=%d calls=%d %+v", name, code, fs.resultCalls, fs)
		}
	}
}

func TestCleanupResultRejectsMalformedAndMapsErrors(t *testing.T) {
	for name, body := range map[string]string{
		"unknown result":      cleanupResultBody("gone", "", false, ""),
		"removed with class":  cleanupResultBody("removed", "engine", false, ""),
		"failed no class":     cleanupResultBody("failed", "", false, ""),
		"unknown class":       cleanupResultBody("failed", "boom", false, ""),
		"failed image kept":   cleanupResultBody("failed", "engine", true, ""),
		"zero id":             `{"cleanup_id":0,"result":"removed","error_class":"","image_kept":false,"summary":""}`,
		"oversize":            cleanupResultBody("failed", "engine", false, strings.Repeat("a", 2049)),
		"unknown field":       `{"cleanup_id":5,"result":"removed","error_class":"","image_kept":false,"summary":"","server_id":"x"}`,
		"release id supplied": `{"cleanup_id":5,"release_id":"r","result":"removed","error_class":"","image_kept":false,"summary":""}`,
	} {
		fs := &fakeCleanupStore{}
		if code, msg, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupResult, body); code != http.StatusBadRequest || msg != "invalid_request" || fs.resultCalls != 0 {
			t.Errorf("%s: %d %q calls=%d", name, code, msg, fs.resultCalls)
		}
	}
	for name, tc := range map[string]struct {
		err  error
		code int
		msg  string
	}{
		"conflict":  {postgres.ErrAgentLeaseIntegrityConflict, http.StatusConflict, "integrity_conflict"},
		"denied":    {postgres.ErrServerAuthorizationDenied, http.StatusUnauthorized, "unauthorized"},
		"corrupt":   {postgres.ErrAgentLeaseCorruptState, http.StatusConflict, "corrupt_state"},
		"transient": {context.DeadlineExceeded, http.StatusServiceUnavailable, "service_unavailable"},
	} {
		fs := &fakeCleanupStore{resultErr: tc.err}
		if code, msg, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupResult, cleanupResultBody("removed", "", false, "")); code != tc.code || msg != tc.msg {
			t.Errorf("%s: %d %q", name, code, msg)
		}
	}
	if code, _, _ := postCleanup(t, Handler{}, pathCleanupResult, cleanupResultBody("removed", "", false, "")); code != http.StatusServiceUnavailable {
		t.Fatalf("nil store: %d", code)
	}
}
