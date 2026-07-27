//go:build linux || darwin

package harness

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// readClaudeManagedPolicyFile atomically refuses a final-component symlink.
// SnapshotClaudeManagedPolicy freezes the bytes read from this descriptor, so
// a later pathname replacement cannot change the probe or launch authority.
func readClaudeManagedPolicyFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}
