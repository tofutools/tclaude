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

// removeDirAtNoFollow recursively removes one direct child of base without
// following symlinks at any path component. Holding directory descriptors from
// the filesystem root through the recursive walk closes the check/use gap that
// string-path os.RemoveAll would leave behind.
func removeDirAtNoFollow(base, name string) (bool, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, string(filepath.Separator)) {
		return false, fmt.Errorf("invalid directory child %q", name)
	}
	parent, err := openDirectoryPathNoFollow(base)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(parent) }()
	return removeEntryAtNoFollow(parent, name)
}

func openDirectoryPathNoFollow(path string) (int, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return -1, fmt.Errorf("path %q is not absolute", path)
	}
	flags := sandboxProfileDirectoryOpenFlags()
	current, err := unix.Open(string(filepath.Separator), flags, 0)
	if err != nil {
		return -1, fmt.Errorf("open filesystem root: %w", err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			_ = unix.Close(current)
			return -1, fmt.Errorf("path %q escapes the filesystem root", path)
		}
		next, openErr := unix.Openat(current, component, flags, 0)
		_ = unix.Close(current)
		if openErr != nil {
			return -1, fmt.Errorf("open directory component %q without following symlinks: %w", component, openErr)
		}
		current = next
	}
	return current, nil
}

func removeEntryAtNoFollow(parent int, name string) (bool, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(parent, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		if err := unix.Unlinkat(parent, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	dir := os.NewFile(uintptr(fd), name)
	if dir == nil {
		_ = unix.Close(fd)
		return false, fmt.Errorf("wrap directory descriptor")
	}
	names, readErr := dir.Readdirnames(-1)
	if readErr != nil {
		_ = dir.Close()
		return false, readErr
	}
	var errs []error
	for _, child := range names {
		if _, err := removeEntryAtNoFollow(fd, child); err != nil {
			errs = append(errs, fmt.Errorf("remove %q: %w", child, err))
		}
	}
	if err := dir.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return false, errors.Join(errs...)
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return false, err
	}
	return true, nil
}
