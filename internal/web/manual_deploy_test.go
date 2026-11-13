package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
)

const manualTestSHA = "0123456789abcdef0123456789abcdef01234567"

type manualStoreStub struct {
	targetErr, enqueueErr error
	ref                   string
	targetCalls           int
	enqueueCalls          int
	gotProject            string
	gotOwner              string
	gotInput              postgres.ManualDeployInput
}

func (s *manualStoreStub) ManualDeployTarget(_ context.Context, p auth.Principal, projectID string) (int64, int64, string, error) {
	s.targetCalls++
	s.gotOwner, s.gotProject = p.OwnerID, projectID
	if s.targetErr != nil {
		return 0, 0, "", s.targetErr
	}
	return 11, 22, s.ref, nil
}

func (s *manualStoreStub) EnqueueManualDeploy(_ context.Context, _ auth.Principal, in postgres.ManualDeployInput) (postgres.ManualDeployResult, error) {
	s.enqueueCalls++
	s.gotInput = in
	if s.enqueueErr != nil {
		return postgres.ManualDeployResult{}, s.enqueueErr
	}
	return postgres.ManualDeployResult{DeploymentID: deployment.ID("mnd_abc"), JobID: 5}, nil
}

type resolverStub struct {
	err                    error
	headCalls, verifyCalls int
	gotInst, gotRepo       int64
	gotRef, gotSHA         string
}

func (s *resolverStub) ResolveBranchHead(_ context.Context, inst, repo int64, ref string) (string, error) {
	s.headCalls++
	s.gotInst, s.gotRepo, s.gotRef = inst, repo, ref
	if s.err != nil {
		return "", s.err
	}
	return manualTestSHA, nil
}

func (s *resolverStub) VerifyCommitOnRef(_ context.Context, inst, repo int64, ref, sha string) error {
	s.verifyCalls++
	s.gotInst, s.gotRepo, s.gotRef, s.gotSHA = inst, repo, ref, sha
	return s.err
}

func manualHandler(t *testing.T) (*Handler, *manualStoreStub, *resolverStub, auth.Principal) {
	t.Helper()
	h, _, p := operationsHandler(t)
	st := &manualStoreStub{ref: "refs/heads/main"}
	rs := &resolverStub{}
	h.config.ManualDeploy, h.config.CommitResolver = st, rs
	return h, st, rs, p
}

func manualForm(t *testing.T, h *Handler, p auth.Principal) url.Values {
	t.Helper()
	return url.Values{"csrf": {serversCSRF(t, h, p)}, "project_id": {"proj"}, "request_nonce": {"nonce-0123456789abcdef"}, "mode": {"head"}, "commit_sha": {""}}
}

func TestManualDeployDisabledWithoutDependencies(t *testing.T) {
	h, _, p := operationsHandler(t)
	if rr := cancelPost(h, p, "/deployments/manual", url.Values{"x": {"y"}}, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no deps = %d", rr.Code)
	}
	h.config.ManualDeploy = &manualStoreStub{}
	if rr := cancelPost(h, p, "/deployments/manual", url.Values{"x": {"y"}}, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no resolver = %d", rr.Code)
	}
	h.config.ManualDeploy, h.config.CommitResolver = nil, &resolverStub{}
	if rr := cancelPost(h, p, "/deployments/manual", url.Values{"x": {"y"}}, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("no store = %d", rr.Code)
	}
}

