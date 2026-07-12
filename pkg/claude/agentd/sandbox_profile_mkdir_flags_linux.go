//go:build linux

package agentd

import "golang.org/x/sys/unix"

// O_PATH obtains a traversal-only descriptor without requiring read access.
func sandboxProfileDirectoryOpenFlags() int {
	return unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}
