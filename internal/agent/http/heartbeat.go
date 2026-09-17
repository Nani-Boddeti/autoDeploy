package agenthttp

import (
	"net/http"
	"unicode/utf8"

	"autodeploy/internal/server"
)

const (
	maxHeartbeatStringBytes = 64
	maxHeartbeatCPUCount    = 1 << 16
)

// heartbeatRequest is the fixed-schema capability struct (bounded well under
// MaxHeartbeatBodyBytes). Only ProtocolVersion and liveness are durably
// recorded in this slice (internal/store/postgres/server_repository.go's
// Heartbeat advances servers.last_heartbeat_at only); the remaining fields
// exist end-to-end on the wire because Decision 10 requires
// ExecutorAvailable to be reported honestly, and because a future execution
// slice will persist the rest without a wire-format change.
type heartbeatRequest struct {
	ProtocolVersion   int    `json:"protocol_version"`
	AgentVersion      string `json:"agent_version"`
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	CPUCount          int    `json:"cpu_count"`
	MemoryBytes       int64  `json:"memory_bytes"`
	DiskFreeBytes     int64  `json:"disk_free_bytes"`
	ExecutorAvailable bool   `json:"executor_available"`
	DockerVersion     string `json:"docker_version,omitempty"`
	BuildKitVersion   string `json:"buildkit_version,omitempty"`
}

func (req heartbeatRequest) valid() bool {
	if req.ProtocolVersion != server.ProtocolVersion {
		return false
	}
	if req.CPUCount < 0 || req.CPUCount > maxHeartbeatCPUCount || req.MemoryBytes < 0 || req.DiskFreeBytes < 0 {
		return false
	}
	return boundedHeartbeatString(req.AgentVersion) && boundedHeartbeatString(req.OS) &&
		boundedHeartbeatString(req.Arch) && boundedHeartbeatString(req.DockerVersion) &&
		boundedHeartbeatString(req.BuildKitVersion)
}

// boundedHeartbeatString bounds length and rejects control characters,
// matching the strictness internal/server.ValidName applies to other
// caller-controlled display strings.
func boundedHeartbeatString(s string) bool {
	if len(s) > maxHeartbeatStringBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type heartbeatResponse struct {
	ServerStatus             string `json:"server_status"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
}

func (h Handler) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.Heartbeater == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req heartbeatRequest
	if !h.readJSON(w, r, MaxHeartbeatBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Heartbeater.Heartbeat(r.Context(), serverID, digest); err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, heartbeatResponse{ServerStatus: "active", HeartbeatIntervalSeconds: HeartbeatIntervalSeconds})
}
