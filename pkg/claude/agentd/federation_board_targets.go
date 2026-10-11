package agentd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Custom roots are operator-selected existing directories. Open each component
// without following links; path spelling and root identity both enter preview.
func boardCustomRoot(path string) (boardSkillRoot, error) {
	target := boardSkillRoot{ID: "custom", Label: "Custom directory", Kind: "custom"}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return target, errors.New("custom_dir must be an absolute existing directory")
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return target, errors.New("custom_dir cannot contain '..'")
		}
	}
	target.Path = filepath.Clean(path)
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return target, e
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(target.Path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return target, fmt.Errorf("custom_dir must exist and contain no symlink components: %w", e)
		}
		_ = unix.Close(fd)
		fd = next
	}
	var info unix.Stat_t
	if e = unix.Fstat(fd, &info); e != nil {
		return target, e
	}
	target.Identity = fmt.Sprintf("%d:%d", info.Dev, info.Ino)
	return target, nil
}
func boardRootIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

// Group skill roots must remain beneath the project, including on first install.
func boardGroupSkillPath(path string) error {
	for _, dir := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("group skill root cannot contain symlinks")
		}
	}
	return nil
}

func openBoardSkillRoot(target boardSkillRoot) (*os.Root, error) {
	if target.Kind != "group" {
		if target.Kind != "custom" {
			if err := os.MkdirAll(target.Path, 0700); err != nil {
				return nil, err
			}
		}
		return os.OpenRoot(target.Path)
	}
	base := filepath.Dir(filepath.Dir(target.Path))
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, err
	}
	if fmt.Sprintf("%d:%d", info.Dev, info.Ino) != target.BoundaryIdentity {
		return nil, errors.New("group directory changed since preview; preview again")
	}
	for _, part := range []string{".claude", "skills"} {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, err
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("group skill directory changed or contains a symlink: %w", err)
		}
		_ = unix.Close(fd)
		fd = next
	}
	// /dev/fd exists on both supported platforms and opens this anchored descriptor,
	// rather than resolving the mutable project path a second time.
	return os.OpenRoot(fmt.Sprintf("/dev/fd/%d", fd))
}
