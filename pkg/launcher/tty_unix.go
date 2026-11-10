//go:build linux || darwin

package launcher

import (
	"errors"
	"os"
	"syscall"
)

// hasControllingTTY reports whether this process has a controlling
// terminal: /dev/tty opens only when it does (ENXIO otherwise).
func hasControllingTTY() (bool, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err == nil {
		_ = f.Close()
		return true, nil
	}
	if errors.Is(err, syscall.ENXIO) {
		return false, nil
	}
	return false, err
}
