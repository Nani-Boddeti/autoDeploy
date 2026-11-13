package agentclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReportDrainResultBodyAndConflictCode(t *testing.T) {
	var got string
	var calls int32
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/agent/v1/drains/result" {
			t.Errorf("path=%s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(status)
		if status == http.StatusConflict {
			_, _ = w.Write([]byte(`{"error":"integrity_conflict"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	if err := c.ReportDrainResult(context.Background(), 7, "rel_old", "41", "stopped", "predecessor stopped"); err != nil {
		t.Fatal(err)
	}
	want := `{"attempt_id":7,"predecessor_release_id":"rel_old","predecessor_runtime_instance_id":"41","outcome":"stopped","summary":"predecessor stopped"}`
	if got != want {
		t.Fatalf("body=%s want %s", got, want)
	}
	status = http.StatusConflict
	err := c.ReportDrainResult(context.Background(), 7, "rel_old", "41", "stopped", "x")
	if code, ok := ConflictCode(err); !ok || code != ConflictIntegrity || !errors.Is(err, ErrIntegrityConflict) {
		t.Fatalf("err=%v code=%q", err, code)
	}
}
