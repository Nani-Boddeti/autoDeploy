package traefik

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file is ADR 0005's post-switch observation half of the health-gate
// contract: "the secured internal Traefik API reports the exact
// release/fence-specific router and service with no configuration error."
// It only ever performs read-only GET requests against a loopback-only API
// base URL; it never mutates Traefik configuration (that is exclusively
// write.go's atomic file replacement) and never dials the public HTTPS
// data-plane probe (a later step, per ADR 0005's separate "bounded HTTPS
// data-plane request through Traefik" requirement).

const (
	// maxObservationResponseBytes bounds every response this client reads.
	// Traefik's per-router/per-service API responses are small, fixed-shape
	// JSON objects; this is deliberately generous while still bounding
	// memory against a misbehaving or compromised local process answering
	// on the configured port.
	maxObservationResponseBytes = 64 * 1024
	defaultObservationTimeout   = 5 * time.Second
)

// ErrorClass mirrors internal/runtime/docker's own ErrorClass shape
// (transient/configuration/malformed), redefined here rather than shared
// because the two packages describe different transports (Docker's Unix
// socket vs. Traefik's loopback HTTP API) and must stay independently
// classifiable; classify.go maps this onto queue.FailureClass the same way
// docker/classify.go maps docker.ErrorClass.
type ErrorClass uint8

const (
	ErrClassUnknown ErrorClass = iota
	// ErrClassTransient covers dial/timeout/connection failures and a
	// non-2xx, non-404 HTTP status: retrying later, once Traefik has
	// finished reloading, may succeed.
	ErrClassTransient
	// ErrClassConfiguration covers a rejected request shape this client
	// itself refuses to send (currently only a non-loopback base URL,
	// caught at construction time -- see NewObserver).
	ErrClassConfiguration
	// ErrClassMalformed covers a response that answered but not in the
	// shape this client expects: an oversized body, an unexpected
	// redirect, or a body that does not decode as the expected JSON shape.
	// A loopback-only, single-purpose management API answering with any of
	// these is itself a signal of local inconsistency, not a routine network
	// blip.
	ErrClassMalformed
)

func (c ErrorClass) String() string {
	switch c {
	case ErrClassTransient:
		return "transient"
	case ErrClassConfiguration:
		return "configuration"
	case ErrClassMalformed:
		return "malformed"
	default:
		return "unknown"
	}
}

// ObservationError is the error type every transport-layer failure in this
// file is returned as. Op names the specific call that failed
// ("observe-router", "observe-service"); Err carries the underlying cause.
type ObservationError struct {
	Class ErrorClass
	Op    string
	Err   error
}

func (e *ObservationError) Error() string {
	return fmt.Sprintf("traefik api %s: %s: %v", e.Op, e.Class, e.Err)
}

func (e *ObservationError) Unwrap() error { return e.Err }

// Sentinel errors Verify returns for the non-transport outcomes ADR 0005's
// post-switch check must distinguish. Each is checked with errors.Is by
// classify.go and by callers that want to react to one specific outcome
// (10c/10d's reconciliation-required decision, for instance, cares whether
// a mismatch is a config rejection or a wrong backend, not just that
// verification failed).
var (
	// ErrNonLoopbackBaseURL is returned by NewObserver when the configured
	// API base URL's host is not 127.0.0.1, ::1, or localhost. The Traefik
	// API is described in ADR 0005 as "private, authenticated management
	// surfaces"; this client additionally refuses to ever address it
	// anywhere but loopback, structurally preventing a misconfigured base
	// URL from turning this observation call into a request against an
	// arbitrary host.
	ErrNonLoopbackBaseURL = errors.New("traefik: observation api base url must be loopback-only")
	// ErrRouterNotLoaded means Traefik's API does not (yet) know the
	// expected router name under the file provider. This is the "not yet
	// loaded" outcome: transient within the caller's own deadline, per ADR
	// 0005's "Traefik reload is asynchronous".
	ErrRouterNotLoaded = errors.New("traefik: expected router is not loaded")
	// ErrServiceNotLoaded is ErrRouterNotLoaded's service-side counterpart.
	ErrServiceNotLoaded = errors.New("traefik: expected service is not loaded")
	// ErrConfigRejected means Traefik loaded the name but reports a
	// provider/status error against it -- the rendered document itself was
	// syntactically or semantically invalid to Traefik. Retrying the exact
	// same document will fail identically.
	ErrConfigRejected = errors.New("traefik: router or service configuration was rejected")
	// ErrRuleMismatch means the loaded router's rule does not match the
	// exact Host() rule this package would render for the expected route.
	ErrRuleMismatch = errors.New("traefik: loaded router rule does not match expected route")
	// ErrTLSMismatch means the loaded router has no TLS certResolver, or a
	// different one than expected. ADR 0005 requires TLS on every router
	// unconditionally.
	ErrTLSMismatch = errors.New("traefik: loaded router tls certResolver does not match expected route")
	// ErrWrongBackend means the loaded service's load-balancer server URL
	// does not match the expected backend. This is the specific "activation
	// switched to the wrong candidate" failure ADR 0005 warns must never be
	// resolved by blind automatic rollback.
	ErrWrongBackend = errors.New("traefik: loaded service backend does not match expected route")
)

