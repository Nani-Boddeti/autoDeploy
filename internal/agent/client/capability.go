package agentclient

import "runtime"

// Capability is the bounded, honest description of this host the agent
// reports on every heartbeat (internal/agent/http's heartbeatRequest;
// MemoryBytes/DiskFreeBytes are optional/best-effort by design). It never
// claims Docker/BuildKit are present in this slice (Decision 10).
type Capability struct {
	AgentVersion  string
	OS            string
	Arch          string
	CPUCount      int
	MemoryBytes   int64 // best-effort; 0 when unavailable
	DiskFreeBytes int64 // best-effort; 0 when unavailable
}

// localCapability describes the running host using only runtime.GOOS/
// GOARCH/NumCPU plus a best-effort free-disk-space read of stateDir; it adds
// no new dependency. Host memory has no portable stdlib accessor (
// runtime.MemStats reports this process's memory, not the host's), so
// MemoryBytes stays 0 rather than reporting a misleading value.
func localCapability(agentVersion, stateDir string) Capability {
	return Capability{
		AgentVersion:  agentVersion,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUCount:      runtime.NumCPU(),
		DiskFreeBytes: bestEffortDiskFree(stateDir),
	}
}
