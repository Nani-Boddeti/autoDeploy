package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReconcileSendsOutcome(t *testing.T) {
	var got map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathReconcile {
			t.Errorf("path=%s, want %s", r.URL.Path, pathReconcile)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	if err := c.Reconcile(context.Background(), "env_1", 5, 3, "not_activated", [32]byte{}); err != nil {
		t.Fatal(err)
	}
	if got["outcome"] != "not_activated" || got["environment_id"] != "env_1" {
		t.Fatalf("body=%v, want outcome not_activated", got)
	}
}

func TestReconcileTerminalConflictsAreNotRetried(t *testing.T) {
	for _, code := range []string{"outcome_mismatch", "activation_unresolved"} {
		t.Run(code, func(t *testing.T) {
			var calls int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
			}))
			defer srv.Close()

			c := newTestClient(t, srv, nil)
			err := c.Reconcile(context.Background(), "env_1", 5, 3, "activated", [32]byte{})
			if !errors.Is(err, ErrIntegrityConflict) || errors.Is(err, ErrServiceUnavailable) {
				t.Fatalf("err=%v, want the non-retryable ErrIntegrityConflict", err)
			}
			if n := atomic.LoadInt32(&calls); n != 1 {
				t.Fatalf("requests=%d, want exactly 1 (no retry)", n)
			}
		})
	}
}
