package agenthttp

import (
	"context"
	"net/http"
	"unicode/utf8"

	"autodeploy/internal/github"
	"autodeploy/internal/store/postgres"
)

// DrainResultRecorder matches (*postgres.AgentLeaseRepository).RecordDrainResult
// exactly, so production wiring passes the repository directly.
type DrainResultRecorder interface {
	RecordDrainResult(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID int64, predecessorReleaseID, predecessorRuntimeInstanceID, outcome, summary string) error
}

// drainResultRequest is the wire shape of POST /agent/v1/drains/result
// (step 20B): what the agent's predecessor drain for the activating attempt
// ended in. There is deliberately no lease or fence: the attempt is closed by
// then and the store derives and verifies the authority from the attempt's own
// succeeded activate operation.
type drainResultRequest struct {
	AttemptID                    int64  `json:"attempt_id"`
	PredecessorReleaseID         string `json:"predecessor_release_id"`
	PredecessorRuntimeInstanceID string `json:"predecessor_runtime_instance_id"`
	Outcome                      string `json:"outcome"`
	Summary                      string `json:"summary"`
}

func (req drainResultRequest) valid() bool {
	if req.AttemptID <= 0 || !github.ValidOpaqueID(req.PredecessorReleaseID) || req.PredecessorRuntimeInstanceID == "" ||
		len(req.PredecessorRuntimeInstanceID) > 19 || len(req.Summary) > 2048 || !utf8.ValidString(req.Summary) {
		return false
	}
	switch req.Outcome {
	case postgres.DrainOutcomeStopped, postgres.DrainOutcomeAlreadyStopped, postgres.DrainOutcomeGone,
		postgres.DrainOutcomeStopFailed, postgres.DrainOutcomeRouteMoved, postgres.DrainOutcomeIdentityError:
		return true
	}
	return false
}

// handleDrainResult serves POST /agent/v1/drains/result. A nil recorder
// answers 503 before touching anything. Replays of an identical report answer
// 200; a different payload for the same attempt is 409 integrity_conflict.
func (h Handler) handleDrainResult(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.DrainRecorder == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req drainResultRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.DrainRecorder.RecordDrainResult(r.Context(), serverID, digest, req.AttemptID, req.PredecessorReleaseID, req.PredecessorRuntimeInstanceID, req.Outcome, req.Summary); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}