// Observer is a read-only client for Traefik's private HTTP API,
// restricted to loopback base URLs.
type Observer struct {
	baseURL string
	client  *http.Client
}

// NewObserver constructs an Observer against baseURL (e.g.
// "http://127.0.0.1:8080"). It refuses any base URL whose host is not a
// loopback address or "localhost" (ErrNonLoopbackBaseURL), and configures
// the underlying http.Client with no redirect following, no environment
// proxy, and a bounded per-request timeout -- every property this file's
// doc comment and ADR 0005's "private, authenticated management surfaces"
// require, applied once here rather than left to each call site to
// remember.
func NewObserver(baseURL string) (*Observer, error) {
	return newObserver(baseURL, defaultObservationTimeout)
}

// newObserver is NewObserver's implementation, parameterized by timeout so
// client_test.go can exercise the timeout-classification path with a
// millisecond-scale deadline instead of defaultObservationTimeout's real 5
// seconds. There is no exported way to construct an Observer with a
// different timeout -- production code only ever calls NewObserver.
func newObserver(baseURL string, timeout time.Duration) (*Observer, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNonLoopbackBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrNonLoopbackBaseURL, u.Scheme)
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%w: host %q is not loopback", ErrNonLoopbackBaseURL, u.Hostname())
	}

	transport := &http.Transport{
		// No proxy from the environment: this is a fixed, local,
		// loopback-only address, and honoring HTTP_PROXY/HTTPS_PROXY here
		// would let host environment configuration redirect a management
		// API call meant for 127.0.0.1 to an arbitrary proxy.
		Proxy: nil,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// Redirects OFF: a loopback-only, single-purpose management API has
		// no legitimate reason to redirect this request anywhere, and
		// following one could send the (unauthenticated, from this
		// client's perspective) request to an address this client never
		// validated as loopback.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}
	return &Observer{baseURL: strings.TrimRight(baseURL, "/"), client: client}, nil
}

// errRedirectRefused is returned from CheckRedirect to make net/http
// surface the attempted redirect as an error instead of silently following
// it. It is unexported and never returned to a caller directly -- get()
// below detects it via errors.As on the resulting *url.Error and reports
// ErrClassMalformed instead.
var errRedirectRefused = errors.New("traefik: refusing to follow a redirect from the observation api")

// isLoopbackHost reports whether host (already split from any port by
// url.URL.Hostname) is a loopback address or the literal name "localhost".
// It does not perform DNS resolution -- a hostname other than "localhost"
// is rejected outright, even if it happens to resolve to 127.0.0.1 today,
// because that resolution is not this client's to trust or re-verify on
// every call.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// routerAPIResponse and serviceAPIResponse are this client's best-effort
// decode of Traefik's public /api/http/routers/{name} and
// /api/http/services/{name} response shapes (status, error, rule, service,
// tls.certResolver, loadBalancer.servers[].url). They are exercised in this
// step only against an httptest fake built from Traefik's documented
// fields, not a running Traefik instance -- see this package's doc comment
// for why that is the one open risk this step cannot close on macOS.
type routerAPIResponse struct {
	Rule    string   `json:"rule"`
	Service string   `json:"service"`
	Status  string   `json:"status"`
	Error   []string `json:"error"`
	TLS     *struct {
		CertResolver string `json:"certResolver"`
	} `json:"tls"`
}

