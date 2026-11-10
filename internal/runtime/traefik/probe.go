package traefik

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"autodeploy/internal/healthgate"
)

// This file is the data-plane half of ADR 0005's activation proof: after the
// agent has written a route and Traefik's API reports it loaded (Observer),
// ProbeDataPlane proves Traefik actually *serves* the new route over real
// HTTPS. It dials the local Traefik listener directly (never DNS), presents
// the route hostname as TLS SNI, verifies the served certificate chain and
// hostname in full, and sends Host = hostname.

// Sentinel errors returned (wrapped) by ProbeDataPlane.
var (
	// ErrProbeConfig is returned immediately, before any network activity,
	// when the ProbeConfig is invalid.
	ErrProbeConfig = errors.New("traefik: invalid data-plane probe config")

	// ErrProbeTLS marks a certificate or handshake failure. During first-deploy
	// ACME issuance Traefik serves its default self-signed certificate, so
	// this is retried until the deadline rather than treated as fatal.
	ErrProbeTLS = errors.New("traefik: data-plane probe tls verification failed")

	// ErrProbeStatus marks a final response outside the accepted 200-399 range.
	ErrProbeStatus = errors.New("traefik: data-plane probe got an unaccepted status")

	// ErrProbeTimeout is returned when the overall deadline elapsed without a
	// served response. It wraps the last attempt's failure (ErrProbeTLS or
	// ErrProbeStatus when applicable) and carries its short reason.
	ErrProbeTimeout = errors.New("traefik: data-plane probe deadline exhausted")
)

const (
	defaultProbeInterval  = 2 * time.Second
	defaultAttemptTimeout = 5 * time.Second
	// probeMinAttempt: below this remaining time a new attempt is not started.
	probeMinAttempt     = 50 * time.Millisecond
	defaultProbePath    = "/"
	probeMaxBodyBytes   = 4 << 10
	probeMaxReasonBytes = 200
)

// ProbeConfig configures ProbeDataPlane.
type ProbeConfig struct {
	// Address is the local Traefik HTTPS listener as host:port. Required. The
	// host must be a literal loopback IP (127.0.0.0/8 or ::1); "localhost"
	// and every non-loopback address are refused so the probe can never be
	// pointed at a remote host.
	Address string
	// Hostname is the TLS SNI, the certificate verification name and the Host
	// header. Required: a bare DNS name, no scheme, port, path or IP literal.
	Hostname string
	// Path is the request path. Defaults to "/".
	Path string
	// Timeout is the overall deadline for the whole poll. Required > 0.
	Timeout time.Duration
	// Interval is the pause between attempts. Defaults to 2s; the final wait
	// is capped by the remaining deadline.
	Interval time.Duration
	// AttemptTimeout bounds one attempt (capped by the remaining deadline) so
	// a stalled listener cannot consume the whole poll. Defaults to 5s.
	AttemptTimeout time.Duration

	// rootCAs is a test seam: nil means the system roots.
	rootCAs *x509.CertPool
}

// ProbeResult describes the poll outcome. It never contains response bodies.
type ProbeResult struct {
	// Attempts is the number of requests started.
	Attempts int
	// LastStatus is the last HTTP status received, 0 if none was.
	LastStatus int
	// Reason is a short (bounded) description of the last failure; empty on
	// success. Suitable for an activation failure summary.
	Reason string
}

