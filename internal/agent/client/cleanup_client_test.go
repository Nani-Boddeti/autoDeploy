package agentclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaimCleanupsAndReportBodies(t *testing.T) {
	var path, got string
	status := http.StatusOK
	respond := `{"cleanups":[{"cleanup_id":5,"release_id":"rel_a","environment_id":"env_1","runtime_instance_id":"42","image_digest":"ab"}]}`
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
	if err != nil || path != "/agent/v1/cleanup/claim" || got != `{"limit":4}` {
		t.Fatalf("claim: %v path=%s body=%s", err, path, got)
	}
	if len(items) != 1 || items[0] != (CleanupItem{CleanupID: 5, ReleaseID: "rel_a", EnvironmentID: "env_1", RuntimeInstanceID: "42", ImageDigest: "ab"}) {
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
