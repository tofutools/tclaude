//go:build linux

package copilotfixture

import (
	"errors"
	"syscall"
)

func ptyProcessGroupGone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
