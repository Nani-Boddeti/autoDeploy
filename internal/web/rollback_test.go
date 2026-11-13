package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/store/postgres"
)

type rollbackStoreStub struct {
	eligible     bool
	eligibleErr  error
	enqueueErr   error
	eligibleCall int
	enqueueCalls int
	gotOwner     string
	gotRelease   string
	gotNonce     string
}

func (s *rollbackStoreStub) RollbackEligible(_ context.Context, _ auth.Principal, releaseID string) (bool, error) {
	s.eligibleCall++
	s.gotRelease = releaseID
	return s.eligible, s.eligibleErr
}

func (s *rollbackStoreStub) EnqueueRollback(_ context.Context, p auth.Principal, releaseID, nonce string) (postgres.RollbackResult, error) {
	s.enqueueCalls++
	s.gotOwner, s.gotRelease, s.gotNonce = p.OwnerID, releaseID, nonce
	if s.enqueueErr != nil {
		return postgres.RollbackResult{}, s.enqueueErr
	}
	return postgres.RollbackResult{DeploymentID: deployment.ID("rbd_abc"), JobID: 8}, nil
}

func rollbackHandler(t *testing.T) (*Handler, *rollbackStoreStub, *operationsStub, auth.Principal) {
	t.Helper()
	h, ops, p := operationsHandler(t)
	st := &rollbackStoreStub{eligible: true}
	h.config.Rollback = st
	return h, st, ops, p
}

func rollbackForm(t *testing.T, h *Handler, p auth.Principal) url.Values {
	t.Helper()
	return url.Values{"csrf": {serversCSRF(t, h, p)}, "release_id": {"rel-1"}, "request_nonce": {"nonce-0123456789abcdef"}, "confirm": {"rollback"}}
}

func retainedDetail(status string) postgres.DeploymentDetail {
	d := sampleDetail("succeeded", false)
	d.Deployment.Status = status
	d.Release.State = "retained"
	return d
}

func TestRollbackFormShownOnlyForEligibleRetainedRelease(t *testing.T) {
	h, st, ops, p := rollbackHandler(t)
	ops.detail = retainedDetail("healthy")
	page := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
	for _, want := range []string{`action="/deployments/rollback"`, `name="release_id" value="rel-1"`, `name="csrf" value="`, `type="checkbox" name="confirm" value="rollback" required`, "Roll back to this release"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := nonceInputRE.FindAllStringSubmatch(page, -1); len(n) != 1 || len(n[0][1]) != 32 {
		t.Fatalf("nonce %v", n)
	}
	form := page[strings.Index(page, `<form method="post" action="/deployments/rollback">`):]
	form = form[:strings.Index(form, "</form>")]
	if got := strings.Count(form, "<input"); got != 4 {
		t.Fatalf("inputs=%d: %s", got, form)
	}
	if st.gotRelease != "rel-1" {
		t.Fatalf("eligibility asked for %q", st.gotRelease)
	}

	// Not eligible, not retained, and no store: no form, and a non-retained
	// release never even asks the store.
	st.eligible = false
	if page = opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(page, "/deployments/rollback") {
		t.Error("form shown for an ineligible release")
	}
	for _, state := range []string{"active", "ready", "candidate", "removed", "failed"} {
		st.eligible, st.eligibleCall = true, 0
		d := retainedDetail("healthy")
		d.Release.State = state
		ops.detail = d
		if page = opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(page, "/deployments/rollback") || st.eligibleCall != 0 {
			t.Errorf("%s: form=%v asked=%d", state, strings.Contains(page, "/deployments/rollback"), st.eligibleCall)
		}
	}
	ops.detail = retainedDetail("healthy")
	h.config.Rollback = nil
	if page = opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(page, "/deployments/rollback") {
		t.Error("form shown without a store")
	}
	ops.detail.Release = nil
	h.config.Rollback = st
	if rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1"); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "/deployments/rollback") {
		t.Errorf("no release: %d", rr.Code)
	}
}

func TestRollbackFormEligibilityFailureIsGeneric500(t *testing.T) {
	h, st, ops, p := rollbackHandler(t)
	ops.detail = retainedDetail("healthy")
	st.eligibleErr = errors.New("ghs_SECRET database detail")
	rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1")
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "SECRET") {
		t.Fatalf("= %d %q", rr.Code, rr.Body.String())
	}
}

func TestRolledBackPill(t *testing.T) {
	h, _, ops, p := rollbackHandler(t)
	ops.detail = sampleDetail("succeeded", false)
	ops.detail.Deployment.Status = "rolled_back"
	page := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String()
	if !strings.Contains(page, `<span class="pill neutral">rolled back</span>`) {
		t.Fatalf("no rolled back pill:\n%s", page)
	}
	ops.detail.Deployment.Status = "healthy"
	if page = opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(page, "rolled back") {
		t.Fatal("pill shown for a healthy deployment")
	}
	// A rolled_back deployment whose release is active again (rolled forward)
	// no longer reads as rolled back.
	ops.detail = sampleDetail("succeeded", true)
	ops.detail.Deployment.Status = "rolled_back"
	if page = opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1").Body.String(); strings.Contains(page, `>rolled back</span>`) {
		t.Fatal("pill shown while the release is active")
	}
}

