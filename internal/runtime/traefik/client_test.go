package traefik

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/queue"
)

// routerJSON/serviceJSON build the exact fixed shape client.go decodes,
// keeping the test fixtures close to Traefik's documented API response
// shape without depending on this package's own unexported decode types.
func routerJSON(rule, service, certResolver string, errs []string) []byte {
	type tls struct {
		CertResolver string `json:"certResolver"`
	}
	body := map[string]any{
		"rule":    rule,
		"service": service,
		"status":  "enabled",
	}
	if certResolver != "" {
		body["tls"] = tls{CertResolver: certResolver}
	}
	if errs != nil {
		body["error"] = errs
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return b
}

func serviceJSON(urls []string, errs []string) []byte {
	type server struct {
		URL string `json:"url"`
	}
	var servers []server
	for _, u := range urls {
		servers = append(servers, server{URL: u})
	}
	body := map[string]any{
		"loadBalancer": map[string]any{"servers": servers},
		"status":       "enabled",
	}
	if errs != nil {
		body["error"] = errs
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return b
}

// expectedNames returns the router/service names and backend URL Verify
// derives internally for r, so tests can build a fake server that answers
// exactly those paths.
func expectedNames(t *testing.T, r EnvironmentRoute) (router, service, backend string) {
	t.Helper()
	router = routerName(r)
	service = serviceName(r)
	var err error
	backend, err = backendServerURL(r)
	if err != nil {
		t.Fatalf("backendServerURL: %v", err)
	}
	return
}

type fakeAPI struct {
	mux *http.ServeMux
	srv *httptest.Server
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{mux: http.NewServeMux()}
	f.srv = httptest.NewServer(f.mux)
	return f
}

func (f *fakeAPI) url() string { return f.srv.URL }
func (f *fakeAPI) close()      { f.srv.Close() }

func routerPath(name string) string  { return "/api/http/routers/" + name + "@file" }
func servicePath(name string) string { return "/api/http/services/" + name + "@file" }

func TestVerifyLoadedCorrect(t *testing.T) {
	r := validRoute()
	router, service, backend := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(routerJSON(ruleValue(r.Hostname), service, r.CertResolverName, nil))
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(serviceJSON([]string{backend}, nil))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	result, err := obs.Verify(t.Context(), r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.Router.Loaded || !result.Service.Loaded {
		t.Fatalf("expected both loaded, got %+v", result)
	}
}

func TestVerifyRouterNotFound(t *testing.T) {
	r := validRoute()
	router, service, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(serviceJSON(nil, nil))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.Verify(t.Context(), r)
	if !errors.Is(err, ErrRouterNotLoaded) {
		t.Fatalf("expected ErrRouterNotLoaded, got %v", err)
	}
	if ClassifyObservation(err) != queue.FailureClassTransient {
		t.Fatalf("expected transient classification")
	}
}

func TestVerifyServiceNotFound(t *testing.T) {
	r := validRoute()
	router, service, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(routerJSON(ruleValue(r.Hostname), service, r.CertResolverName, nil))
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.Verify(t.Context(), r)
	if !errors.Is(err, ErrServiceNotLoaded) {
		t.Fatalf("expected ErrServiceNotLoaded, got %v", err)
	}
}

func TestVerifyWrongBackend(t *testing.T) {
	r := validRoute()
	router, service, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(routerJSON(ruleValue(r.Hostname), service, r.CertResolverName, nil))
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(serviceJSON([]string{"http://autodeploy-rt-wrongone:8080"}, nil))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.Verify(t.Context(), r)
	if !errors.Is(err, ErrWrongBackend) {
		t.Fatalf("expected ErrWrongBackend, got %v", err)
	}
	if got := ClassifyObservation(err); got != queue.FailureClassCorruptState {
		t.Fatalf("expected corrupt_state classification, got %v", got)
	}
}

func TestVerifyWrongRule(t *testing.T) {
	r := validRoute()
	router, service, backend := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(routerJSON("Host(`wrong.example.com`)", service, r.CertResolverName, nil))
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(serviceJSON([]string{backend}, nil))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.Verify(t.Context(), r)
	if !errors.Is(err, ErrRuleMismatch) {
		t.Fatalf("expected ErrRuleMismatch, got %v", err)
	}
}

func TestVerifyMissingTLS(t *testing.T) {
	r := validRoute()
	router, service, backend := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(routerJSON(ruleValue(r.Hostname), service, "", nil)) // no certResolver
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(serviceJSON([]string{backend}, nil))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.Verify(t.Context(), r)
	if !errors.Is(err, ErrTLSMismatch) {
		t.Fatalf("expected ErrTLSMismatch, got %v", err)
	}
}

func TestVerifyConfigRejected(t *testing.T) {
	r := validRoute()
	router, service, backend := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(routerJSON(ruleValue(r.Hostname), service, r.CertResolverName, []string{"invalid rule syntax"}))
	})
	api.mux.HandleFunc(servicePath(service), func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(serviceJSON([]string{backend}, nil))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.Verify(t.Context(), r)
	if !errors.Is(err, ErrConfigRejected) {
		t.Fatalf("expected ErrConfigRejected, got %v", err)
	}
	if got := ClassifyObservation(err); got != queue.FailureClassPermanentConfiguration {
		t.Fatalf("expected permanent_configuration classification, got %v", got)
	}
}

func TestObserveRouterNon2xxIsTransient(t *testing.T) {
	r := validRoute()
	router, _, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.ObserveRouter(t.Context(), router)
	var obsErr *ObservationError
	if !errors.As(err, &obsErr) || obsErr.Class != ErrClassTransient {
		t.Fatalf("expected transient ObservationError, got %v", err)
	}
	if got := ClassifyObservation(err); got != queue.FailureClassTransient {
		t.Fatalf("expected transient classification, got %v", got)
	}
}

func TestObserveRouterOversizedBodyIsMalformed(t *testing.T) {
	r := validRoute()
	router, _, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	huge := strings.Repeat("a", maxObservationResponseBytes+1024)
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		_, _ = fmt.Fprintf(w, `{"rule":"%s"}`, huge)
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.ObserveRouter(t.Context(), router)
	var obsErr *ObservationError
	if !errors.As(err, &obsErr) || obsErr.Class != ErrClassMalformed {
		t.Fatalf("expected malformed ObservationError for oversized body, got %v", err)
	}
	if got := ClassifyObservation(err); got != queue.FailureClassCorruptState {
		t.Fatalf("expected corrupt_state classification, got %v", got)
	}
}

// TestGetRejectsOversizedBodyPrecisely calls the unexported get() seam
// directly (this test file is in package traefik) so the size-limit guard
// is proven in isolation from json.Unmarshal's own failure on a body
// truncated mid-token -- ObserveRouter's own oversized-body test above
// passes even with the size check removed, because io.LimitReader still
// truncates the read and the truncated bytes then fail to parse as JSON
// for an unrelated reason. This test's oversized body is exactly one byte
// too large and otherwise well-formed up to the truncation point, so
// disabling get()'s explicit `len(body) > maxObservationResponseBytes`
// check (verified live during Step 10b: see the task report) makes this
// specific test keep going down a truncated-body path with a distinct,
// checkable error message rather than merely "some malformed error or
// other".
func TestGetRejectsOversizedBodyPrecisely(t *testing.T) {
	api := newFakeAPI()
	defer api.close()
	huge := strings.Repeat("a", maxObservationResponseBytes+1)
	api.mux.HandleFunc("/oversized", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(huge))
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, _, err = obs.get(t.Context(), "test-op", "/oversized")
	var obsErr *ObservationError
	if !errors.As(err, &obsErr) || obsErr.Class != ErrClassMalformed {
		t.Fatalf("expected malformed ObservationError, got %v", err)
	}
	if !strings.Contains(obsErr.Error(), "exceeds") {
		t.Fatalf("expected size-limit error message, got %q", obsErr.Error())
	}
}

func TestObserveRouterRedirectNotFollowed(t *testing.T) {
	r := validRoute()
	router, _, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/somewhere-else", http.StatusFound)
	})

	obs, err := NewObserver(api.url())
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	_, err = obs.ObserveRouter(t.Context(), router)
	var obsErr *ObservationError
	if !errors.As(err, &obsErr) || obsErr.Class != ErrClassMalformed {
		t.Fatalf("expected malformed ObservationError for unfollowed redirect, got %v", err)
	}
}

