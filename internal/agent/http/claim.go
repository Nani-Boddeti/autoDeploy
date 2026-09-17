package agenthttp

import (
	"net/http"
	"time"

	"autodeploy/internal/queue"
)

// claimLimit is the only value V1 accepts (tasks/todo.md Step 5: "limit
// (1..1; V1 max 1)"); anything else is rejected as invalid input before any
// repository call.
const claimLimit = 1

type claimRequest struct {
	Limit             int  `json:"limit"`
	ExecutorAvailable bool `json:"executor_available"`
}

func (req claimRequest) valid() bool { return req.Limit == claimLimit }

type claimResponse struct {
	Leases []leaseView `json:"leases"`
}

// leaseView is the wire shape of one queue.ClaimedLease: job, attempt,
// fence, environment, deployment, repository, ref, commit, and both
// deadlines, RFC3339Nano UTC (tasks/todo.md Step 5).
type leaseView struct {
	JobID              int64  `json:"job_id"`
	AttemptID          int64  `json:"attempt_id"`
	AttemptNumber      int    `json:"attempt_number"`
	Fence              int64  `json:"fence"`
	ProjectID          string `json:"project_id"`
	EnvironmentID      string `json:"environment_id"`
	DeploymentID       string `json:"deployment_id"`
	RepositoryID       int64  `json:"repository_id"`
	Ref                string `json:"ref"`
	CommitSHA          string `json:"commit_sha"`
	LeaseExpiresAt     string `json:"lease_expires_at"`
	AuthorityExpiresAt string `json:"authority_expires_at"`
}

func formatDeadline(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func newLeaseView(l queue.ClaimedLease) leaseView {
	return leaseView{
		JobID:              l.JobID,
		AttemptID:          l.AttemptID,
		AttemptNumber:      l.AttemptNumber,
		Fence:              l.Fence,
		ProjectID:          l.ProjectID,
		EnvironmentID:      l.EnvironmentID,
		DeploymentID:       l.DeploymentID,
		RepositoryID:       l.RepositoryID,
		Ref:                l.Ref,
		CommitSHA:          l.CommitSHA,
		LeaseExpiresAt:     formatDeadline(l.LeaseExpiresAt),
		AuthorityExpiresAt: formatDeadline(l.AuthorityExpiresAt),
	}
}

// handleClaim requires ExecutorAvailable to be true in the request before
// it will ever call Claimer.Claim (Decision 10: "cmd/agent in this slice
// never reports a fabricated success ... claim returns nothing to such
// agents"). Authentication itself still always runs first, via
// Authenticator, so an unauthenticated caller cannot use
// executor_available=false as a way to probe this route without a valid
// credential.
func (h Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Authenticator == nil || h.Claimer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req claimRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Authenticator.Authenticate(r.Context(), serverID, digest); err != nil {
		h.mapError(w, err)
		return
	}
	if !req.ExecutorAvailable {
		h.writeJSON(w, http.StatusOK, claimResponse{Leases: []leaseView{}})
		return
	}
	leases, err := h.Claimer.Claim(r.Context(), serverID, digest, req.Limit)
	if err != nil {
		h.mapError(w, err)
		return
	}
	views := make([]leaseView, 0, len(leases))
	for _, l := range leases {
		views = append(views, newLeaseView(l))
	}
	h.writeJSON(w, http.StatusOK, claimResponse{Leases: views})
}
