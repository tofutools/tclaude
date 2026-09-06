//go:build linux || darwin

package host

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("action credential file has unsupported native identity")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}
