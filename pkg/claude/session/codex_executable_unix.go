//go:build unix && !aix

package session

import "golang.org/x/sys/unix"

// codexExecutableAccess reports whether this process may execute path. It asks
// the kernel rather than reading mode bits, so the ownership the caller
// actually has and a noexec mount both count — faccessat reports EACCES for an
// executable on a noexec mount, which a mode-bit test cannot see.
//
// AT_EACCESS makes the question use the effective UID, which is the one execve
// tests; this is the same call exec.LookPath makes for the same reason.
//
// It is not a complete answer for every refusal. A path-based LSM such as
// AppArmor mediates program execution at exec rather than at faccessat, so its
// denials still surface only when execve runs.
//
// AIX is excluded from the build tag rather than served here: x/sys does not
// define AT_EACCESS for it, so this would not compile. It falls to the mode-bit
// implementation, which is what this package did on every platform before.
func codexExecutableAccess(path string) error {
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS)
}
