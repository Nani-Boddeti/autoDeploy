// Package agentclient is the host agent's control-plane HTTP client and
// lease-handling orchestration (tasks/todo.md Step 6c; docs/adr/0001
// control-plane/agent separation; docs/adr/0004 fenced lease protocol). It
// talks only to internal/agent/http's exact wire contracts: TLS required,
// bounded/strictly-decoded responses, retried only for 503/network
// failures, and it never logs a raw credential, digest, or token.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"
	"autodeploy/internal/server"
)

const (
	pathEnroll      = "/agent/v1/enroll"
	pathHeartbeat   = "/agent/v1/heartbeat"
	pathClaim       = "/agent/v1/jobs/claim"
	pathRenew       = "/agent/v1/leases/renew"
	pathComplete    = "/agent/v1/leases/complete"
	pathCancelAck   = "/agent/v1/leases/cancel-ack"
	pathReconcile   = "/agent/v1/environments/reconcile"
	pathMaterialize = "/agent/v1/leases/materialize-secrets"

	requestTimeout   = 10 * time.Second
	maxResponseBytes = 64 * 1024
	maxRetries       = 5

	// retryBaseDelay/retryCapDelay bound this client's own full-jitter HTTP
	// retry backoff. This is deliberately a separate, much faster policy
	// than internal/queue.FullJitterDelay's job-retry schedule (base 5s,
	// cap 5m): that policy paces database job retries across minutes, while
	// a single control-plane request must resolve well inside the 90s lease
	// authority window and the 20s heartbeat cadence.
	retryBaseDelay = 200 * time.Millisecond
	retryCapDelay  = 5 * time.Second
)

// httpRetryDelay returns one full-jitter delay for the attempt'th HTTP
// retry (1-based): uniform(0, min(retryCapDelay, retryBaseDelay*2^(attempt-1))).
func httpRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 10 {
		shift = 10
	}
	backoff := retryBaseDelay * time.Duration(uint64(1)<<uint(shift))
	if backoff > retryCapDelay || backoff <= 0 {
		backoff = retryCapDelay
	}
	return time.Duration(rand.Float64() * float64(backoff))
}

// Sentinel errors this package's callers switch on. Only ErrServiceUnavailable
// and ErrTransportFailure are retried; every other outcome -- including
// ErrUnauthorized -- is returned to the caller immediately.
var (
	ErrUnauthorized       = errors.New("agent credential rejected")
	ErrStaleLease         = errors.New("stale lease")
	ErrIntegrityConflict  = errors.New("integrity conflict")
	ErrInvalidRequest     = errors.New("invalid request")
	ErrServiceUnavailable = errors.New("control plane unavailable")
	ErrTransportFailure   = errors.New("control plane request failed")
)

// Client is a narrow, testable wrapper around one control-plane origin and
// one server identity. Enroll deliberately never uses the credential/
// Authorization header (internal/agent/http's enroll route has none); every
// other method always authenticates.
type Client struct {
	http     *http.Client
	baseURL  string // canonical https origin, no trailing slash or path
	serverID string
	token    server.CredentialToken
	logger   *log.Logger
}

// NewClient validates baseURL (HTTPS required; see auth.CanonicalHTTPSOrigin)
// and serverID before any network use. token may be empty when the Client
// is only used for Enroll.
func NewClient(baseURL, serverID string, token server.CredentialToken, httpClient *http.Client, logger *log.Logger) (*Client, error) {
	origin, err := auth.CanonicalHTTPSOrigin(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid control-plane base URL: %w", err)
	}
	if !github.ValidOpaqueID(serverID) {
		return nil, errors.New("invalid server id")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	// Never follow redirects: net/http keeps Authorization on same-host
	// redirects, so an https->http redirect would resend the credential in
	// cleartext. A copy keeps a caller-supplied client unmodified.
	noRedirect := *httpClient
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	httpClient = &noRedirect
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Client{http: httpClient, baseURL: origin, serverID: serverID, token: token, logger: logger}, nil
}

// post marshals body, POSTs it to c.baseURL+path, and strictly decodes a
// 200 response into out (nil to discard an empty body). It retries only
// ErrServiceUnavailable and ErrTransportFailure (503s and network/transport
// failures) with full-jitter backoff (httpRetryDelay); 401 and every other
// 4xx never retry.
func (c *Client) post(ctx context.Context, path string, body, out any, authenticate bool) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		lastErr = c.attempt(ctx, path, payload, out, authenticate)
		if lastErr == nil {
			return nil
		}
		if !errors.Is(lastErr, ErrServiceUnavailable) && !errors.Is(lastErr, ErrTransportFailure) {
			return lastErr
		}
		if attempt == maxRetries {
			break
		}
		select {
		case <-time.After(httpRetryDelay(attempt)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}

func (c *Client) attempt(ctx context.Context, path string, payload []byte, out any, authenticate bool) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: build request", ErrTransportFailure)
	}
	req.Header.Set("Content-Type", "application/json")
	if authenticate {
		req.Header.Set("Authorization", "Bearer "+c.token.String())
		req.Header.Set("X-AutoDeploy-Server-ID", c.serverID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A network/transport error never carries request header/body
		// content in its own text, so it is safe to wrap directly.
		return fmt.Errorf("%w: %v", ErrTransportFailure, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read response", ErrTransportFailure)
	}
	if len(respBody) > maxResponseBytes {
		return fmt.Errorf("%w: response too large", ErrTransportFailure)
	}
	if resp.StatusCode != http.StatusOK {
		return c.mapStatus(resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(respBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: decode response", ErrTransportFailure)
	}
	return nil
}

type wireError struct {
	Error string `json:"error"`
}

func (c *Client) mapStatus(status int, body []byte) error {
	var parsed wireError
	_ = json.Unmarshal(body, &parsed) // best effort; a malformed body still maps purely by status below
	switch status {
	case http.StatusUnauthorized:
		c.logger.Print("agent credential rejected")
		return ErrUnauthorized
	case http.StatusConflict:
		if parsed.Error == "stale_lease" {
			return ErrStaleLease
		}
		return ErrIntegrityConflict
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return ErrInvalidRequest
	case http.StatusServiceUnavailable:
		return ErrServiceUnavailable
	default:
		return fmt.Errorf("%w: status %d", ErrTransportFailure, status)
	}
}
