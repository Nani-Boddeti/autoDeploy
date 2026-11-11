package agentclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

var serverConflictCodes = []string{
	ConflictIntegrity, ConflictCandidateNotReady, ConflictNoRouteHead, ConflictActivationOpen,
	ConflictEnvironmentBlocked, ConflictLateReportRecorded, ConflictRouteMoved, ConflictOutcomeMismatch,
	ConflictActivationUnresolved, ConflictCorruptState, ConflictConstraintViolation, ConflictOperationMismatch,
}

func TestMapStatusConflictCodes(t *testing.T) {
	c := &Client{logger: log.New(io.Discard, "", 0)}
	for _, code := range serverConflictCodes {
		err := c.mapStatus(409, []byte(`{"error":"`+code+`"}`))
		got, ok := ConflictCode(err)
		if !ok || got != code || !errors.Is(err, ErrIntegrityConflict) || errors.Is(err, ErrStaleLease) {
			t.Fatalf("%s: code=%q ok=%v err=%v", code, got, ok, err)
		}
	}
	err := c.mapStatus(409, []byte(`{"error":"stale_lease"}`))
	if err != ErrStaleLease {
		t.Fatalf("stale_lease err=%v, want exactly ErrStaleLease", err)
	}
	if _, ok := ConflictCode(err); ok {
		t.Fatal("stale_lease must not be a ConflictError")
	}
	for _, body := range []string{"", "not json", `{}`} {
		err := c.mapStatus(409, []byte(body))
		if code, ok := ConflictCode(err); !ok || code != "" || !errors.Is(err, ErrIntegrityConflict) {
			t.Fatalf("body %q: code=%q ok=%v err=%v", body, code, ok, err)
		}
	}
	for status, want := range map[int]error{401: ErrUnauthorized, 400: ErrInvalidRequest, 413: ErrInvalidRequest, 503: ErrServiceUnavailable, 500: ErrTransportFailure} {
		err := c.mapStatus(status, []byte(`{"error":"integrity_conflict"}`))
		if !errors.Is(err, want) {
			t.Fatalf("status %d err=%v, want %v", status, err, want)
		}
		if _, ok := ConflictCode(err); ok {
			t.Fatalf("status %d must not be a ConflictError", status)
		}
	}
}

func goodRouteDigest() (string, [32]byte) {
	var d [32]byte
	for i := range d {
		d[i] = byte(i + 1)
	}
	return "sha256:" + hex.EncodeToString(d[:]), d
}