func TestManualDeployRequestGates(t *testing.T) {
	h, st, rs, p := manualHandler(t)
	if rr := opsGet(h, p, http.MethodGet, "/deployments/manual"); rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("GET = %d", rr.Code)
	}
	form := manualForm(t, h, p)
	if rr := cancelPost(h, p, "/deployments/manual?x=1", form, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("query = %d", rr.Code)
	}
	if rr := cancelPost(h, p, "/deployments/manual", form, func(r *http.Request) { r.Header.Del("Cookie") }); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("no session = %d", rr.Code)
	}
	bad := manualForm(t, h, p)
	bad.Set("csrf", "bogus")
	if rr := cancelPost(h, p, "/deployments/manual", bad, nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("bad csrf = %d", rr.Code)
	}
	if rr := cancelPost(h, p, "/deployments/manual", form, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.test") }); rr.Code != http.StatusBadRequest {
		t.Fatalf("cross-site = %d", rr.Code)
	}
	if rr := cancelPost(h, p, "/deployments/manual", form, func(r *http.Request) { r.Header.Del("Origin") }); rr.Code != http.StatusBadRequest {
		t.Fatalf("no origin = %d", rr.Code)
	}
	// Wrong field set: missing each field, plus extras (a client-supplied ref/repository/installation).
	for _, drop := range []string{"csrf", "project_id", "request_nonce", "mode", "commit_sha"} {
		f := manualForm(t, h, p)
		f.Del(drop)
		if rr := cancelPost(h, p, "/deployments/manual", f, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("missing %s = %d", drop, rr.Code)
		}
	}
	for _, extra := range []string{"ref", "repository_id", "installation_id", "environment_id"} {
		f := manualForm(t, h, p)
		f.Set(extra, "x")
		if rr := cancelPost(h, p, "/deployments/manual", f, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("extra %s = %d", extra, rr.Code)
		}
	}
	dup := manualForm(t, h, p)
	dup.Add("mode", "head")
	if rr := cancelPost(h, p, "/deployments/manual", dup, nil); rr.Code != http.StatusBadRequest {
		t.Errorf("duplicate mode = %d", rr.Code)
	}
	if st.targetCalls+st.enqueueCalls+rs.headCalls+rs.verifyCalls != 0 {
		t.Fatal("dependencies reached on rejected requests")
	}
}

func TestManualDeployModeValidation(t *testing.T) {
	cases := []struct{ name, mode, sha, nonce, project string }{
		{"bad mode", "other", "", "nonce-0123456789abcdef", "proj"},
		{"empty mode", "", "", "nonce-0123456789abcdef", "proj"},
		{"head with sha", "head", manualTestSHA, "nonce-0123456789abcdef", "proj"},
		{"sha empty", "sha", "", "nonce-0123456789abcdef", "proj"},
		{"sha short", "sha", manualTestSHA[:39], "nonce-0123456789abcdef", "proj"},
		{"sha long", "sha", manualTestSHA + "0", "nonce-0123456789abcdef", "proj"},
		{"sha upper", "sha", strings.ToUpper(manualTestSHA), "nonce-0123456789abcdef", "proj"},
		{"sha non-hex", "sha", strings.Repeat("g", 40), "nonce-0123456789abcdef", "proj"},
		{"empty nonce", "head", "", "", "proj"},
		{"empty project", "head", "", "nonce-0123456789abcdef", ""},
		{"space project", "head", "", "nonce-0123456789abcdef", "a b"},
	}
	for _, tc := range cases {
		h, st, rs, p := manualHandler(t)
		f := manualForm(t, h, p)
		f.Set("mode", tc.mode)
		f.Set("commit_sha", tc.sha)
		f.Set("request_nonce", tc.nonce)
		f.Set("project_id", tc.project)
		if rr := cancelPost(h, p, "/deployments/manual", f, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", tc.name, rr.Code)
		}
		if st.targetCalls != 0 || rs.headCalls+rs.verifyCalls != 0 {
			t.Errorf("%s reached dependencies", tc.name)
		}
	}
}

