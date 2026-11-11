package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// LogChunk is one shipped log chunk (stream allowlist and size rules are the
// server's; see internal/agent/http/logs.go).
type LogChunk struct {
	Stream string `json:"stream"`
	Seq    int64  `json:"seq"`
	Body   string `json:"body"`
}

// AppendLogsRequest is one batch for one attempt.
type AppendLogsRequest struct {
	AttemptID   int64      `json:"attempt_id"`
	Fence       int64      `json:"fence"`
	WriterEpoch int64      `json:"writer_epoch"`
	Chunks      []LogChunk `json:"chunks"`
}

// AppendLogsResult is the server's accounting for a successful append.
type AppendLogsResult struct {
	Accepted   int  `json:"accepted"`
	Duplicates int  `json:"duplicates"`
	Conflicts  int  `json:"conflicts"`
	Dropped    int  `json:"dropped"`
	Marker     bool `json:"marker"`
}

// LogOutcome classifies an AppendLogs result for the shipper, which owns
// retry policy.
type LogOutcome int

const (
	// LogSuccess: the batch is durably handled (accepted, duplicate or
	// dropped at a cap); do not resend it.
	LogSuccess LogOutcome = iota
	// LogRetryable: transport failure, 503, 429 or another 5xx; the same
	// batch may be resent (appends are idempotent on stream/epoch/seq).
	LogRetryable
	// LogPermanent: the server refused the batch (400, 401/403/404, 409,
	// 413) or answered 200 with a malformed body; resending cannot help and
	// the shipper should stop shipping for that attempt.
	LogPermanent
)

// errLogRejected is a 403/404 answer: refused, and will be refused again. It
// is local to AppendLogs; the shared mapStatus (and so every other route's
// retry behaviour) is deliberately untouched.
var errLogRejected = errors.New("control plane rejected the request")

// ClassifyLogError maps an AppendLogs error to its outcome class.
func ClassifyLogError(err error) LogOutcome {
	switch {
	case err == nil:
		return LogSuccess
	case errors.Is(err, ErrServiceUnavailable), errors.Is(err, ErrTransportFailure):
		return LogRetryable
	default:
		// ErrUnauthorized, ErrInvalidRequest, errLogRejected, ErrStaleLease,
		// ErrIntegrityConflict, ErrMalformedResponse and anything unknown.
		return LogPermanent
	}
}

// AppendLogs sends one batch with exactly ONE HTTP attempt under the caller's
// ctx (plus the client's 10 s per-request timeout): unlike post it has no
// retry loop, because the shipper owns retry and must not be held up at exit.
// Classify the error with ClassifyLogError. A 200 whose body does not strictly
// match the contract yields ErrMalformedResponse (not retried: the append very
// likely committed); a 200 that is not JSON at all is reported by the shared
// transport layer as ErrTransportFailure (retryable; appends are idempotent).
func (c *Client) AppendLogs(ctx context.Context, req AppendLogsRequest) (AppendLogsResult, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return AppendLogsResult{}, fmt.Errorf("encode request: %w", err)
	}
	raw, err := c.postLogsOnce(ctx, payload)
	if err != nil {
		return AppendLogsResult{}, err
	}
	var res AppendLogsResult
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil || res.Accepted < 0 || res.Duplicates < 0 || res.Conflicts < 0 || res.Dropped < 0 {
		return AppendLogsResult{}, fmt.Errorf("%w: logs response", ErrMalformedResponse)
	}
	return res, nil
}

// postLogsOnce performs the single HTTP attempt. It mirrors Client.attempt but
// classifies 403/404 as permanent locally, delegating every other status to
// the shared mapStatus.
func (c *Client) postLogsOnce(ctx context.Context, payload []byte) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+pathLogs, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrTransportFailure)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token.String())
	req.Header.Set("X-AutoDeploy-Server-ID", c.serverID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransportFailure, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrTransportFailure)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response too large", ErrTransportFailure)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("%w: status %d", errLogRejected, resp.StatusCode)
	default:
		return nil, c.mapStatus(resp.StatusCode, body)
	}
}
