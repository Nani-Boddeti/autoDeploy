package agenthttp

import (
	"context"
	"net/http"

	"autodeploy/internal/store/postgres"
)

const (
	// MaxLogBodyBytes bounds one /leases/logs request body. It is the only
	// route above the global MaxBodyBytes: 32 chunks of up to 4 KiB cannot fit
	// in 16 KiB, and the shipper batches at most ~24 KiB. It is applied by
	// handleLogs alone (readJSON takes the cap per call).
	MaxLogBodyBytes = 64 * 1024
	// maxLogChunksPerRequest mirrors the store's per-append bound.
	maxLogChunksPerRequest = postgres.MaxLogChunksPerAppend
)

var logRequestStreams = map[string]struct{}{"build": {}, "phase": {}, "health": {}, "activation": {}, "drain": {}, "system": {}}

type logChunkRequest struct {
	Stream string `json:"stream"`
	Seq    int64  `json:"seq"`
	Body   string `json:"body"`
}

type logsRequest struct {
	AttemptID   int64             `json:"attempt_id"`
	Fence       int64             `json:"fence"`
	WriterEpoch int64             `json:"writer_epoch"`
	Chunks      []logChunkRequest `json:"chunks"`
}

type logsResponse struct {
	Accepted   int  `json:"accepted"`
	Duplicates int  `json:"duplicates"`
	Conflicts  int  `json:"conflicts"`
	Dropped    int  `json:"dropped"`
	Marker     bool `json:"marker"`
}

// valid checks shape only; secrets and control characters are handled by
// sanitizeLogBody afterwards.
func (req logsRequest) valid() bool {
	if req.AttemptID <= 0 || req.Fence <= 0 || req.WriterEpoch <= 0 ||
		len(req.Chunks) == 0 || len(req.Chunks) > maxLogChunksPerRequest {
		return false
	}
	for _, c := range req.Chunks {
		if _, ok := logRequestStreams[c.Stream]; !ok || c.Seq <= 0 ||
			len(c.Body) < 1 || len(c.Body) > postgres.MaxLogChunkBytes {
			return false
		}
	}
	return true
}

// handleLogs appends agent-shipped deployment log chunks (step 15g). It
// authenticates exactly like progress; every authorization failure (unknown
// attempt, another server's attempt, wrong fence, closed beyond the window)
// reaches mapError as ErrServerAuthorizationDenied and so yields the same
// generic 401 without revealing which check failed.
//
// Each body passes sanitizeLogBody before the store. A chunk that is empty
// after sanitizing (control characters only) is dropped, not stored: the
// store rejects empty bodies, and failing the batch for it would make the
// shipper stop. Dropped chunks count toward "dropped" in the response; a
// request whose chunks all drop answers 200 without touching the store.
func (h Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.LogAppender == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req logsRequest
	if !h.readJSON(w, r, MaxLogBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	chunks := make([]postgres.LogChunkInput, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		body := sanitizeLogBody(c.Body)
		if body == "" {
			continue
		}
		chunks = append(chunks, postgres.LogChunkInput{Stream: c.Stream, Seq: c.Seq, Body: body})
	}
	skipped := len(req.Chunks) - len(chunks)
	if len(chunks) == 0 {
		// Nothing left to store: refuse like any other pre-store validation
		// failure rather than answering 200 for a request that did nothing.
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	res, err := h.LogAppender.AppendLogChunks(r.Context(), serverID, digest, req.AttemptID, req.Fence, req.WriterEpoch, chunks)
	if err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, logsResponse{
		Accepted: res.Accepted, Duplicates: res.Duplicates, Conflicts: res.Conflicts,
		Dropped: res.Dropped + skipped, Marker: res.Marker,
	})
}

// LogAppender matches (*postgres.DeploymentLogRepository).AppendLogChunks
// exactly.
type LogAppender interface {
	AppendLogChunks(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence, writerEpoch int64, chunks []postgres.LogChunkInput) (postgres.AppendLogResult, error)
}