func TestManualDeployHeadSuccessUsesBindingRefNotForm(t *testing.T) {
	h, st, rs, p := manualHandler(t)
	st.ref = "refs/heads/release"
	rr := cancelPost(h, p, "/deployments/manual", manualForm(t, h, p), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/deployments/detail?id=mnd_abc" {
		t.Fatalf("head = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if rs.headCalls != 1 || rs.verifyCalls != 0 || rs.gotRef != "refs/heads/release" || rs.gotInst != 11 || rs.gotRepo != 22 {
		t.Fatalf("resolver %+v", rs)
	}
	want := postgres.ManualDeployInput{ProjectID: "proj", CommitSHA: manualTestSHA, RequestNonce: "nonce-0123456789abcdef", ExpectedInstallationID: 11, ExpectedRepositoryID: 22, ExpectedRef: "refs/heads/release"}
	if st.gotInput != want || st.gotOwner != "owner" || st.gotProject != "proj" {
		t.Fatalf("enqueue %+v owner=%q", st.gotInput, st.gotOwner)
	}
}

func TestManualDeploySHASuccessVerifiesOnBindingRef(t *testing.T) {
	h, st, rs, p := manualHandler(t)
	f := manualForm(t, h, p)
	f.Set("mode", "sha")
	f.Set("commit_sha", manualTestSHA)
	rr := cancelPost(h, p, "/deployments/manual", f, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/deployments/detail?id=mnd_abc" {
		t.Fatalf("sha = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if rs.verifyCalls != 1 || rs.headCalls != 0 || rs.gotRef != "refs/heads/main" || rs.gotSHA != manualTestSHA {
		t.Fatalf("resolver %+v", rs)
	}
	if st.gotInput.CommitSHA != manualTestSHA || st.gotInput.ExpectedRef != "refs/heads/main" || st.gotInput.ExpectedInstallationID != 11 || st.gotInput.ExpectedRepositoryID != 22 {
		t.Fatalf("enqueue %+v", st.gotInput)
	}
}

func TestManualDeployErrorMapping(t *testing.T) {
	const secret = "ghs_SECRETTOKEN response body"
	cases := []struct {
		name    string
		target  error
		resolve error
		enqueue error
		code    int
		body    string
	}{
		{"target unauthorized", postgres.ErrManualDeployUnauthorized, nil, nil, 403, "this repository is not currently authorized for this project; reconcile the installation\n"},
		{"resolver unauthorized", nil, githubapp.ErrRepositoryUnauthorized, nil, 403, "this repository is not currently authorized for this project; reconcile the installation\n"},
		{"enqueue unauthorized", nil, nil, postgres.ErrManualDeployUnauthorized, 403, "this repository is not currently authorized for this project; reconcile the installation\n"},
		{"ref not found", nil, githubapp.ErrRefNotFound, nil, 422, "the bound branch was not found\n"},
		{"commit not found", nil, githubapp.ErrCommitNotFound, nil, 422, "that commit was not found in the repository\n"},
		{"commit not on ref", nil, githubapp.ErrCommitNotOnRef, nil, 422, "that commit is not on the bound branch\n"},
		{"in flight", nil, nil, postgres.ErrManualDeployInFlight, 409, "this commit is already deploying in this environment\n"},
		{"conflict", nil, nil, postgres.ErrManualDeployConflict, 409, "this form was already submitted with different values; reload the page\n"},
		{"unavailable", nil, githubapp.ErrGitHubUnavailable, nil, 503, "GitHub is unavailable; try again\n"},
		{"invalid", nil, nil, postgres.ErrManualDeployInvalid, 400, "bad request\n"},
		{"invalid ref", nil, githubapp.ErrInvalidRef, nil, 400, "bad request\n"},
		{"wrapped sentinel", nil, fmt.Errorf("%s: %w", secret, githubapp.ErrCommitNotOnRef), nil, 422, "that commit is not on the bound branch\n"},
		{"unknown target", errors.New(secret), nil, nil, 500, "the deployment could not be queued; retry\n"},
		{"unknown resolver", nil, errors.New(secret), nil, 500, "the deployment could not be queued; retry\n"},
		{"unknown enqueue", nil, nil, errors.New(secret), 500, "the deployment could not be queued; retry\n"},
	}
	for _, mode := range []string{"head", "sha"} {
		for _, tc := range cases {
			h, st, rs, p := manualHandler(t)
			st.targetErr, rs.err, st.enqueueErr = tc.target, tc.resolve, tc.enqueue
			f := manualForm(t, h, p)
			if mode == "sha" {
				f.Set("mode", "sha")
				f.Set("commit_sha", manualTestSHA)
			}
			rr := cancelPost(h, p, "/deployments/manual", f, nil)
			if rr.Code != tc.code || rr.Body.String() != tc.body {
				t.Errorf("%s/%s = %d %q", mode, tc.name, rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "SECRET") || rr.Header().Get("Location") != "" && tc.code != 303 {
				t.Errorf("%s/%s leaked or redirected", mode, tc.name)
			}
			if tc.resolve != nil && st.enqueueCalls != 0 {
				t.Errorf("%s/%s enqueued after resolver failure", mode, tc.name)
			}
		}
	}
}

var nonceInputRE = regexp.MustCompile(`name="request_nonce" value="([A-Za-z0-9_-]+)"`)

func TestProjectsPageRendersDeployFormPerBoundProject(t *testing.T) {
	h, _, _, p := manualHandler(t)
	stub := h.config.OperationsReader.(*operationsStub)
	stub.projects = []postgres.ProjectSummary{
		{ProjectID: "bound-a", HasBinding: true, BindingActive: true, RepositoryFullName: "o/a", RepositoryDefaultRef: "refs/heads/main"},
		{ProjectID: "bound-b", HasBinding: true, BindingActive: true, RepositoryFullName: "o/b", RepositoryDefaultRef: "refs/heads/dev"},
		{ProjectID: "unbound", LatestDeployment: &postgres.DeploymentSummary{DeploymentID: "d", Status: "queued"}},
		{ProjectID: "inactive", HasBinding: true, RepositoryFullName: "o/c", RepositoryDefaultRef: "refs/heads/main"},
		{ProjectID: "no-target", HasBinding: true, BindingActive: true, RepositoryFullName: "o/d"},
	}
	page := opsGet(h, p, http.MethodGet, "/projects").Body.String()
	if got := strings.Count(page, `action="/deployments/manual"`); got != 2 {
		t.Fatalf("forms = %d\n%s", got, page)
	}
	for _, want := range []string{`name="project_id" value="bound-a"`, `name="project_id" value="bound-b"`, `name="csrf" value="`, `type="radio" name="mode" value="head" checked`, `value="sha"`, `pattern="[0-9a-f]{40}"`, "refs/heads/dev", ">Deploy<"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{`value="unbound"`, `value="inactive"`, `value="no-target"`, "style="} {
		if strings.Contains(page, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	nonces := nonceInputRE.FindAllStringSubmatch(page, -1)
	if len(nonces) != 2 || nonces[0][1] == nonces[1][1] || len(nonces[0][1]) != 32 {
		t.Fatalf("nonces %v", nonces)
	}
	again := nonceInputRE.FindAllStringSubmatch(opsGet(h, p, http.MethodGet, "/projects").Body.String(), -1)
	if len(again) != 2 || again[0][1] == nonces[0][1] || again[1][1] == nonces[1][1] {
		t.Fatalf("nonce reused across renders: %v vs %v", again, nonces)
	}
}

func TestProjectsPageNoDeployFormWhenDisabled(t *testing.T) {
	h, stub, p := operationsHandler(t)
	stub.projects = []postgres.ProjectSummary{{ProjectID: "bound", HasBinding: true, BindingActive: true, RepositoryFullName: "o/a", RepositoryDefaultRef: "refs/heads/main"}}
	page := opsGet(h, p, http.MethodGet, "/projects").Body.String()
	if strings.Contains(page, "/deployments/manual") || strings.Contains(page, "<th>Deploy</th>") {
		t.Fatal("deploy form rendered without dependencies")
	}
}
