//go:build unix

package sandboxpolicy

import (
	"io/fs"
	"syscall"
)

// pathDevice returns the device number identifying the filesystem an entry
// lives on, so the spelling probe can prove it never wandered onto a
// neighbouring volume whose case semantics say nothing about the one in
// question.
//
// A false second return means the answer is unavailable, which callers treat as
// "cannot prove we stayed put" — the refusing direction.
func pathDevice(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Dev), true
}
