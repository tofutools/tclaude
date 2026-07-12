//go:build darwin

package agentd

import "golang.org/x/sys/unix"

// Darwin has no O_PATH/O_SEARCH, and O_EVTONLY descriptors cannot traverse a
// search-only directory with openat. O_RDONLY is therefore intentionally
// stricter than mkdir -p: existing ancestors must also be readable so we can
// retain descriptor-relative no-follow traversal.
func sandboxProfileDirectoryOpenFlags() int {
	return unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}
