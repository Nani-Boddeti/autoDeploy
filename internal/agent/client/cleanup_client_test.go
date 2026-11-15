package agentclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestClaimCleanupsAndReportBodies(t *testing.T) {
	var path, got string
	status := http.StatusOK
	respond := `{"cleanups":[{"cleanup_id":5,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":"ab","kind":"retained"}]}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(status)
		if status == http.StatusConflict {
			_, _ = w.Write([]byte(`{"error":"integrity_conflict"}`))
			return
		}
		_, _ = w.Write([]byte(respond))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	items, err := c.ClaimCleanups(context.Background(), 4)
	if err != nil || path != "/agent/v1/cleanup/claim" || got != `{"limit":4,"kinds":["retained","failed_candidate"]}` {
		t.Fatalf("claim: %v path=%s body=%s", err, path, got)
	}
	if len(items) != 1 || !reflect.DeepEqual(items[0], CleanupItem{CleanupID: 5, ReleaseID: "rel_a", EnvironmentID: "env_1", RuntimeInstanceID: "42", ImageDigest: "ab", Kind: CleanupKindRetained}) {
		t.Fatalf("items = %+v", items)
	}
	respond = `{}`
	if err = c.ReportCleanupResult(context.Background(), 5, "removed", "", true, "ok"); err != nil {
		t.Fatal(err)
	}
	if path != "/agent/v1/cleanup/result" || got != `{"cleanup_id":5,"result":"removed","error_class":"","image_kept":true,"summary":"ok"}` {
		t.Fatalf("report path=%s body=%s", path, got)
	}
	status = http.StatusConflict
	err = c.ReportCleanupResult(context.Background(), 5, "failed", "engine", false, "x")
	if code, ok := ConflictCode(err); !ok || code != ConflictIntegrity || !errors.Is(err, ErrIntegrityConflict) {
		t.Fatalf("err=%v code=%q", err, code)
	}
}

// kindsServer mirrors the real control plane: internal/agent/http decodes the
// claim strictly, so a pre-21B server answers a request carrying kinds with
// 400 {"error":"invalid_request"} (writeError), and one that knows kinds answers
// the extended shape. A non-zero status forces that fixed answer instead.
func kindsServer(t *testing.T, knowsKinds bool, status int, errBody string, bodies *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		hasKinds := strings.Contains(string(b), "kinds")
		switch {
		case status != 0:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(errBody))
		case hasKinds && !knowsKinds:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
		case hasKinds:
			_, _ = w.Write([]byte(`{"cleanups":[` +
				`{"cleanup_id":1,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":"ab","kind":"retained"},` +
				`{"cleanup_id":2,"release_id":"rel_b","environment_id":"env_1","runtime_instance_id":"","image_digest":"","kind":"failed_candidate","runtime_instance_ids":["7","8"]},` +
				`{"cleanup_id":3,"release_id":"rel_c","environment_id":"env_1","runtime_instance_id":"9","image_digest":"ab","kind":"mystery"},` +
				`{"cleanup_id":4,"release_id":"rel_d","environment_id":"env_1","runtime_instance_id":"9","image_digest":"ab","kind":""}]}`))
		default:
			_, _ = w.Write([]byte(`{"cleanups":[{"cleanup_id":5,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":"ab"}]}`))
		}
	}))
}

func TestClaimCleanupsExtendedShape(t *testing.T) {
	var bodies []string
	srv := kindsServer(t, true, 0, "", &bodies)
	defer srv.Close()
	items, err := newTestClient(t, srv, nil).ClaimCleanups(context.Background(), 4)
	if err != nil || len(bodies) != 1 {
		t.Fatalf("err=%v requests=%v", err, bodies)
	}
	if len(items) != 4 || items[0].Kind != CleanupKindRetained || items[1].Kind != CleanupKindFailedCandidate ||
		!reflect.DeepEqual(items[1].RuntimeInstanceIDs, []string{"7", "8"}) || items[1].ImageDigest != "" || items[1].RuntimeInstanceID != "" {
		t.Fatalf("items = %+v", items)
	}
	// An unknown or empty kind in an extended answer is never guessed.
	if items[2].Kind != CleanupKindUnknown || items[3].Kind != CleanupKindUnknown {
		t.Fatalf("unknown kinds = %q %q", items[2].Kind, items[3].Kind)
	}
}

func TestClaimCleanupsFallsBackOnceForOlderControlPlane(t *testing.T) {
	var bodies []string
	srv := kindsServer(t, false, 0, "", &bodies)
	defer srv.Close()
	items, err := newTestClient(t, srv, nil).ClaimCleanups(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], "kinds") || bodies[1] != `{"limit":4}` {
		t.Fatalf("requests = %v", bodies)
	}
	if len(items) != 1 || items[0].CleanupID != 5 || items[0].Kind != CleanupKindRetained {
		t.Fatalf("items = %+v", items)
	}
}

func TestClaimCleanupsDoesNotFallBackOnOtherErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"400 other code": {http.StatusBadRequest, `{"error":"something_else"}`, ErrInvalidRequest},
		"400 empty body": {http.StatusBadRequest, ``, ErrInvalidRequest},
		"413":            {http.StatusRequestEntityTooLarge, `{"error":"request_entity_too_large"}`, ErrInvalidRequest},
		"401":            {http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrUnauthorized},
		"409":            {http.StatusConflict, `{"error":"integrity_conflict"}`, ErrIntegrityConflict},
	} {
		var bodies []string
		srv := kindsServer(t, true, tc.status, tc.body, &bodies)
		_, err := newTestClient(t, srv, nil).ClaimCleanups(context.Background(), 4)
		srv.Close()
		if !errors.Is(err, tc.want) || len(bodies) != 1 {
			t.Fatalf("%s: err=%v requests=%d", name, err, len(bodies))
		}
	}
}

// The fallback is one retry only: a server that also rejects the plain request
// surfaces its error and is not asked a third time.
func TestClaimCleanupsFallbackDoesNotLoop(t *testing.T) {
	var bodies []string
	srv := kindsServer(t, true, http.StatusBadRequest, `{"error":"invalid_request"}`, &bodies)
	defer srv.Close()
	_, err := newTestClient(t, srv, nil).ClaimCleanups(context.Background(), 4)
	if !errors.Is(err, ErrInvalidRequest) || len(bodies) != 2 {
		t.Fatalf("err=%v requests=%d", err, len(bodies))
	}
}
