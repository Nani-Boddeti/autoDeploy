//go:build linux

package credential

import "golang.org/x/sys/unix"

type statFingerprint struct {
	dev, ino                 uint64
	size                     int64
	mode                     uint32
	uid                      uint32
	msec, mnsec, csec, cnsec int64
}

func safeCredentialStat(stat unix.Stat_t, expectedUID int) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && safeFileMode(uint32(stat.Mode)) && int(stat.Uid) == expectedUID
}
func credentialStatFingerprint(stat unix.Stat_t) statFingerprint {
	return statFingerprint{stat.Dev, stat.Ino, stat.Size, uint32(stat.Mode), stat.Uid, stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec}
}
