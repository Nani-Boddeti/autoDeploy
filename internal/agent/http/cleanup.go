package agenthttp

import (
	"context"
	"net/http"
	"unicode/utf8"

	"autodeploy/internal/store/postgres"
)

// CleanupStore matches (*postgres.AgentLeaseRepository)'s ClaimCleanups and
// RecordCleanupResult exactly (step 20E), so production wiring passes the
// repository directly.
type CleanupStore interface {
	ClaimCleanups(ctx context.Context, serverID string, presentedDigest [32]byte, limit int) ([]postgres.CleanupClaim, error)
	RecordCleanupResult(ctx context.Context, serverID string, presentedDigest [32]byte, cleanupID int64, result, errorClass string, imageKept bool, summary string) error
}

// MaxCleanupClaim bounds one claim; the store enforces the same bound.
const MaxCleanupClaim = 16

type cleanupClaimRequest struct {
	Limit int `json:"limit"`
}

type cleanupClaimItem struct {
	CleanupID         int64  `json:"cleanup_id"`
	ReleaseID         string `json:"release_id"`
	EnvironmentID     string `json:"environment_id"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	ImageDigest       string `json:"image_digest"`
}

type cleanupClaimResponse struct {
	Cleanups []cleanupClaimItem `json:"cleanups"`
}

// handleCleanupClaim serves POST /agent/v1/cleanup/claim. Credential only: the
// server is the credential's, never a body field. The body names only how many
// releases the agent will take at most.
func (h Handler) handleCleanupClaim(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.CleanupStore == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req cleanupClaimRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if req.Limit < 1 || req.Limit > MaxCleanupClaim {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	claims, err := h.CleanupStore.ClaimCleanups(r.Context(), serverID, digest, req.Limit)
	if err != nil {
		h.mapError(w, err)
		return
	}
	resp := cleanupClaimResponse{Cleanups: make([]cleanupClaimItem, 0, len(claims))}
	for _, c := range claims {
		resp.Cleanups = append(resp.Cleanups, cleanupClaimItem{
			CleanupID: c.CleanupID, ReleaseID: c.ReleaseID, EnvironmentID: c.EnvironmentID,
			RuntimeInstanceID: c.RuntimeInstanceID, ImageDigest: c.ImageDigest,
		})
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// cleanupResultRequest is the wire shape of POST /agent/v1/cleanup/result.
type cleanupResultRequest struct {
	CleanupID  int64  `json:"cleanup_id"`
	Result     string `json:"result"`
	ErrorClass string `json:"error_class"`
	ImageKept  bool   `json:"image_kept"`
	Summary    string `json:"summary"`
}

func (req cleanupResultRequest) valid() bool {
	if req.CleanupID <= 0 || len(req.Summary) > 2048 || !utf8.ValidString(req.Summary) {
		return false
	}
	switch req.Result {
	case postgres.CleanupResultRemoved:
		return req.ErrorClass == ""
	case postgres.CleanupResultFailed, postgres.CleanupResultSkipped:
		return !req.ImageKept && (req.ErrorClass == postgres.CleanupErrorEngine || req.ErrorClass == postgres.CleanupErrorIdentity ||
			req.ErrorClass == postgres.CleanupErrorStop || req.ErrorClass == postgres.CleanupErrorOther)
	}
	return false
}

// handleCleanupResult serves POST /agent/v1/cleanup/result. Replays of an
// identical report answer 200; a different payload for a completed cleanup is
// 409 integrity_conflict; another server's or an unknown cleanup id is 401.
func (h Handler) handleCleanupResult(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.CleanupStore == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req cleanupResultRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.CleanupStore.RecordCleanupResult(r.Context(), serverID, digest, req.CleanupID, req.Result, req.ErrorClass, req.ImageKept, req.Summary); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct{}{})
}
