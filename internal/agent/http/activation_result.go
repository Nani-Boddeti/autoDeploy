package agenthttp

import (
	"context"
	"net/http"
	"strings"
)

// maxActivationFailureSummaryBytes bounds the free-text failure summary an
// agent may send; the repository sanitizes and truncates it further before
// it reaches release_operations.outcome_summary.
const maxActivationFailureSummaryBytes = 2048

// ActivationResultReporter matches
// (*postgres.AgentLeaseRepository).ReportActivationResult's signature
// exactly, so production wiring can pass that repository directly without an
// adapter.
type ActivationResultReporter interface {
	ReportActivationResult(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, operationID string, succeeded bool, observedRouteDigest [32]byte, failureSummary string) error
}

// activationResultRequest is tx2's wire shape. On success the agent echoes
// the route digest it observed (the control plane compares it with the one
// it rendered in tx1); on failure or ambiguity it sends a failure_summary
// instead and no digest.
type activationResultRequest struct {
	AttemptID           int64  `json:"attempt_id"`
	Fence               int64  `json:"fence"`
	OperationID         string `json:"operation_id"`
	Succeeded           *bool  `json:"succeeded"`
	ObservedRouteDigest string `json:"observed_route_digest,omitempty"`
	FailureSummary      string `json:"failure_summary,omitempty"`
}

func (req activationResultRequest) parse() (attemptID, fence int64, operationID string, succeeded bool, digest [32]byte, summary string, ok bool) {
	// A missing "succeeded" must not default to a declared failure: that
	// would durably block the environment on a malformed body.
	if req.Succeeded == nil || req.AttemptID <= 0 || req.Fence <= 0 || !validOperationID(req.OperationID) || len(req.FailureSummary) > maxActivationFailureSummaryBytes {
		return 0, 0, "", false, digest, "", false
	}
	if *req.Succeeded {
		if req.FailureSummary != "" {
			return 0, 0, "", false, digest, "", false
		}
		// tx1 returns the route digest as "sha256:<hex>"; accept that form
		// or bare hex (every other digest on this wire).
		digest, ok = decodeDigest(strings.TrimPrefix(req.ObservedRouteDigest, "sha256:"))
		if !ok {
			return 0, 0, "", false, digest, "", false
		}
		return req.AttemptID, req.Fence, req.OperationID, true, digest, "", true
	}
	if req.ObservedRouteDigest != "" {
		return 0, 0, "", false, digest, "", false
	}
	return req.AttemptID, req.Fence, req.OperationID, false, digest, req.FailureSummary, true
}

// handleActivationResult serves POST /agent/v1/leases/activation-result
// (Step 10c tx2). A nil ActivationReporter answers 503 before touching any
// repository: this is the activationEnabled gate, exactly as handleActivate.
func (h Handler) handleActivationResult(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.ActivationReporter == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req activationResultRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	attemptID, fence, operationID, succeeded, routeDigest, summary, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.ActivationReporter.ReportActivationResult(r.Context(), serverID, digest, attemptID, fence, operationID, succeeded, routeDigest, summary); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}
