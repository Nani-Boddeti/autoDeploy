package agenthttp

import (
	"context"
	"net/http"

	"autodeploy/internal/queue"
)

// LeaseActivator matches (*postgres.AgentLeaseRepository).Activate's
// signature exactly, so production wiring can pass that repository
// directly without an adapter -- the same convention every other
// LeaseClaimer/LeaseRenewer/... interface in this package already follows.
type LeaseActivator interface {
	Activate(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64, imageDigest [32]byte) (queue.ActivationResult, error)
}

// activateRequest carries no route digest of its own (2026-09-22
// orchestrator decision overriding the Step 10c design draft): the control
// plane renders the route document and computes its digest in tx1, so the
// agent has nothing of its own to present here yet -- it learns the new
// route revision/hostname/backend identity from activateResponse and only
// then renders and compares.
type activateRequest struct {
	AttemptID   int64  `json:"attempt_id"`
	Fence       int64  `json:"fence"`
	ImageDigest string `json:"image_digest"`
}

func (req activateRequest) parse() (attemptID, fence int64, digest [32]byte, ok bool) {
	if req.AttemptID <= 0 || req.Fence <= 0 {
		return 0, 0, digest, false
	}
	digest, ok = decodeDigest(req.ImageDigest)
	if !ok {
		return 0, 0, digest, false
	}
	return req.AttemptID, req.Fence, digest, true
}

type activateResponse struct {
	OperationID              string `json:"operation_id"`
	RouteRevision            int64  `json:"route_revision"`
	Hostname                 string `json:"hostname"`
	EntrypointName           string `json:"entrypoint_name"`
	CertResolverName         string `json:"cert_resolver_name"`
	BackendRuntimeInstanceID string `json:"backend_runtime_instance_id"`
	RouteDigest              string `json:"route_digest"`
}

// handleActivate serves POST /agent/v1/leases/activate (Step 10c tx1). A
// nil Activator answers 503 before touching any repository -- this is the
// activationEnabled gate (cmd/controlplane/main.go mirrors cmd/agent's
// executorEnabled convention: wiring is skipped entirely while disabled,
// so this nil-dependency branch, already used by every other route in this
// file, is the one gate this endpoint needs).
func (h Handler) handleActivate(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Activator == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req activateRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	attemptID, fence, imageDigest, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := h.Activator.Activate(r.Context(), serverID, digest, attemptID, fence, imageDigest)
	if err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, activateResponse{
		OperationID:              result.OperationID,
		RouteRevision:            result.RouteRevision,
		Hostname:                 result.Hostname,
		EntrypointName:           result.EntrypointName,
		CertResolverName:         result.CertResolverName,
		BackendRuntimeInstanceID: result.BackendRuntimeInstanceID,
		RouteDigest:              result.RouteDigest,
	})
}
