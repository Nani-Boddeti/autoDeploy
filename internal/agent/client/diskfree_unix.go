//go:build darwin || linux

package agentclient

import "golang.org/x/sys/unix"

// bestEffortDiskFree reports free bytes on the filesystem holding path, or
// 0 if path is empty or the syscall fails. It is explicitly best-effort
// (heartbeatRequest.valid only rejects a negative value), so any failure
// here must never block a heartbeat.
func bestEffortDiskFree(path string) int64 {
	if path == "" {
		return 0
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0
	}
	free := uint64(stat.Bavail) * uint64(stat.Bsize) //nolint:unconvert
	if free > 1<<62 {
		return 0
	}
	return int64(free)
}
