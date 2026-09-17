package agenthttp

import (
	"net/http"

	"autodeploy/internal/queue"
)

type renewRequest struct {
	AttemptID int64 `json:"attempt_id"`
	Fence     int64 `json:"fence"`
	Sequence  int   `json:"sequence"`
}

func (req renewRequest) valid() bool {
	return req.AttemptID > 0 && req.Fence > 0 && req.Sequence >= 0
}

type renewResponse struct {
	LeaseExpiresAt     string `json:"lease_expires_at"`
	AuthorityExpiresAt string `json:"authority_expires_at"`
	CancelRequested    bool   `json:"cancel_requested"`
}

func (h Handler) handleRenew(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Renewer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req renewRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := h.Renewer.Renew(r.Context(), serverID, digest, req.AttemptID, req.Fence, req.Sequence)
	if err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, renewResponse{
		LeaseExpiresAt:     formatDeadline(result.LeaseExpiresAt),
		AuthorityExpiresAt: formatDeadline(result.AuthorityExpiresAt),
		CancelRequested:    result.CancelRequested,
	})
}

// completeRequest's Outcome/FailureClass strings are validated against the
// same closed enums internal/queue defines; an unrecognized value is
// rejected as invalid input before any repository call, matching
// ErrAgentLeaseInvalidInput's own validation (kept here too so a malformed
// request never even reaches that repository round trip).
type completeRequest struct {
	AttemptID    int64  `json:"attempt_id"`
	Fence        int64  `json:"fence"`
	Outcome      string `json:"outcome"`
	FailureClass string `json:"failure_class,omitempty"`
	OperationID  string `json:"operation_id"`
	ResultDigest string `json:"result_digest"`
}

func (req completeRequest) parse() (outcome queue.CompletionOutcome, failureClass queue.FailureClass, digest [32]byte, ok bool) {
	if req.AttemptID <= 0 || req.Fence <= 0 || !validOperationID(req.OperationID) {
		return "", "", digest, false
	}
	outcome = queue.CompletionOutcome(req.Outcome)
	switch outcome {
	case queue.CompletionSucceeded:
		if req.FailureClass != "" {
			return "", "", digest, false
		}
	case queue.CompletionFailed:
		failureClass = queue.FailureClass(req.FailureClass)
		if !failureClass.Valid() {
			return "", "", digest, false
		}
	default:
		return "", "", digest, false
	}
	digest, ok = decodeDigest(req.ResultDigest)
	if !ok {
		return "", "", digest, false
	}
	return outcome, failureClass, digest, true
}

// validOperationID mirrors agent_job_attempts.result_operation_id's CHECK
// (octet_length BETWEEN 1 AND 128), matching
// internal/store/postgres/agent_lease_repository.go's own validOperationID
// so a malformed operation ID is rejected here rather than round-tripping
// to the database first.
func validOperationID(id string) bool {
	n := len(id)
	return n >= 1 && n <= 128
}

func (h Handler) handleComplete(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Completer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req completeRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	outcome, failureClass, resultDigest, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Completer.Complete(r.Context(), serverID, digest, req.AttemptID, req.Fence, outcome, failureClass, req.OperationID, resultDigest); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}

type cancelAckRequest struct {
	AttemptID    int64  `json:"attempt_id"`
	Fence        int64  `json:"fence"`
	OperationID  string `json:"operation_id"`
	ResultDigest string `json:"result_digest"`
}

func (req cancelAckRequest) parse() (digest [32]byte, ok bool) {
	if req.AttemptID <= 0 || req.Fence <= 0 || !validOperationID(req.OperationID) {
		return digest, false
	}
	return decodeDigest(req.ResultDigest)
}

func (h Handler) handleCancelAck(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Canceller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req cancelAckRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	resultDigest, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Canceller.AcknowledgeCancellation(r.Context(), serverID, digest, req.AttemptID, req.Fence, req.OperationID, resultDigest); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}