func TestRollbackDisabledWithoutStore(t *testing.T) {
	h, _, p := operationsHandler(t)
	if rr := cancelPost(h, p, "/deployments/rollback", url.Values{"x": {"y"}}, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no store = %d", rr.Code)
	}
}

func TestRollbackRequestGates(t *testing.T) {
	h, st, _, p := rollbackHandler(t)
	if rr := opsGet(h, p, http.MethodGet, "/deployments/rollback"); rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("GET = %d", rr.Code)
	}
	form := rollbackForm(t, h, p)
	if rr := cancelPost(h, p, "/deployments/rollback?x=1", form, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("query = %d", rr.Code)
	}
	if rr := cancelPost(h, p, "/deployments/rollback", form, func(r *http.Request) { r.Header.Del("Cookie") }); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d", rr.Code)
	}
	bad := rollbackForm(t, h, p)
	bad.Set("csrf", "bogus")
	if rr := cancelPost(h, p, "/deployments/rollback", bad, nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("bad csrf = %d", rr.Code)
	}
	if rr := cancelPost(h, p, "/deployments/rollback", form, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.test") }); rr.Code != http.StatusBadRequest {
		t.Fatalf("cross-site = %d", rr.Code)
	}
	for _, drop := range []string{"csrf", "release_id", "request_nonce", "confirm"} {
		f := rollbackForm(t, h, p)
		f.Del(drop)
		if rr := cancelPost(h, p, "/deployments/rollback", f, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("missing %s = %d", drop, rr.Code)
		}
	}
	for _, extra := range []string{"environment_id", "target", "strategy"} {
		f := rollbackForm(t, h, p)
		f.Set(extra, "x")
		if rr := cancelPost(h, p, "/deployments/rollback", f, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("extra %s = %d", extra, rr.Code)
		}
	}
	for name, set := range map[string][2]string{"no confirm value": {"confirm", ""}, "wrong confirm": {"confirm", "yes"}, "empty nonce": {"request_nonce", ""}, "space release": {"release_id", "a b"}, "empty release": {"release_id", ""}} {
		f := rollbackForm(t, h, p)
		f.Set(set[0], set[1])
		if rr := cancelPost(h, p, "/deployments/rollback", f, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", name, rr.Code)
		}
	}
	if st.enqueueCalls != 0 {
		t.Fatal("store reached on rejected requests")
	}
}

func TestRollbackSuccessRedirectsToTheNewDeployment(t *testing.T) {
	h, st, _, p := rollbackHandler(t)
	rr := cancelPost(h, p, "/deployments/rollback", rollbackForm(t, h, p), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/deployments/detail?id=rbd_abc" {
		t.Fatalf("= %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if st.enqueueCalls != 1 || st.gotOwner != "owner" || st.gotRelease != "rel-1" || st.gotNonce != "nonce-0123456789abcdef" {
		t.Fatalf("store %+v", st)
	}
}

func TestRollbackErrorMapping(t *testing.T) {
	const secret = "ghs_SECRETTOKEN internal detail"
	cases := []struct {
		name string
		err  error
		code int
		body string
	}{
		{"invalid", postgres.ErrRollbackInvalid, 400, "bad request\n"},
		{"unauthorized", postgres.ErrRollbackUnauthorized, 403, "this session is not authorized to roll back\n"},
		{"not found", postgres.ErrRollbackNotFound, 404, "that release was not found\n"},
		{"in flight", postgres.ErrRollbackInFlight, 409, "a deployment is in progress in this environment; wait for it to finish\n"},
		{"revoked secret", postgres.ErrRollbackSecretRevoked, 409, "this release uses a secret version that was revoked; redeploy instead\n"},
		{"not eligible", postgres.ErrRollbackNotEligible, 409, "this release can no longer be rolled back to; reload the page\n"},
		{"conflict", postgres.ErrRollbackConflict, 409, "this form was already submitted with different values; reload the page\n"},
		{"wrapped sentinel", fmt.Errorf("%s: %w", secret, postgres.ErrRollbackInFlight), 409, "a deployment is in progress in this environment; wait for it to finish\n"},
		{"unknown", errors.New(secret), 500, "the rollback could not be queued; retry\n"},
	}
	for _, tc := range cases {
		h, st, _, p := rollbackHandler(t)
		st.enqueueErr = tc.err
		rr := cancelPost(h, p, "/deployments/rollback", rollbackForm(t, h, p), nil)
		if rr.Code != tc.code || rr.Body.String() != tc.body {
			t.Errorf("%s = %d %q", tc.name, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "SECRET") || rr.Header().Get("Location") != "" {
			t.Errorf("%s leaked or redirected", tc.name)
		}
	}
}
