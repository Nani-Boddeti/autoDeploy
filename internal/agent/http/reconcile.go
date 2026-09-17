package agenthttp

import (
	"net/http"

	"autodeploy/internal/github"
)

type reconcileRequest struct {
	EnvironmentID string `json:"environment_id"`
	AttemptID     int64  `json:"attempt_id"`
	Fence         int64  `json:"fence"`
	OutcomeDigest string `json:"outcome_digest"`
}

func (req reconcileRequest) parse() (digest [32]byte, ok bool) {
	if !github.ValidOpaqueID(req.EnvironmentID) || req.AttemptID <= 0 || req.Fence <= 0 {
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
	if err := h.Reconciler.Reconcile(r.Context(), serverID, digest, req.EnvironmentID, req.AttemptID, req.Fence, outcomeDigest); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}
