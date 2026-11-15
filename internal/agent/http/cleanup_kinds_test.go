package agenthttp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/store/postgres"
)

// kindsCleanupStore is a fakeCleanupStore that also serves the opt-in claim.
type kindsCleanupStore struct {
	fakeCleanupStore
	kindsCalls   int
	includeAsked bool
}

func (f *kindsCleanupStore) ClaimCleanupsWithKinds(_ context.Context, serverID string, _ [32]byte, limit int, include bool) ([]postgres.CleanupClaim, error) {
	f.kindsCalls++
	f.serverID, f.limit, f.includeAsked = serverID, limit, include
	return f.claims, f.claimErr
}

func mixedClaims() []postgres.CleanupClaim {
	return []postgres.CleanupClaim{
		{CleanupID: 5, ReleaseID: "rel_a", EnvironmentID: "env_1", RuntimeInstanceID: "42", ImageDigest: strings.Repeat("ab", 32), Kind: postgres.CleanupKindRetained},
		{CleanupID: 6, ReleaseID: "rel_f", EnvironmentID: "env_1", ImageDigest: "", Kind: postgres.CleanupKindFailedCandidate, RuntimeInstanceIDs: []string{"7", "9"}},
	}
}

func TestCleanupClaimWithoutKindsIsTheStep20EResponseByteForByte(t *testing.T) {
	want := fmt.Sprintf(`{"cleanups":[{"cleanup_id":5,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":%q}]}`, strings.Repeat("ab", 32))
	for name, body := range map[string]string{"absent": `{"limit":3}`, "empty": `{"limit":3,"kinds":[]}`, "null": `{"limit":3,"kinds":null}`} {
		fs := &kindsCleanupStore{fakeCleanupStore: fakeCleanupStore{claims: mixedClaims()[:1]}}
		code, _, got := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, body)
		if code != http.StatusOK || string(got) != want || fs.kindsCalls != 0 || fs.claimCalls != 1 {
			t.Errorf("%s: %d %s kindsCalls=%d claimCalls=%d", name, code, got, fs.kindsCalls, fs.claimCalls)
		}
	}
}

func TestCleanupClaimOptInAddsKindAndPerAttemptInstances(t *testing.T) {
	fs := &kindsCleanupStore{fakeCleanupStore: fakeCleanupStore{claims: mixedClaims()}}
	code, _, body := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":4,"kinds":["failed_candidate"]}`)
	if code != http.StatusOK || fs.kindsCalls != 1 || !fs.includeAsked || fs.limit != 4 || fs.serverID != "server_1" || fs.claimCalls != 0 {
		t.Fatalf("status=%d kindsCalls=%d include=%v limit=%d server=%q claimCalls=%d", code, fs.kindsCalls, fs.includeAsked, fs.limit, fs.serverID, fs.claimCalls)
	}
	want := fmt.Sprintf(`{"cleanups":[{"cleanup_id":5,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":%q,"kind":"retained"},`+
		`{"cleanup_id":6,"release_id":"rel_f","environment_id":"env_1","runtime_instance_id":"","image_digest":"","kind":"failed_candidate","runtime_instance_ids":["7","9"]}]}`, strings.Repeat("ab", 32))
	if string(body) != want {
		t.Fatalf("body = %s", body)
	}
	fs.claims = nil
	if _, _, body = postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":1,"kinds":["failed_candidate"]}`); string(body) != `{"cleanups":[]}` {
		t.Fatalf("empty body = %s", body)
	}
}

func TestCleanupClaimKindsRetainedOnlyDoesNotOptInToFailedCandidates(t *testing.T) {
	fs := &kindsCleanupStore{fakeCleanupStore: fakeCleanupStore{claims: mixedClaims()[:1]}}
	code, _, body := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":2,"kinds":["retained"]}`)
	if code != http.StatusOK || fs.kindsCalls != 0 || fs.claimCalls != 1 || !strings.Contains(string(body), `"kind":"retained"`) {
		t.Fatalf("%d kindsCalls=%d claimCalls=%d body=%s", code, fs.kindsCalls, fs.claimCalls, body)
	}
}

func TestCleanupClaimOptInAgainstAStoreWithoutKindsFallsBackToRetained(t *testing.T) {
	fs := &fakeCleanupStore{claims: mixedClaims()[:1]}
	code, _, body := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":2,"kinds":["failed_candidate"]}`)
	if code != http.StatusOK || fs.claimCalls != 1 || !strings.Contains(string(body), `"kind":"retained"`) {
		t.Fatalf("%d claimCalls=%d body=%s", code, fs.claimCalls, body)
	}
}

func TestCleanupClaimRejectsBadKinds(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":    `{"limit":1,"kinds":["everything"]}`,
		"empty name": `{"limit":1,"kinds":[""]}`,
		"too many":   `{"limit":1,"kinds":["retained","retained","retained","retained","retained"]}`,
		"string":     `{"limit":1,"kinds":"failed_candidate"}`,
		"number":     `{"limit":1,"kinds":[1]}`,
	} {
		fs := &kindsCleanupStore{}
		if code, msg, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, body); code != http.StatusBadRequest || msg != "invalid_request" || fs.kindsCalls != 0 || fs.claimCalls != 0 {
			t.Errorf("%s: %d %q kinds=%d claims=%d", name, code, msg, fs.kindsCalls, fs.claimCalls)
		}
	}
}

func TestCleanupClaimKindsStoreErrorsAreMapped(t *testing.T) {
	fs := &kindsCleanupStore{fakeCleanupStore: fakeCleanupStore{claimErr: postgres.ErrServerAuthorizationDenied}}
	if code, msg, _ := postCleanup(t, Handler{CleanupStore: fs}, pathCleanupClaim, `{"limit":1,"kinds":["failed_candidate"]}`); code != http.StatusUnauthorized || msg != "unauthorized" {
		t.Fatalf("%d %q", code, msg)
	}
}
