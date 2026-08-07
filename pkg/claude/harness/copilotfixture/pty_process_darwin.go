//go:build darwin

package copilotfixture

import (
	"errors"
	"syscall"
)

func ptyProcessGroupGone(err error) bool {
	// XNU keeps a process group addressable while it contains zombies, but its
	// killpg1 iterator excludes those zombies. With POSIX kill semantics that
	// leaves zero signalable members and returns EPERM rather than ESRCH. At
	// this point cleanup has already sent SIGKILL to every signalable member;
	// either error therefore means nothing remains that can mutate fixture
	// state. Linux reports ESRCH for the corresponding terminal state and must
	// not inherit this Darwin-specific EPERM rule.
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)
}
