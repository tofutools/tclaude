//go:build unix

package bundletransfer

import (
	"golang.org/x/sys/unix"
	"os"
)

func openSpoolFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
