package traefik

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAuthUser = "agent"
	testAuthPass = "s3cr3t-Pa55word"
)

func TestBasicAuthHeaderSentExactlyWhenConfigured(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.ObserveRouter(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if !gotOK || gotUser != testAuthUser || gotPass != testAuthPass {
		t.Fatalf("basic auth not sent correctly: ok=%v user=%q", gotOK, gotUser)
	}

	obs, err = NewObserver(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.ObserveService(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	if gotOK {
		t.Fatal("Authorization header sent although no credential configured")
	}
}

func TestNewObserverRejectsBaseURLUserinfo(t *testing.T) {
	for _, u := range []string{
		"http://user:pass@127.0.0.1:8080",
		"http://user@127.0.0.1:8080",
		"http://:pass@localhost:8080",
	} {
		_, err := NewObserver(u)
		if err != ErrBaseURLUserinfo {
			t.Fatalf("%q: expected ErrBaseURLUserinfo, got %v", u, err)
		}
		if strings.Contains(err.Error(), "pass") {
			t.Fatalf("error leaks userinfo: %v", err)
		}
	}
}

func TestNewObserverParseErrorDoesNotEchoURL(t *testing.T) {
	_, err := NewObserver("http://user:topsecret@127.0.0.1:bad")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("parse error leaks password: %v", err)
	}
}

func TestWithBasicAuthRejectsUnusableCredentials(t *testing.T) {
	for _, c := range [][2]string{{"", "p"}, {"u", ""}, {"a:b", "p"}, {"u", "p\x00"}, {"u\n", "p"}, {"u", "p\x7f"}} {
		_, err := NewObserver("http://127.0.0.1:8080", WithBasicAuth(c[0], c[1]))
		if err != ErrInvalidBasicAuth {
			t.Fatalf("%q: expected ErrInvalidBasicAuth, got %v", c, err)
		}
	}
}

func TestCredentialNeverAppearsInErrorsOrFormatting(t *testing.T) {
	check := func(name string, v any) {
		t.Helper()
		for _, s := range []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v)} {
			if strings.Contains(s, testAuthPass) {
				t.Fatalf("%s leaks password: %s", name, s)
			}
		}
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		status := status
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, "nope")
		}))
		obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
		if err != nil {
			t.Fatal(err)
		}
		check("observer", obs)
		_, err = obs.ObserveRouter(context.Background(), "r")
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		check(fmt.Sprintf("error for %d", status), err.Error())
		srv.Close()
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	obs, err := newObserver(slow.URL, 20*time.Millisecond, WithBasicAuth(testAuthUser, testAuthPass))
	if err != nil {
		t.Fatal(err)
	}
	_, err = obs.ObserveRouter(context.Background(), "r")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	check("timeout error", err.Error())

	_, err = NewObserver("http://example.com", WithBasicAuth(testAuthUser, testAuthPass))
	if err == nil {
		t.Fatal("expected non-loopback error")
	}
	check("non-loopback error", err.Error())
}

func TestBasicAuthNeverFollowedThroughRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	obs, err := NewObserver(srv.URL, WithBasicAuth(testAuthUser, testAuthPass))
	if err != nil {
		t.Fatal(err)
	}
	_, err = obs.ObserveRouter(context.Background(), "r")
	if err == nil || strings.Contains(err.Error(), testAuthPass) {
		t.Fatalf("expected redirect refusal without secret, got %v", err)
	}
	if leaked.Load() {
		t.Fatal("credential followed a redirect")
	}
}
