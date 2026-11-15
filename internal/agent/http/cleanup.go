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

// CleanupKindsStore is the optional extension of CleanupStore that serves a
// claim which opted in to failed candidates (step 21B-1).
// (*postgres.AgentLeaseRepository) implements it; a store without it simply
// never yields failed candidates.
type CleanupKindsStore interface {
	ClaimCleanupsWithKinds(ctx context.Context, serverID string, presentedDigest [32]byte, limit int, includeFailedCandidates bool) ([]postgres.CleanupClaim, error)
}

// Production wiring passes the repository, so both interfaces must hold: a
// missing method would silently drop the failed-candidate opt-in.
var (
	_ CleanupStore      = (*postgres.AgentLeaseRepository)(nil)
	_ CleanupKindsStore = (*postgres.AgentLeaseRepository)(nil)
)

// MaxCleanupClaim bounds one claim; the store enforces the same bound.
const MaxCleanupClaim = 16

// maxCleanupKinds bounds the optional kinds list of a claim request.
const maxCleanupKinds = 4

// cleanupClaimRequest is the wire shape of POST /agent/v1/cleanup/claim.
// Kinds is the agent's capability opt-in: absent or empty is the step 20E
// request and gets the step 20E response byte for byte; a non-empty list
// switches the response to the extended item shape, and only a list naming
// "failed_candidate" can yield failed-candidate items.
type cleanupClaimRequest struct {
	Limit int      `json:"limit"`
	Kinds []string `json:"kinds"`
}

// parseKinds reports whether the request opted in at all and whether it asked
// for failed candidates. Unknown kinds are an invalid request (the decoder is
// strict everywhere else too).
func (req cleanupClaimRequest) parseKinds() (extended, failedCandidates, ok bool) {
	if len(req.Kinds) > maxCleanupKinds {
		return false, false, false
	}
	for _, k := range req.Kinds {
		switch k {
		case postgres.CleanupKindRetained:
		case postgres.CleanupKindFailedCandidate:
			failedCandidates = true
		default:
			return false, false, false
		}
	}
	return len(req.Kinds) > 0, failedCandidates, true
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

// cleanupClaimItemExtended is the item shape for a claim that sent kinds. A
// retained item is the 20E item plus kind "retained". A failed-candidate item
// has an empty runtime_instance_id, one runtime_instance_ids entry per attempt
// of its deployment, and an image_digest that is empty when the image is kept.
type cleanupClaimItemExtended struct {
	CleanupID          int64    `json:"cleanup_id"`
	ReleaseID          string   `json:"release_id"`
	EnvironmentID      string   `json:"environment_id"`
	RuntimeInstanceID  string   `json:"runtime_instance_id"`
	ImageDigest        string   `json:"image_digest"`
	Kind               string   `json:"kind"`
	RuntimeInstanceIDs []string `json:"runtime_instance_ids,omitempty"`
}

type cleanupClaimResponseExtended struct {
	Cleanups []cleanupClaimItemExtended `json:"cleanups"`
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
	extended, wantFailed, kindsOK := req.parseKinds()
	if !kindsOK {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var (
		claims []postgres.CleanupClaim
		err    error
	)
	if ks, ok := h.CleanupStore.(CleanupKindsStore); ok && wantFailed {
		claims, err = ks.ClaimCleanupsWithKinds(r.Context(), serverID, digest, req.Limit, true)
	} else {
		claims, err = h.CleanupStore.ClaimCleanups(r.Context(), serverID, digest, req.Limit)
	}
	if err != nil {
		h.mapError(w, err)
		return
	}
	if extended {
		resp := cleanupClaimResponseExtended{Cleanups: make([]cleanupClaimItemExtended, 0, len(claims))}
		for _, c := range claims {
			kind := c.Kind
			if kind == "" {
				kind = postgres.CleanupKindRetained
			}
			resp.Cleanups = append(resp.Cleanups, cleanupClaimItemExtended{
				CleanupID: c.CleanupID, ReleaseID: c.ReleaseID, EnvironmentID: c.EnvironmentID,
				RuntimeInstanceID: c.RuntimeInstanceID, ImageDigest: c.ImageDigest,
				Kind: kind, RuntimeInstanceIDs: c.RuntimeInstanceIDs,
			})
		}
		h.writeJSON(w, http.StatusOK, resp)
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