// ProbeDataPlane polls the local Traefik HTTPS listener until a response in
// 200-399 arrives or cfg.Timeout elapses. Any status 200-399 counts as served
// (the application's own health path is unknown here); redirects are not
// followed, so a 3xx proves only that Traefik routed to the backend.
//
// Only 200-399 is accepted, so the caller must pick a Path that returns 2xx or
// 3xx without authentication. A caller ctx deadline or cancel returns the ctx
// error (wrapped), not ErrProbeTimeout; only cfg.Timeout yields ErrProbeTimeout.
//
// Every attempt failure kind is retried until the deadline. On exhaustion the
// error wraps ErrProbeTimeout plus ErrProbeTLS or ErrProbeStatus when that
// was the last failure. Context cancellation returns promptly with ctx.Err()
// wrapped. A bad config returns ErrProbeConfig without touching the network.
func ProbeDataPlane(ctx context.Context, cfg ProbeConfig) (ProbeResult, error) {
	cfg, err := normalizeProbeConfig(cfg)
	if err != nil {
		return ProbeResult{}, err
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	client := newProbeClient(cfg)
	defer client.CloseIdleConnections()
	target := "https://" + cfg.Hostname + cfg.Path

	var (
		res  ProbeResult
		last *probeFailure
	)
	for {
		remaining := time.Until(deadline(deadlineCtx))
		if res.Attempts > 0 && remaining < probeMinAttempt {
			return res, timeoutError(last)
		}
		attemptCtx, attemptCancel := context.WithTimeout(deadlineCtx, min(cfg.AttemptTimeout, remaining))
		res.Attempts++
		status, fail := probeOnce(attemptCtx, client, target)
		attemptCancel()
		if status != 0 {
			res.LastStatus = status
		}
		if fail == nil {
			res.Reason = ""
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("traefik: data-plane probe canceled: %w", err)
		}
		if fail.timedOut {
			// A timed-out attempt never masks an earlier, more informative
			// failure, but is recorded when nothing better exists.
			if last == nil || deadlineCtx.Err() == nil {
				fail = &probeFailure{reason: "attempt timed out"}
			} else {
				fail = last
			}
		}
		last = fail
		res.Reason = last.reason

		wait := min(cfg.Interval, time.Until(deadline(deadlineCtx)))
		if deadlineCtx.Err() != nil || wait <= 0 {
			return res, timeoutError(last)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return res, fmt.Errorf("traefik: data-plane probe canceled: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func deadline(ctx context.Context) time.Time {
	dl, _ := ctx.Deadline()
	return dl
}

// probeFailure is one classified attempt failure. kind is ErrProbeTLS,
// ErrProbeStatus or nil; reason is built from the inner cause only so the
// bounded budget holds the useful text.
type probeFailure struct {
	kind     error
	reason   string
	timedOut bool
}

func timeoutError(last *probeFailure) error {
	switch {
	case last == nil:
		return ErrProbeTimeout
	case last.kind != nil:
		return fmt.Errorf("%w: %w: %s", ErrProbeTimeout, last.kind, last.reason)
	default:
		return fmt.Errorf("%w: %s", ErrProbeTimeout, last.reason)
	}
}

func normalizeProbeConfig(cfg ProbeConfig) (ProbeConfig, error) {
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return cfg, fmt.Errorf("%w: address must be a loopback ip host:port", ErrProbeConfig)
	}
	if n, perr := strconv.ParseUint(port, 10, 16); perr != nil || n == 0 {
		return cfg, fmt.Errorf("%w: address port must be numeric 1-65535", ErrProbeConfig)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return cfg, fmt.Errorf("%w: address host must be a literal loopback ip", ErrProbeConfig)
	}
	if err := validateProbeHostname(cfg.Hostname); err != nil {
		return cfg, err
	}
	if cfg.Path == "" {
		cfg.Path = defaultProbePath
	}
	if !strings.HasPrefix(cfg.Path, "/") || strings.ContainsAny(cfg.Path, " \t\r\n#") {
		return cfg, fmt.Errorf("%w: path must start with / and contain no whitespace or fragment", ErrProbeConfig)
	}
	if cfg.Timeout <= 0 {
		return cfg, fmt.Errorf("%w: timeout must be positive", ErrProbeConfig)
	}
	if cfg.Interval < 0 {
		return cfg, fmt.Errorf("%w: interval must not be negative", ErrProbeConfig)
	}
	if cfg.AttemptTimeout < 0 {
		return cfg, fmt.Errorf("%w: attempt timeout must not be negative", ErrProbeConfig)
	}
	if cfg.AttemptTimeout == 0 {
		cfg.AttemptTimeout = defaultAttemptTimeout
	}
	if cfg.Interval == 0 {
		cfg.Interval = defaultProbeInterval
	}
	return cfg, nil
}

// validateProbeHostname applies the same canonical rule the route itself is
// validated by (healthgate.ValidRouteHostname), so the probe can never refuse a
// hostname the control plane was allowed to authorize.
func validateProbeHostname(h string) error {
	if !healthgate.ValidRouteHostname(h) {
		return fmt.Errorf("%w: hostname %q is not a valid canonical route hostname", ErrProbeConfig, h)
	}
	return nil
}

func newProbeClient(cfg ProbeConfig) *http.Client {
	dialer := &net.Dialer{}
	tr := &http.Transport{
		Proxy: nil,
		// Always dial the configured loopback listener, whatever the URL host.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, cfg.Address)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: cfg.Hostname,
			RootCAs:    cfg.rootCAs,
		},
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// probeOnce performs one attempt and returns the status (0 if no response)
// and a classified failure, nil when the status is within 200-399.
func probeOnce(ctx context.Context, client *http.Client, target string) (int, *probeFailure) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, &probeFailure{reason: boundReason("build request: " + err.Error())}
	}
	req.Header.Set("User-Agent", "autodeploy-probe")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, &probeFailure{timedOut: true, reason: "attempt timed out"}
		}
		cause := boundReason(unwrapURLError(err).Error())
		if isTLSFailure(err) {
			return 0, &probeFailure{kind: ErrProbeTLS, reason: cause}
		}
		return 0, &probeFailure{reason: cause}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeMaxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return resp.StatusCode, &probeFailure{kind: ErrProbeStatus, reason: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return resp.StatusCode, nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func isTLSFailure(err error) bool {
	var (
		certErr   *tls.CertificateVerificationError
		unknownCA x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		invalid   x509.CertificateInvalidError
		alert     tls.AlertError
		recHdr    tls.RecordHeaderError
	)
	if errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) ||
		errors.As(err, &invalid) || errors.As(err, &alert) || errors.As(err, &recHdr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:")
}

func boundReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > probeMaxReasonBytes {
		s = strings.ToValidUTF8(s[:probeMaxReasonBytes], "") + "..."
	}
	return s
}