type serviceAPIResponse struct {
	Status       string   `json:"status"`
	Error        []string `json:"error"`
	LoadBalancer *struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	} `json:"loadBalancer"`
}

// RouterObservation is ObserveRouter's typed result. Loaded distinguishes
// "not yet loaded" (false, no error) from every other outcome.
type RouterObservation struct {
	Loaded          bool
	Rule            string
	Service         string
	TLSCertResolver string
	ConfigErrors    []string
}

// ServiceObservation is ObserveService's typed result.
type ServiceObservation struct {
	Loaded       bool
	BackendURLs  []string
	ConfigErrors []string
}

// get performs one bounded, non-redirecting GET against o.baseURL+path and
// returns the response body (up to maxObservationResponseBytes+1, so the
// caller can detect truncation) and status code, or a typed
// *ObservationError for every transport-layer failure: dial/connect/
// timeout failures and context deadline expiry are ErrClassTransient; an
// oversized body or a refused redirect are ErrClassMalformed. A 404 status
// is returned to the caller as-is (status, nil, nil) -- "not found" is not
// a transport error, callers translate it into their own not-loaded
// result.
func (o *Observer) get(ctx context.Context, op, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+path, nil)
	if err != nil {
		return 0, nil, &ObservationError{Class: ErrClassConfiguration, Op: op, Err: err}
	}
	resp, err := o.client.Do(req)
	if err != nil {
		if isRedirectRefusal(err) {
			return 0, nil, &ObservationError{Class: ErrClassMalformed, Op: op, Err: errRedirectRefused}
		}
		return 0, nil, &ObservationError{Class: ErrClassTransient, Op: op, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxObservationResponseBytes+1))
	if err != nil {
		return 0, nil, &ObservationError{Class: ErrClassTransient, Op: op, Err: err}
	}
	if len(body) > maxObservationResponseBytes {
		return 0, nil, &ObservationError{Class: ErrClassMalformed, Op: op, Err: fmt.Errorf("response body exceeds %d bytes", maxObservationResponseBytes)}
	}
	return resp.StatusCode, body, nil
}

func isRedirectRefusal(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr) && errors.Is(urlErr.Err, errRedirectRefused)
}

// ObserveRouter fetches "<routerName>@file" from Traefik's router API.
func (o *Observer) ObserveRouter(ctx context.Context, routerName string) (RouterObservation, error) {
	status, body, err := o.get(ctx, "observe-router", "/api/http/routers/"+url.PathEscape(routerName+"@file"))
	if err != nil {
		return RouterObservation{}, err
	}
	if status == http.StatusNotFound {
		return RouterObservation{Loaded: false}, nil
	}
	if status < 200 || status >= 300 {
		return RouterObservation{}, &ObservationError{Class: ErrClassTransient, Op: "observe-router", Err: fmt.Errorf("unexpected status %d", status)}
	}
	var decoded routerAPIResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return RouterObservation{}, &ObservationError{Class: ErrClassMalformed, Op: "observe-router", Err: err}
	}
	obs := RouterObservation{
		Loaded:       true,
		Rule:         decoded.Rule,
		Service:      decoded.Service,
		ConfigErrors: decoded.Error,
	}
	if decoded.TLS != nil {
		obs.TLSCertResolver = decoded.TLS.CertResolver
	}
	return obs, nil
}

// ObserveService fetches "<serviceName>@file" from Traefik's service API.
func (o *Observer) ObserveService(ctx context.Context, svcName string) (ServiceObservation, error) {
	status, body, err := o.get(ctx, "observe-service", "/api/http/services/"+url.PathEscape(svcName+"@file"))
	if err != nil {
		return ServiceObservation{}, err
	}
	if status == http.StatusNotFound {
		return ServiceObservation{Loaded: false}, nil
	}
	if status < 200 || status >= 300 {
		return ServiceObservation{}, &ObservationError{Class: ErrClassTransient, Op: "observe-service", Err: fmt.Errorf("unexpected status %d", status)}
	}
	var decoded serviceAPIResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ServiceObservation{}, &ObservationError{Class: ErrClassMalformed, Op: "observe-service", Err: err}
	}
	obs := ServiceObservation{Loaded: true, ConfigErrors: decoded.Error}
	if decoded.LoadBalancer != nil {
		for _, s := range decoded.LoadBalancer.Servers {
			obs.BackendURLs = append(obs.BackendURLs, s.URL)
		}
	}
	return obs, nil
}

