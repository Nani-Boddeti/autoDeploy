package traefik

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const probeTestHost = "app.example.test"

type testPKI struct {
	pool *x509.CertPool
	cert tls.Certificate
}

// newTestPKI issues a leaf for dnsName signed by a fresh CA. A zero
// notAfter means valid for an hour; selfSigned skips the CA (leaf is its own
// issuer and the pool is empty of it).
func newTestPKI(t *testing.T, dnsName string, notBefore, notAfter time.Time) testPKI {
	t.Helper()
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-time.Hour)
	}
	if notAfter.IsZero() {
		notAfter = time.Now().Add(time.Hour)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(2 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return testPKI{pool: pool, cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}
}

type observed struct {
	mu    sync.Mutex
	sni   []string
	hosts []string
	paths []string
}

func startTLSServer(t *testing.T, cert tls.Certificate, h http.Handler) (*httptest.Server, *observed) {
	t.Helper()
	obs := &observed{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs.mu.Lock()
		obs.hosts = append(obs.hosts, r.Host)
		obs.paths = append(obs.paths, r.URL.Path)
		obs.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			obs.mu.Lock()
			obs.sni = append(obs.sni, hi.ServerName)
			obs.mu.Unlock()
			return nil, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, obs
}

func statusHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

func probeCfg(srv *httptest.Server, pool *x509.CertPool) ProbeConfig {
	return ProbeConfig{
		Address:  srv.Listener.Addr().String(),
		Hostname: probeTestHost,
		Timeout:  300 * time.Millisecond,
		Interval: 20 * time.Millisecond,
		rootCAs:  pool,
	}
}

func slowProbeCfg(srv *httptest.Server, pool *x509.CertPool) ProbeConfig {
	cfg := probeCfg(srv, pool)
	cfg.Timeout = 2 * time.Second
	return cfg
}

func TestProbeDataPlaneSuccess(t *testing.T) {
	pki := newTestPKI(t, probeTestHost, time.Time{}, time.Time{})
	srv, obs := startTLSServer(t, pki.cert, statusHandler(200))
	res, err := ProbeDataPlane(context.Background(), slowProbeCfg(srv, pki.pool))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Attempts != 1 || res.LastStatus != 200 || res.Reason != "" {
		t.Fatalf("res = %+v", res)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.sni) != 1 || obs.sni[0] != probeTestHost {
		t.Errorf("sni = %v", obs.sni)
	}
	if len(obs.hosts) != 1 || obs.hosts[0] != probeTestHost {
		t.Errorf("host = %v", obs.hosts)
	}
	if obs.paths[0] != "/" {
		t.Errorf("path = %v", obs.paths)
	}
}

func TestProbeDataPlaneTLSFailuresRetryUntilDeadline(t *testing.T) {
	tests := []struct {
		name string
		pki  testPKI
		pool *x509.CertPool
	}{
		{"wrong SAN", newTestPKI(t, "other.example.test", time.Time{}, time.Time{}), nil},
		{"untrusted", newTestPKI(t, probeTestHost, time.Time{}, time.Time{}), x509.NewCertPool()},
		{"expired", newTestPKI(t, probeTestHost, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := tc.pool
			if pool == nil {
				pool = tc.pki.pool // trusted CA, so only the SAN/expiry is wrong
			}
			srv, _ := startTLSServer(t, tc.pki.cert, statusHandler(200))
			res, err := ProbeDataPlane(context.Background(), probeCfg(srv, pool))
			if !errors.Is(err, ErrProbeTimeout) || !errors.Is(err, ErrProbeTLS) {
				t.Fatalf("err = %v", err)
			}
			if res.Attempts < 1 {
				t.Errorf("attempts = %d", res.Attempts)
			}
			if res.Reason == "" || len(res.Reason) > probeMaxReasonBytes+3 {
				t.Errorf("reason = %q", res.Reason)
			}
		})
	}
}

func TestProbeDataPlaneRetriesThenSucceeds(t *testing.T) {
	pki := newTestPKI(t, probeTestHost, time.Time{}, time.Time{})
	var n atomic.Int32
	srv, _ := startTLSServer(t, pki.cert, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	res, err := ProbeDataPlane(context.Background(), slowProbeCfg(srv, pki.pool))
	if err != nil || res.Attempts != 2 || res.LastStatus != 200 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestProbeDataPlanePersistentStatus(t *testing.T) {
	pki := newTestPKI(t, probeTestHost, time.Time{}, time.Time{})
	srv, _ := startTLSServer(t, pki.cert, statusHandler(503))
	res, err := ProbeDataPlane(context.Background(), slowProbeCfg(srv, pki.pool))
	if !errors.Is(err, ErrProbeTimeout) || !errors.Is(err, ErrProbeStatus) {
		t.Fatalf("err = %v", err)
	}
	if res.LastStatus != 503 || !strings.Contains(err.Error(), "503") {
		t.Errorf("res = %+v err = %v", res, err)
	}
}

func TestProbeDataPlaneRedirectNotFollowed(t *testing.T) {
	pki := newTestPKI(t, probeTestHost, time.Time{}, time.Time{})
	var followed atomic.Bool
	srv, _ := startTLSServer(t, pki.cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	res, err := ProbeDataPlane(context.Background(), probeCfg(srv, pki.pool))
	if err != nil || res.LastStatus != 302 || res.Attempts != 1 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if followed.Load() {
		t.Error("redirect target was requested")
	}
}

func TestProbeDataPlaneConfigRejected(t *testing.T) {
	ok := ProbeConfig{Address: "127.0.0.1:1", Hostname: probeTestHost, Timeout: time.Second}
	tests := map[string]func(*ProbeConfig){
		"remote ip":      func(c *ProbeConfig) { c.Address = "10.0.0.1:443" },
		"localhost name": func(c *ProbeConfig) { c.Address = "localhost:443" },
		"no port":        func(c *ProbeConfig) { c.Address = "127.0.0.1" },
		"empty host":     func(c *ProbeConfig) { c.Hostname = "" },
		"scheme host":    func(c *ProbeConfig) { c.Hostname = "https://a.example" },
		"port host":      func(c *ProbeConfig) { c.Hostname = "a.example:443" },
		"path host":      func(c *ProbeConfig) { c.Hostname = "a.example/x" },
		"ip host":        func(c *ProbeConfig) { c.Hostname = "127.0.0.1" },
		"port zero":      func(c *ProbeConfig) { c.Address = "127.0.0.1:0" },
		"port empty":     func(c *ProbeConfig) { c.Address = "127.0.0.1:" },
		"port abc":       func(c *ProbeConfig) { c.Address = "127.0.0.1:abc" },
		"neg attempt":    func(c *ProbeConfig) { c.AttemptTimeout = -1 },
		"zero timeout":   func(c *ProbeConfig) { c.Timeout = 0 },
		"bad path":       func(c *ProbeConfig) { c.Path = "x" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := ok
			mutate(&cfg)
			res, err := ProbeDataPlane(context.Background(), cfg)
			if !errors.Is(err, ErrProbeConfig) || res.Attempts != 0 {
				t.Fatalf("res = %+v err = %v", res, err)
			}
		})
	}
	// ::1 literal is accepted by validation.
	cfg := ok
	cfg.Address = "[::1]:443"
	if _, err := normalizeProbeConfig(cfg); err != nil {
		t.Errorf("::1 rejected: %v", err)
	}
}

func TestProbeDataPlaneContextCancel(t *testing.T) {
	pki := newTestPKI(t, probeTestHost, time.Time{}, time.Time{})
	srv, _ := startTLSServer(t, pki.cert, statusHandler(503))
	cfg := probeCfg(srv, pki.pool)
	cfg.Timeout = 30 * time.Second
	cfg.Interval = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := ProbeDataPlane(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestProbeDataPlaneStalledListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	cfg := ProbeConfig{
		Address: ln.Addr().String(), Hostname: probeTestHost,
		Timeout: 400 * time.Millisecond, Interval: 20 * time.Millisecond,
		AttemptTimeout: 100 * time.Millisecond,
	}
	start := time.Now()
	res, err := ProbeDataPlane(context.Background(), cfg)
	if !errors.Is(err, ErrProbeTimeout) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if res.Attempts < 1 || time.Since(start) > 2*time.Second {
		t.Errorf("attempts = %d elapsed = %v", res.Attempts, time.Since(start))
	}
}

func TestProbeDataPlaneIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	pki := newTestPKI(t, probeTestHost, time.Time{}, time.Time{})
	srv, _ := startTLSServer(t, pki.cert, statusHandler(200))
	if _, err := ProbeDataPlane(context.Background(), slowProbeCfg(srv, pki.pool)); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestBoundReason(t *testing.T) {
	if got := boundReason("a\r\nb\x00c"); got != "a  b c" {
		t.Errorf("control chars: %q", got)
	}
	// Multi-byte rune straddling the cap must not leave invalid UTF-8.
	in := strings.Repeat("a", probeMaxReasonBytes-1) + "é" + "tail"
	got := boundReason(in)
	if !utf8.ValidString(got) || len(got) > probeMaxReasonBytes+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("cap: len=%d %q", len(got), got)
	}
	if got := boundReason("short"); got != "short" {
		t.Errorf("short: %q", got)
	}
}
