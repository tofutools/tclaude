//go:build linux || darwin

package store

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// removeLegacyRunLocks holds no-follow directory descriptors while it inspects
// and unlinks old run locks. Path-based removal after ReadDir would allow a
// concurrently replaced .locks directory to redirect cleanup outside the
// process root.
func removeLegacyRunLocks(root string) error {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rootFD, err := unix.Open(root, flags, 0)
	if err != nil {
		return fmt.Errorf("open process root without following symlinks: %w", err)
	}
	defer func() { _ = unix.Close(rootFD) }()

	lockFD, err := unix.Openat(rootFD, ".locks", flags, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open process lock directory without following symlinks: %w", err)
	}
	lockDir := os.NewFile(uintptr(lockFD), ".locks")
	if lockDir == nil {
		_ = unix.Close(lockFD)
		return fmt.Errorf("wrap process lock directory descriptor")
	}
	defer func() { _ = lockDir.Close() }()

	entries, err := lockDir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read process lock directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isLegacyRunLockName(name) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(lockFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect legacy process run lock %q: %w", name, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if err := unix.Unlinkat(lockFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("remove legacy process run lock %q: %w", name, err)
		}
	}
	return nil
}