// VerifyResult is Verify's outcome when both the router and service are at
// least loaded (Verify returns before constructing this if either is not
// loaded or a transport error occurs -- see Verify's doc comment for the
// exact precedence).
type VerifyResult struct {
	Router  RouterObservation
	Service ServiceObservation
}

// Verify fetches the router and service Render(r) would name and reports
// whether Traefik's API shows exactly what activation authorized: loaded,
// enabled (no config errors), the expected Host() rule, the expected TLS
// certResolver, and the expected backend server URL. It returns (result,
// nil) only when every one of those holds.
//
// On any other outcome it returns a sentinel error, checked in this fixed
// precedence so a caller (and classify.go) always gets the most actionable
// answer first:
//
//  1. A transport-layer failure from either call (*ObservationError) --
//     returned immediately, before any not-loaded/mismatch check, since
//     nothing else below could be evaluated anyway.
//  2. ErrRouterNotLoaded / ErrServiceNotLoaded -- Traefik has not (yet)
//     picked up the file; this is the caller's cue to keep polling within
//     its own deadline, not a router/service comparison failure.
//  3. ErrConfigRejected -- the name loaded, but Traefik itself reports a
//     provider error against it: the rendered document was rejected, not
//     merely different from expected.
//  4. ErrRuleMismatch, then ErrTLSMismatch, then ErrWrongBackend -- the
//     name loaded cleanly but disagrees with the expected route in one
//     specific, named way.
func (o *Observer) Verify(ctx context.Context, r EnvironmentRoute) (VerifyResult, error) {
	if err := r.validate(); err != nil {
		return VerifyResult{}, err
	}
	expectedBackend, err := backendServerURL(r)
	if err != nil {
		return VerifyResult{}, err
	}
	expectedRule := ruleValue(r.Hostname)
	expectedRouter := routerName(r)
	expectedService := serviceName(r)

	routerObs, err := o.ObserveRouter(ctx, expectedRouter)
	if err != nil {
		return VerifyResult{}, err
	}
	svcObs, err := o.ObserveService(ctx, expectedService)
	if err != nil {
		return VerifyResult{}, err
	}

	if !routerObs.Loaded {
		return VerifyResult{Router: routerObs, Service: svcObs}, ErrRouterNotLoaded
	}
	if !svcObs.Loaded {
		return VerifyResult{Router: routerObs, Service: svcObs}, ErrServiceNotLoaded
	}
	if len(routerObs.ConfigErrors) > 0 || len(svcObs.ConfigErrors) > 0 {
		return VerifyResult{Router: routerObs, Service: svcObs}, fmt.Errorf("%w: router=%v service=%v", ErrConfigRejected, routerObs.ConfigErrors, svcObs.ConfigErrors)
	}
	if routerObs.Rule != expectedRule {
		return VerifyResult{Router: routerObs, Service: svcObs}, fmt.Errorf("%w: got %q want %q", ErrRuleMismatch, routerObs.Rule, expectedRule)
	}
	if routerObs.TLSCertResolver == "" || routerObs.TLSCertResolver != r.CertResolverName {
		return VerifyResult{Router: routerObs, Service: svcObs}, fmt.Errorf("%w: got %q want %q", ErrTLSMismatch, routerObs.TLSCertResolver, r.CertResolverName)
	}
	if !containsExact(svcObs.BackendURLs, expectedBackend) {
		return VerifyResult{Router: routerObs, Service: svcObs}, fmt.Errorf("%w: got %v want %q", ErrWrongBackend, svcObs.BackendURLs, expectedBackend)
	}
	return VerifyResult{Router: routerObs, Service: svcObs}, nil
}

func containsExact(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
