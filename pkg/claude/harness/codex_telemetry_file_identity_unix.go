//go:build linux || darwin

package harness

import (
	"os"
	"syscall"
)

// codexTelemetryFileIdentity makes os.SameFile's device/inode check durable
// across daemon restarts. Linux and macOS are tclaude's supported platforms.
func codexTelemetryFileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}
