package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// releaseReaderStub is an OperationsReader that also serves the optional
// EnvironmentReleaseReader; it returns rows only for its own owner, like the
// real owner-scoped query.
type releaseReaderStub struct {
	operationsStub
	ownerID  string
	releases []postgres.EnvironmentRelease
	err      error
	gotEnv   string
	calls    int
}

func (s *releaseReaderStub) ListEnvironmentReleases(_ context.Context, p auth.Principal, environmentID string) ([]postgres.EnvironmentRelease, error) {
	s.calls++
	s.gotEnv = environmentID
	if s.err != nil {
		return nil, s.err
	}
	if p.OwnerID != s.ownerID {
		return []postgres.EnvironmentRelease{}, nil
	}
	return s.releases, nil
}

func routeReleasesGet(t *testing.T, h *Handler, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, serversRequest(http.MethodGet, "/environments/route?environment_id=env-a", nil, p, h))
	return rr
}

func sampleEnvReleases() []postgres.EnvironmentRelease {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	done := at.Add(time.Minute)
	return []postgres.EnvironmentRelease{
		{ID: "rel-active", State: "active", Strategy: "blue_green", CreatedAt: at},
		{ID: "rel-retained", State: "retained", Strategy: "blue_green", CreatedAt: at.Add(-time.Hour)},
		{ID: "rel-removing", State: "removing", Strategy: "blue_green", CreatedAt: at.Add(-2 * time.Hour), Cleanup: &postgres.ReleaseCleanup{ID: 1, Attempts: 1}},
		{ID: "rel-removed", State: "removed", Strategy: "stop_then_start", CreatedAt: at.Add(-3 * time.Hour), Cleanup: &postgres.ReleaseCleanup{ID: 2, Result: "removed", ImageKept: true, CompletedAt: &done, Attempts: 1}},
		{ID: "rel-failedcand", State: "removing", Strategy: "blue_green", CreatedAt: at.Add(-4 * time.Hour), Cleanup: &postgres.ReleaseCleanup{ID: 3, Result: "failed", ErrorClass: "engine", Summary: "daemon unreachable", Attempts: 3, CompletedAt: &done}},
		{ID: "rel-skipped", State: "removing", Strategy: "blue_green", CreatedAt: at.Add(-5 * time.Hour), Cleanup: &postgres.ReleaseCleanup{ID: 4, Result: "skipped", ErrorClass: "identity", Attempts: 1, CompletedAt: &done}},
	}
}

func routeReleasesHandler(t *testing.T) (*Handler, *releaseReaderStub, auth.Principal) {
	t.Helper()
	h, admin, p := routeHeadHandler(t)
	admin.getFound = true
	stub := &releaseReaderStub{ownerID: p.OwnerID, releases: sampleEnvReleases()}
	h.config.OperationsReader = stub
	return h, stub, p
}

func TestRoutePageShowsReleasesAndCleanup(t *testing.T) {
	h, stub, p := routeReleasesHandler(t)
	rr := routeReleasesGet(t, h, p)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	b := rr.Body.String()
	if stub.gotEnv != "env-a" {
		t.Fatalf("env = %q", stub.gotEnv)
	}
	for _, w := range []string{
		"<h2>Releases</h2>", "rel-active", `pill ok">active`, `pill neutral">retained`, `pill busy">removing`, `pill neutral">removed`,
		`pill busy">in progress`, `pill ok">removed</span> container removed; image kept`,
		`pill bad">failed</span> engine: daemon unreachable (attempt 3)`, `pill bad">skipped</span> identity (attempt 1)`,
	} {
		if !strings.Contains(b, w) {
			t.Errorf("missing %q", w)
		}
	}
	// Existing content is intact.
	if !strings.Contains(b, "Runtime configuration") || !strings.Contains(b, `action="/environments/route"`) {
		t.Error("existing page content missing")
	}
}

func TestRoutePageReleasesOwnerScoped(t *testing.T) {
	h, stub, p := routeReleasesHandler(t)
	stub.ownerID = "someone-else"
	b := routeReleasesGet(t, h, p).Body.String()
	if strings.Contains(b, "Releases</h2>") || strings.Contains(b, "rel-active") {
		t.Fatal("other owner's releases rendered")
	}
}

func TestRoutePageReleasesEmptyAndUnavailable(t *testing.T) {
	h, stub, p := routeReleasesHandler(t)
	stub.releases = nil
	rr := routeReleasesGet(t, h, p)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "Releases</h2>") {
		t.Fatalf("empty: %d", rr.Code)
	}
	// An OperationsReader without the optional method (or none) omits the section.
	h.config.OperationsReader = &operationsStub{}
	if rr = routeReleasesGet(t, h, p); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "Releases</h2>") {
		t.Fatalf("plain reader: %d", rr.Code)
	}
	h.config.OperationsReader = nil
	if rr = routeReleasesGet(t, h, p); rr.Code != http.StatusOK {
		t.Fatalf("nil reader: %d", rr.Code)
	}
}

func TestRoutePageReleasesEscapesAndTruncatesSummary(t *testing.T) {
	h, stub, p := routeReleasesHandler(t)
	evil := `<script>alert(1)</script>` + strings.Repeat("x", 500)
	stub.releases = []postgres.EnvironmentRelease{{ID: `<i>rel</i>`, State: "removing", Strategy: "blue_green", CreatedAt: time.Now(),
		Cleanup: &postgres.ReleaseCleanup{Result: "failed", ErrorClass: "other", Summary: evil, Attempts: 1}}}
	b := routeReleasesGet(t, h, p).Body.String()
	if strings.Contains(b, "<script>alert") || strings.Contains(b, "<i>rel</i>") {
		t.Fatal("unescaped DB text")
	}
	if !strings.Contains(b, "&lt;script&gt;") {
		t.Fatal("escaped summary missing")
	}
	if strings.Contains(b, strings.Repeat("x", 300)) || !strings.Contains(b, "...") {
		t.Fatal("summary not truncated")
	}
}

func TestRoutePageReleaseReadFailureKeepsPage(t *testing.T) {
	h, stub, p := routeReleasesHandler(t)
	stub.err = errors.New("db down")
	rr := routeReleasesGet(t, h, p)
	b := rr.Body.String()
	if rr.Code != http.StatusOK || stub.calls != 1 || strings.Contains(b, "Releases</h2>") || !strings.Contains(b, `action="/environments/route"`) {
		t.Fatalf("status=%d calls=%d", rr.Code, stub.calls)
	}
}
