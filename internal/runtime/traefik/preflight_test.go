package traefik

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/queue"
)

func preflightServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func requireClass(t *testing.T, err error, want ErrorClass) *ObservationError {
	t.Helper()
	var oe *ObservationError
	if !errors.As(err, &oe) || oe.Class != want {
		t.Fatalf("want class %v, got %v", want, err)
	}
	return oe
}

func TestPreflightSuccess(t *testing.T) {
	srv := preflightServer(t, 200, `{"Version":"3.1.0","Codename":"x"}`)
	obs, err := NewObserver(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := obs.Preflight(context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestPreflightMalformedBodies(t *testing.T) {
	for _, body := range []string{`[]`, `"x"`, `null`, `42`, `not json`, ``} {
		srv := preflightServer(t, 200, body)
		obs, err := NewObserver(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		requireClass(t, obs.Preflight(context.Background()), ErrClassMalformed)
	}
}

func TestPreflightStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		class  ErrorClass
		fail   queue.FailureClass
	}{
		{401, ErrClassConfiguration, queue.FailureClassPermanentConfiguration},
		{403, ErrClassConfiguration, queue.FailureClassPermanentConfiguration},
		{404, ErrClassConfiguration, queue.FailureClassPermanentConfiguration},
		{418, ErrClassConfiguration, queue.FailureClassPermanentConfiguration},
		{408, ErrClassTransient, queue.FailureClassTransient},
		{425, ErrClassTransient, queue.FailureClassTransient},
		{429, ErrClassTransient, queue.FailureClassTransient},
		{500, ErrClassTransient, queue.FailureClassTransient},
		{503, ErrClassTransient, queue.FailureClassTransient},
	}
	for _, c := range cases {
		srv := preflightServer(t, c.status, "x")
		obs, err := NewObserver(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		err = obs.Preflight(context.Background())
		requireClass(t, err, c.class)
		if got := ClassifyObservation(err); got != c.fail {
			t.Fatalf("status %d: classify = %v, want %v", c.status, got, c.fail)
		}
	}
}

func TestPreflightCredentialMessages(t *testing.T) {
	for status, want := range map[int]string{
		401: "rejected the agent's credentials",
		403: "rejected the agent's credentials",
		404: "not found at the configured url",
	} {
		srv := preflightServer(t, status, "x")
		obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
		if err != nil {
			t.Fatal(err)
		}
		err = obs.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("status %d: want message containing %q, got %v", status, want, err)
		}
	}
}

func TestPreflightTimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	obs, err := newObserver(srv.URL, 20*time.Millisecond, WithBasicAuth(testAuthUser, testAuthPass))
	if err != nil {
		t.Fatal(err)
	}
	err = obs.Preflight(context.Background())
	requireClass(t, err, ErrClassTransient)
	if strings.Contains(err.Error(), testAuthPass) {
		t.Fatal("timeout error leaks password")
	}
}

func TestPreflightRedirectRefusedIsMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
	if err != nil {
		t.Fatal(err)
	}
	err = obs.Preflight(context.Background())
	requireClass(t, err, ErrClassMalformed)
	if strings.Contains(err.Error(), testAuthPass) {
		t.Fatal("redirect error leaks password")
	}
}

func TestPreflightSendsCredentialsAndNeverLeaksThem(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
	if err != nil {
		t.Fatal(err)
	}
	if err := obs.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !gotOK || gotUser != testAuthUser || gotPass != testAuthPass {
		t.Fatalf("credentials not sent on preflight: ok=%v", gotOK)
	}
	for _, st := range []int{401, 403, 404, 418, 500, 503} {
		status = st
		err := obs.Preflight(context.Background())
		if err == nil {
			t.Fatalf("status %d: expected error", st)
		}
		for _, s := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if strings.Contains(s, testAuthPass) {
				t.Fatalf("status %d: error leaks password: %s", st, s)
			}
		}
	}
}

func TestObserveAuthRejectionIsConfiguration(t *testing.T) {
	for _, st := range []int{401, 403} {
		srv := statusOnlyServer(t, st)
		obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := obs.ObserveRouter(context.Background(), "r")
		requireClass(t, rerr, ErrClassConfiguration)
		_, serr := obs.ObserveService(context.Background(), "s")
		requireClass(t, serr, ErrClassConfiguration)
		for _, e := range []error{rerr, serr} {
			if got := ClassifyObservation(e); got != queue.FailureClassPermanentConfiguration {
				t.Fatalf("status %d: classify = %v", st, got)
			}
			if strings.Contains(e.Error(), testAuthPass) {
				t.Fatalf("status %d: leaks password", st)
			}
		}
	}
}

func TestObserveServerErrorStillTransientAndNotFoundUnchanged(t *testing.T) {
	srv500 := statusOnlyServer(t, 500)
	obs, _ := NewObserver(srv500.URL)
	_, err := obs.ObserveRouter(context.Background(), "r")
	requireClass(t, err, ErrClassTransient)
	_, err = obs.ObserveService(context.Background(), "s")
	requireClass(t, err, ErrClassTransient)

	srv404 := statusOnlyServer(t, 404)
	obs, _ = NewObserver(srv404.URL)
	ro, err := obs.ObserveRouter(context.Background(), "r")
	if err != nil || ro.Loaded {
		t.Fatalf("router 404: got %+v, %v", ro, err)
	}
	so, err := obs.ObserveService(context.Background(), "s")
	if err != nil || so.Loaded {
		t.Fatalf("service 404: got %+v, %v", so, err)
	}
}

func statusOnlyServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}
