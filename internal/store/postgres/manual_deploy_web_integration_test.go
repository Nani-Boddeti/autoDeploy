//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
	"autodeploy/internal/web"
)

const webManualSHA = "0123456789abcdef0123456789abcdef01234567"

type fakeCommitResolver struct {
	gotRef string
	err    error
}

func (f *fakeCommitResolver) ResolveBranchHead(_ context.Context, _, _ int64, ref string) (string, error) {
	f.gotRef = ref
	return webManualSHA, f.err
}

func (f *fakeCommitResolver) VerifyCommitOnRef(_ context.Context, _, _ int64, ref, _ string) error {
	f.gotRef = ref
	return f.err
}

func TestManualDeployWebEndToEnd(t *testing.T) {
	fx := postgres.NewManualDeployFixtureForTest(t, 140)
	resolver := &fakeCommitResolver{}
	policy := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	dummy, err := auth.BuildDummyPasswordVerifier(policy)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.CSRFKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)}}
	throttle := auth.UsernameThrottleKeyRing{ActiveVersion: "v1", Keys: map[string][]byte{"v1": bytes.Repeat([]byte{2}, 32)}}
	h, err := web.New(resolveSessionRepository{principal: fx.Principal}, web.Config{
		PublicOrigin: "https://admin.example.test", CSRFKeys: keys, UsernameThrottleKeys: throttle, AuthThrottleMaxRows: 8,
		PasswordPolicy: policy, DummyPassword: dummy,
		OperationsReader: fx.Ops, ManualDeploy: fx.Jobs, CommitResolver: resolver,
		Now: func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }, Random: bytes.NewReader(bytes.Repeat([]byte{3}, 512)),
	})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target string, form url.Values) *httptest.ResponseRecorder {
		var r *http.Request
		if form != nil {
			r = httptest.NewRequest(method, "https://admin.example.test"+target, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://admin.example.test")
		} else {
			r = httptest.NewRequest(method, "https://admin.example.test"+target, nil)
		}
		r.AddCookie(&http.Cookie{Name: "__Host-ad_session", Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	csrf, err := auth.NewSessionCSRF(keys, fx.Principal.SessionID, "https://admin.example.test")
	if err != nil {
		t.Fatal(err)
	}
	form := func(nonce string) url.Values {
		return url.Values{"csrf": {csrf}, "project_id": {fx.ProjectID}, "request_nonce": {nonce}, "mode": {"head"}, "commit_sha": {""}}
	}
	if page := do(http.MethodGet, "/projects", nil).Body.String(); !strings.Contains(page, `action="/deployments/manual"`) {
		t.Fatal("projects page has no Deploy form for the bound project")
	}
	first := do(http.MethodPost, "/deployments/manual", form("nonce-0123456789abcdef"))
	loc := first.Header().Get("Location")
	if first.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/deployments/detail?id=mnd_") || resolver.gotRef != "refs/heads/main" {
		t.Fatalf("head deploy = %d %q ref=%q", first.Code, loc, resolver.gotRef)
	}
	if detail := do(http.MethodGet, loc, nil); detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), webManualSHA[:12]) {
		t.Fatalf("detail = %d", detail.Code)
	}
	if replay := do(http.MethodPost, "/deployments/manual", form("nonce-0123456789abcdef")); replay.Code != http.StatusSeeOther || replay.Header().Get("Location") != loc {
		t.Fatalf("replay = %d %q", replay.Code, replay.Header().Get("Location"))
	}
	if inflight := do(http.MethodPost, "/deployments/manual", form("nonce-fedcba9876543210")); inflight.Code != http.StatusConflict {
		t.Fatalf("second nonce, same commit = %d %q", inflight.Code, inflight.Body.String())
	}
	resolver.err = githubapp.ErrCommitNotOnRef
	sha := form("nonce-aaaaaaaaaaaaaaaa")
	sha.Set("mode", "sha")
	sha.Set("commit_sha", "89abcdef0123456789abcdef0123456789abcdef")
	if rr := do(http.MethodPost, "/deployments/manual", sha); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("not on ref = %d", rr.Code)
	}
	resolver.err = nil
	unknown := form("nonce-bbbbbbbbbbbbbbbb")
	unknown.Set("project_id", "no-such-project")
	if rr := do(http.MethodPost, "/deployments/manual", unknown); rr.Code != http.StatusForbidden {
		t.Fatalf("unbound project = %d", rr.Code)
	}
}