func TestNewObserverRejectsNonLoopbackBaseURL(t *testing.T) {
	for _, u := range []string{
		"http://example.com:8080",
		"http://10.0.0.5:8080",
		"http://[::1]:8080@evil.com",
		"ftp://127.0.0.1:8080",
	} {
		if _, err := NewObserver(u); !errors.Is(err, ErrNonLoopbackBaseURL) {
			t.Fatalf("base url %q: expected ErrNonLoopbackBaseURL, got %v", u, err)
		}
	}
}

func TestNewObserverAcceptsLoopbackVariants(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"http://[::1]:8080",
	} {
		if _, err := NewObserver(u); err != nil {
			t.Fatalf("base url %q: expected accept, got %v", u, err)
		}
	}
}

func TestObserveRouterTimeout(t *testing.T) {
	r := validRoute()
	router, _, _ := expectedNames(t, r)

	api := newFakeAPI()
	defer api.close()
	blocked := make(chan struct{})
	api.mux.HandleFunc(routerPath(router), func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
		close(blocked)
	})

	obs, err := newObserver(api.url(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("newObserver: %v", err)
	}
	_, err = obs.ObserveRouter(t.Context(), router)
	var obsErr *ObservationError
	if !errors.As(err, &obsErr) || obsErr.Class != ErrClassTransient {
		t.Fatalf("expected transient ObservationError for timeout, got %v", err)
	}
	if got := ClassifyObservation(err); got != queue.FailureClassTransient {
		t.Fatalf("expected transient classification for timeout, got %v", got)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler never observed client timeout")
	}
}