func activateBody(mutate func(map[string]any)) string {
	s, _ := goodRouteDigest()
	m := map[string]any{
		"operation_id": "op_1", "route_revision": 7, "hostname": "app.example.com", "entrypoint_name": "websecure",
		"cert_resolver_name": "le", "backend_runtime_instance_id": "rt_1", "route_digest": s,
		"predecessor_release_id": "", "predecessor_runtime_instance_id": "",
	}
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestActivateRequestAndResponse(t *testing.T) {
	var gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/leases/activate" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(activateBody(nil)))
	}))
	defer srv.Close()
	var img [32]byte
	img[0], img[31] = 0xab, 0xcd
	resp, err := newTestClient(t, srv, nil).Activate(context.Background(), 5, 3, img)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"attempt_id":5,"fence":3,"image_digest":"` + hex.EncodeToString(img[:]) + `"}`
	if gotBody != want {
		t.Fatalf("body=%s want %s", gotBody, want)
	}
	_, wd := goodRouteDigest()
	if resp.OperationID != "op_1" || resp.RouteRevision != 7 || resp.Hostname != "app.example.com" || resp.EntrypointName != "websecure" ||
		resp.CertResolverName != "le" || resp.BackendRuntimeInstanceID != "rt_1" || resp.RouteDigest != wd {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestActivateMalformedResponses(t *testing.T) {
	good, _ := goodRouteDigest()
	cases := map[string]func(map[string]any){
		"no operation_id": func(m map[string]any) { delete(m, "operation_id") },
		"no revision":     func(m map[string]any) { delete(m, "route_revision") },
		"zero revision":   func(m map[string]any) { m["route_revision"] = 0 },
		"no hostname":     func(m map[string]any) { delete(m, "hostname") },
		"no entrypoint":   func(m map[string]any) { delete(m, "entrypoint_name") },
		"no resolver":     func(m map[string]any) { delete(m, "cert_resolver_name") },
		"no backend":      func(m map[string]any) { delete(m, "backend_runtime_instance_id") },
		"no digest":       func(m map[string]any) { delete(m, "route_digest") },
		"bare hex":        func(m map[string]any) { m["route_digest"] = strings.TrimPrefix(good, "sha256:") },
		"uppercase": func(m map[string]any) {
			m["route_digest"] = "sha256:" + strings.ToUpper(strings.TrimPrefix(good, "sha256:"))
		},
		"short":        func(m map[string]any) { m["route_digest"] = good[:len(good)-2] },
		"non-hex":      func(m map[string]any) { m["route_digest"] = "sha256:" + strings.Repeat("g", 64) },
		"wrong prefix": func(m map[string]any) { m["route_digest"] = "sha512:" + strings.TrimPrefix(good, "sha256:") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int32
			body := activateBody(mutate)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			_, err := newTestClient(t, srv, nil).Activate(context.Background(), 5, 3, [32]byte{})
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err=%v, want ErrMalformedResponse", err)
			}
			if n := atomic.LoadInt32(&calls); n != 1 {
				t.Fatalf("requests=%d, want 1 (not retried)", n)
			}
		})
	}
}

func TestActivatePredecessorParsing(t *testing.T) {
	serve := func(t *testing.T, mutate func(map[string]any)) (ActivateResponse, error) {
		t.Helper()
		body := activateBody(mutate)
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		defer srv.Close()
		return newTestClient(t, srv, nil).Activate(context.Background(), 5, 3, [32]byte{})
	}
	t.Run("both empty", func(t *testing.T) {
		resp, err := serve(t, func(m map[string]any) { m["predecessor_release_id"] = ""; m["predecessor_runtime_instance_id"] = "" })
		if err != nil || resp.PredecessorReleaseID != "" || resp.PredecessorRuntimeInstanceID != "" {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	})
	t.Run("both absent is malformed", func(t *testing.T) {
		if _, err := serve(t, func(m map[string]any) {
			delete(m, "predecessor_release_id")
			delete(m, "predecessor_runtime_instance_id")
		}); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("err=%v, want ErrMalformedResponse", err)
		}
	})
	t.Run("one key absent is malformed", func(t *testing.T) {
		if _, err := serve(t, func(m map[string]any) { m["predecessor_release_id"] = ""; delete(m, "predecessor_runtime_instance_id") }); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("err=%v, want ErrMalformedResponse", err)
		}
	})
	t.Run("both set", func(t *testing.T) {
		resp, err := serve(t, func(m map[string]any) {
			m["predecessor_release_id"] = "rel_0"
			m["predecessor_runtime_instance_id"] = "rt_0"
		})
		if err != nil || resp.PredecessorReleaseID != "rel_0" || resp.PredecessorRuntimeInstanceID != "rt_0" {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	})
	bad := map[string]func(map[string]any){
		"release only": func(m map[string]any) { m["predecessor_release_id"] = "rel_0" },
		"runtime only": func(m map[string]any) { m["predecessor_runtime_instance_id"] = "rt_0" },
		"invalid release id": func(m map[string]any) {
			m["predecessor_release_id"] = "rel\u0000"
			m["predecessor_runtime_instance_id"] = "rt_0"
		},
		"invalid runtime id": func(m map[string]any) {
			m["predecessor_release_id"] = "rel_0"
			m["predecessor_runtime_instance_id"] = "bad id/!"
		},
		"predecessor equals backend": func(m map[string]any) {
			m["predecessor_release_id"] = "rel_0"
			m["predecessor_runtime_instance_id"] = "rt_1"
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := serve(t, mutate); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err=%v, want ErrMalformedResponse", err)
			}
		})
	}
}

func TestActivateSurfacesConflictCode(t *testing.T) {
	var calls int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"no_route_head"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(t, srv, nil).Activate(context.Background(), 5, 3, [32]byte{})
	if code, ok := ConflictCode(err); !ok || code != ConflictNoRouteHead || !errors.Is(err, ErrIntegrityConflict) {
		t.Fatalf("err=%v code=%q", err, code)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("requests=%d, want 1", n)
	}
}

func captureServer(t *testing.T, got *string, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.URL.Path != "/agent/v1/leases/activation-result" {
			t.Errorf("path=%s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		*got = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
}

func TestReportActivationResultBodies(t *testing.T) {
	var got string
	var calls int32
	srv := captureServer(t, &got, &calls)
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	s, d := goodRouteDigest()
	if err := c.ReportActivationResult(context.Background(), 5, 3, "op_1", ActivationSucceeded(d)); err != nil {
		t.Fatal(err)
	}
	if want := `{"attempt_id":5,"fence":3,"operation_id":"op_1","succeeded":true,"observed_route_digest":"` + s + `"}`; got != want {
		t.Fatalf("success body=%s want %s", got, want)
	}
	failed, err := ActivationFailed("probe timed out")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReportActivationResult(context.Background(), 5, 3, "op_1", failed); err != nil {
		t.Fatal(err)
	}
	if want := `{"attempt_id":5,"fence":3,"operation_id":"op_1","succeeded":false,"failure_summary":"probe timed out"}`; got != want {
		t.Fatalf("failure body=%s want %s", got, want)
	}
}

func TestReportActivationResultRejectsInvalidClientSide(t *testing.T) {
	var got string
	var calls int32
	srv := captureServer(t, &got, &calls)
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	if _, err := ActivationFailed(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty summary err=%v", err)
	}
	if _, err := ActivationFailed(strings.Repeat("x", 2049)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversize summary err=%v", err)
	}
	if _, err := ActivationFailed(strings.Repeat("x", 2048)); err != nil {
		t.Fatalf("2048-byte summary err=%v", err)
	}
	_, d := goodRouteDigest()
	for name, call := range map[string]func() error{
		"zero result": func() error { return c.ReportActivationResult(context.Background(), 5, 3, "op_1", ActivationResult{}) },
		"bad attempt": func() error {
			return c.ReportActivationResult(context.Background(), 0, 3, "op_1", ActivationSucceeded(d))
		},
		"bad fence": func() error {
			return c.ReportActivationResult(context.Background(), 5, 0, "op_1", ActivationSucceeded(d))
		},
		"no op id": func() error { return c.ReportActivationResult(context.Background(), 5, 3, "", ActivationSucceeded(d)) },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s err=%v", name, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("requests=%d, want 0", n)
	}
}

func TestReportActivationResultLateReportConflict(t *testing.T) {
	var calls int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"late_report_recorded"}`))
	}))
	defer srv.Close()
	_, d := goodRouteDigest()
	err := newTestClient(t, srv, nil).ReportActivationResult(context.Background(), 5, 3, "op_1", ActivationSucceeded(d))
	if code, ok := ConflictCode(err); !ok || code != ConflictLateReportRecorded || !errors.Is(err, ErrIntegrityConflict) {
		t.Fatalf("err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("requests=%d, want 1", n)
	}
}

// retryOnce503Server answers 503 first, then 200 with okBody, recording
// every request body.
func retryOnce503Server(okBody string, bodies *[]string) *httptest.Server {
	var n int32
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"service_unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(okBody))
	}))
}

func TestActivateRetries503WithIdenticalBody(t *testing.T) {
	var bodies []string
	srv := retryOnce503Server(activateBody(nil), &bodies)
	defer srv.Close()
	if _, err := newTestClient(t, srv, nil).Activate(context.Background(), 5, 3, [32]byte{1}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("bodies=%q, want two identical", bodies)
	}
}

func TestReportActivationResultRetries503WithIdenticalBody(t *testing.T) {
	var bodies []string
	srv := retryOnce503Server(`{}`, &bodies)
	defer srv.Close()
	_, d := goodRouteDigest()
	if err := newTestClient(t, srv, nil).ReportActivationResult(context.Background(), 5, 3, "op_1", ActivationSucceeded(d)); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("bodies=%q, want two identical", bodies)
	}
}
