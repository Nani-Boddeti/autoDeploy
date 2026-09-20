package agenthttp

import (
	"context"
	"net/http"

	"autodeploy/internal/deployment"
)

// progressRequest reports that an attempt has entered one execution phase.
// The phase is validated against deployment.AgentReportablePhase before any
// repository call, so an agent cannot name `activating` or `healthy` — those
// are the control plane's own verdicts under ADR 0005, not an agent's to
// claim.
type progressRequest struct {
	AttemptID int64  `json:"attempt_id"`
	Fence     int64  `json:"fence"`
	Phase     string `json:"phase"`
}

func (req progressRequest) parse() (deployment.Status, bool) {
	if req.AttemptID <= 0 || req.Fence <= 0 {
		return "", false
	}
	phase := deployment.Status(req.Phase)
	if !deployment.AgentReportablePhase(phase) {
		return "", false
	}
	return phase, true
}

// handleProgress advances the deployment's visible status as an attempt moves
// through checkout, build, start, and the direct health gate.
//
// A duplicate or already-passed phase is absorbed as success by the
// repository rather than rejected: progress reporting is observability, and a
// report retried after a network failure must never fail an otherwise healthy
// build.
func (h Handler) handleProgress(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.ProgressReporter == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req progressRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	phase, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.ProgressReporter.ReportProgress(r.Context(), serverID, digest, req.AttemptID, req.Fence, phase); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}

// ProgressReporter advances a deployment's status as its attempt enters an
// execution phase. It matches
// (*postgres.AgentLeaseRepository).ReportProgress exactly.
type ProgressReporter interface {
	ReportProgress(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, phase deployment.Status) error
}
