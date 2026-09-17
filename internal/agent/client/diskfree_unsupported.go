//go:build !darwin && !linux

package agentclient

func bestEffortDiskFree(string) int64 { return 0 }
