//go:build linux || darwin

package agentd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// mkdirAllNoFollow implements mkdir -p without ever following a symlink.
// Sandbox-profile paths have already had legitimate symlinks canonicalized,
// so encountering one here means the tree changed after validation. Holding a
// descriptor for every parent while opening/creating the next component closes
// the check/use gap that a string-path os.MkdirAll would leave behind.
func mkdirAllNoFollow(path string, perm os.FileMode) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	current, err := unix.Open(string(filepath.Separator), sandboxProfileDirectoryOpenFlags(), 0)
	if err != nil {
		return fmt.Errorf("open filesystem root: %w", err)
	}
	defer func() { _ = unix.Close(current) }()

	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			return fmt.Errorf("path %q escapes the filesystem root", path)
		}
		next, openErr := unix.Openat(current, component, sandboxProfileDirectoryOpenFlags(), 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, component, uint32(perm.Perm())); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return fmt.Errorf("create directory component %q: %w", component, mkdirErr)
			}
			next, openErr = unix.Openat(current, component, sandboxProfileDirectoryOpenFlags(), 0)
		}
		if openErr != nil {
			return fmt.Errorf("open directory component %q without following symlinks: %w", component, openErr)
		}
		_ = unix.Close(current)
		current = next
	}
	return nil
}
