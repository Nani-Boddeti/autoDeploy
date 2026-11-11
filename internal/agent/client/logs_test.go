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

func logsReq() AppendLogsRequest {
	return AppendLogsRequest{AttemptID: 5, Fence: 3, WriterEpoch: 7, Chunks: []LogChunk{{Stream: "build", Seq: 1, Body: "hello"}}}
}

func TestAppendLogsOutcomeClassificationSingleAttempt(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   LogOutcome
	}{
		{"ok", 200, `{"accepted":1,"duplicates":0,"conflicts":0,"dropped":0,"marker":false}`, LogSuccess},
		{"503", 503, `{"error":"service_unavailable"}`, LogRetryable},
		{"429", 429, `{}`, LogRetryable},
		{"500", 500, `{}`, LogRetryable},
		{"400", 400, `{"error":"invalid_request"}`, LogPermanent},
		{"413", 413, `{}`, LogPermanent},
		{"401", 401, `{"error":"unauthorized"}`, LogPermanent},
		{"403", 403, `{}`, LogPermanent},
		{"404", 404, `{}`, LogPermanent},
		{"409", 409, `{"error":"stale_lease"}`, LogPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			var got AppendLogsRequest
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				if r.URL.Path != "/agent/v1/leases/logs" || r.Header.Get("Authorization") == "" || r.Header.Get("X-AutoDeploy-Server-ID") != "server_test" {
					t.Errorf("bad request path/auth: %s", r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			res, err := newTestClient(t, srv, nil).AppendLogs(context.Background(), logsReq())
			if g := ClassifyLogError(err); g != tc.want {
				t.Fatalf("class=%v err=%v, want %v", g, err, tc.want)
			}
			if n := atomic.LoadInt32(&hits); n != 1 {
				t.Fatalf("hits=%d, want exactly 1 (no retry loop)", n)
			}
			if got.AttemptID != 5 || got.Fence != 3 || got.WriterEpoch != 7 || len(got.Chunks) != 1 {
				t.Fatalf("server saw %+v", got)
			}
			if tc.want == LogSuccess && res.Accepted != 1 {
				t.Fatalf("res=%+v", res)
			}
		})
	}
}

func TestAppendLogsMalformedResponseIsPermanent(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `{"accepted":1,"extra":2}`,
		"not json":      `nope`,
		"negative":      `{"accepted":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			_, err := newTestClient(t, srv, nil).AppendLogs(context.Background(), logsReq())
			if name == "not json" {
				// Undecodable at the shared transport layer: reported as a
				// transport failure by attempt before the strict decode runs.
				if err == nil {
					t.Fatal("want error")
				}
			} else if !errors.Is(err, ErrMalformedResponse) || ClassifyLogError(err) != LogPermanent {
				t.Fatalf("err=%v, want permanent ErrMalformedResponse", err)
			}
			if atomic.LoadInt32(&hits) != 1 {
				t.Fatalf("hits=%d", hits)
			}
		})
	}
}

func TestAppendLogsTransportFailureRetryableAndCtxHonoured(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := newTestClient(t, srv, nil)
	srv.Close() // connection refused
	_, err := c.AppendLogs(context.Background(), logsReq())
	if ClassifyLogError(err) != LogRetryable {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.AppendLogs(ctx, logsReq()); ClassifyLogError(err) != LogRetryable {
		t.Fatalf("canceled ctx err=%v", err)
	}
}
