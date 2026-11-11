//go:build integration

package agenthttp

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autodeploy/internal/server"
)

// TestAgentHTTPLogsAppendReplayAndFence drives the real handler and the real
// DeploymentLogRepository on PostgreSQL 18: append (with the L5 sanitizer
// applied), idempotent replay, wrong fence, and unauthenticated access.
func TestAgentHTTPLogsAppendReplayAndFence(t *testing.T) {
	f := newAgentHTTPFixture(t)
	srv := httptest.NewServer(withTLS(newTestHandler(f.pool)))
	defer srv.Close()

	tok, err := server.NewCredentialToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := server.CredentialTokenDigest(tok)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := f.issueEnrollment(t)
	if resp, body := postJSON(t, srv, pathEnroll, "", "", enrollRequest{
		ServerID: f.serverID, EnrollmentToken: string(enrollment), CredentialDigest: hexOf(digest),
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll: %d %v", resp.StatusCode, body)
	}
	resp, body := postJSON(t, srv, pathClaim, tok, f.serverID, claimRequest{Limit: 1, ExecutorAvailable: true})
	leases, _ := body["leases"].([]any)
	if resp.StatusCode != http.StatusOK || len(leases) != 1 {
		t.Fatalf("claim: %d %v", resp.StatusCode, body)
	}
	lease := leases[0].(map[string]any)
	attemptID, fence := int64(lease["attempt_id"].(float64)), int64(lease["fence"].(float64))

	secret := "ghp_" + strings.Repeat("Z", 36)
	req := logsRequest{AttemptID: attemptID, Fence: fence, WriterEpoch: 1, Chunks: []logChunkRequest{
		{Stream: "build", Seq: 1, Body: "step 1 nul\x00 " + secret},
		{Stream: "phase", Seq: 1, Body: "building"},
	}}
	resp, body = postJSON(t, srv, pathLogs, tok, f.serverID, req)
	if resp.StatusCode != http.StatusOK || body["accepted"] != float64(2) {
		t.Fatalf("append: %d %v", resp.StatusCode, body)
	}
	var stored string
	if err := f.pool.QueryRow(context.Background(), `SELECT body FROM deployment_log_chunks WHERE attempt_id=$1 AND stream='build' AND seq=1`, attemptID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "step 1 nul [REDACTED]" {
		t.Fatalf("stored body %q", stored)
	}

	// Replay is idempotent: duplicates counted, nothing added.
	resp, body = postJSON(t, srv, pathLogs, tok, f.serverID, req)
	if resp.StatusCode != http.StatusOK || body["accepted"] != float64(0) || body["duplicates"] != float64(2) {
		t.Fatalf("replay: %d %v", resp.StatusCode, body)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM deployment_log_chunks WHERE attempt_id=$1`, attemptID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows=%d err=%v", n, err)
	}

	// Wrong fence: generic 401, nothing stored.
	bad := req
	bad.Fence = fence + 1
	bad.Chunks = []logChunkRequest{{Stream: "build", Seq: 9, Body: "x"}}
	resp, body = postJSON(t, srv, pathLogs, tok, f.serverID, bad)
	if resp.StatusCode != http.StatusUnauthorized || body["error"] != "unauthorized" {
		t.Fatalf("wrong fence: %d %v", resp.StatusCode, body)
	}
	// No credential: same generic 401.
	if resp, _ := postJSON(t, srv, pathLogs, "", "", req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM deployment_log_chunks WHERE attempt_id=$1`, attemptID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows after rejects=%d err=%v", n, err)
	}
}
