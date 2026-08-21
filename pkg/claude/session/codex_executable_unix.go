//go:build unix

package session

import "golang.org/x/sys/unix"

// codexExecutableAccess reports whether this process may execute path, as
// execve would decide it: the kernel answers, so a noexec mount and an LSM
// denial both count, which a mode-bit test cannot see.
//
// unix.Access asks about the real UID where execve tests the effective one.
// They differ only for a set-user-ID process, which neither tclaude nor the
// daemon is, and the portable alternative across every unix target here would
// be a hand-rolled faccessat.
func codexExecutableAccess(path string) error {
	return unix.Access(path, unix.X_OK)
}
