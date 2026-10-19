package agenthttp

import (
	"net/http"

	"autodeploy/internal/github"
	"autodeploy/internal/store/postgres"
)

type reconcileRequest struct {
	EnvironmentID string `json:"environment_id"`
	AttemptID     int64  `json:"attempt_id"`
	Fence         int64  `json:"fence"`
	OutcomeDigest string `json:"outcome_digest"`
	// Outcome declares what the block being cleared was: an activation that
	// took effect, an activation that did not, or no activation at all. It is
	// required and has no default: a silent default would let an agent clear
	// an activation block as if nothing had been switched.
	Outcome string `json:"outcome"`
}

func (req reconcileRequest) parse() (digest [32]byte, ok bool) {
	if !github.ValidOpaqueID(req.EnvironmentID) || req.AttemptID <= 0 || req.Fence <= 0 {
		return digest, false
	}
	switch req.Outcome {
	case postgres.ActivationOutcomeActivated, postgres.ActivationOutcomeNotActivated, postgres.ActivationOutcomeNoActivation:
	default:
		return digest, false
	}
	return decodeDigest(req.OutcomeDigest)
}

func (h Handler) handleReconcile(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Reconciler == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req reconcileRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	outcomeDigest, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Reconciler.ReconcileActivation(r.Context(), serverID, digest, req.EnvironmentID, req.AttemptID, req.Fence, req.Outcome, outcomeDigest); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}
