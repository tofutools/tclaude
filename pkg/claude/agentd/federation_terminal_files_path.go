package agentd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/harnesscredentials"
	"golang.org/x/sys/unix"
)

const terminalFileMaxBytes = 32 << 20

type terminalFileError struct {
	code, message string
	status        int
}

func (e *terminalFileError) Error() string { return e.message }
func terminalFileRefusal(status int, code, message string) error {
	return &terminalFileError{code, message, status}
}

// This directory is selected locally, once at attach. All peer-selected
// components below it are subsequently opened relative to pinned descriptors.
func openTerminalFileRoot(cwd string) (*os.File, string, error) {
	if !filepath.IsAbs(cwd) {
		return nil, "", terminalFileRefusal(403, "root_too_broad", "downloads need an agent running in a project directory")
	}
	expected, err := os.Stat(cwd)
	if err != nil || !expected.IsDir() {
		return nil, "", terminalFileRefusal(403, "unsafe_path", "working directory unavailable")
	}
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, "", terminalFileRefusal(403, "unsafe_path", "working directory unavailable")
	}
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	policyRoot := root
	if runtime.GOOS == "darwin" {
		policyRoot = strings.ToLower(root)
	}
	broad := root == "/" || root == home
	if homeInfo, e := os.Stat(home); e == nil {
		broad = broad || os.SameFile(expected, homeInfo)
	}
	for _, dir := range []string{"/etc", "/usr", "/var", "/private", "/private/etc", "/private/var", "/System", "/Library", "/bin", "/sbin", "/dev", "/proc", "/sys"} {
		if runtime.GOOS == "darwin" {
			dir = strings.ToLower(dir)
		}
		broad = broad || policyRoot == dir
	}
	for _, dir := range []string{"/etc/", "/usr/", "/private/etc/", "/System/", "/Library/", "/dev/", "/proc/", "/sys/"} {
		if runtime.GOOS == "darwin" {
			dir = strings.ToLower(dir)
		}
		broad = broad || strings.HasPrefix(policyRoot, dir)
	}
	if broad {
		return nil, "", terminalFileRefusal(403, "root_too_broad", "downloads need an agent running in a project directory; system and home roots are refused")
	}
	if _, err := terminalFileRelative("/", root); err != nil {
		return nil, "", err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	for _, part := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, "", terminalFileRefusal(403, "unsafe_path", "working directory changed while opening")
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), root)
	opened, e := f.Stat()
	if e != nil || !os.SameFile(expected, opened) {
		_ = f.Close()
		return nil, "", terminalFileRefusal(403, "unsafe_path", "working directory changed while opening")
	}
	return f, root, nil
}

func terminalFileRelative(root, path string) (string, error) {
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", terminalFileRefusal(403, "unsafe_path", "invalid file path")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", terminalFileRefusal(403, "unsafe_path", "parent traversal is refused")
		}
	}
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(root, path)
		if err != nil || path == ".." || strings.HasPrefix(path, "../") {
			return "", terminalFileRefusal(403, "unsafe_path", "file must be inside the attached project's working directory")
		}
	}
	path = filepath.Clean(path)
	parts := strings.Split(filepath.Join(root, path), "/")
	folded := parts
	if runtime.GOOS == "darwin" {
		folded = strings.Split(strings.ToLower(filepath.Join(root, path)), "/")
	}
	for i, part := range folded {
		if part == ".ssh" || part == ".gnupg" || part == ".aws" || part == ".azure" || part == ".kube" || part == ".netrc" || part == ".npmrc" || part == ".pypirc" || part == ".git-credentials" || part == ".env" || strings.HasPrefix(part, ".env.") {
			return "", terminalFileRefusal(403, "unsafe_path", "credential and secret paths cannot be downloaded")
		}
		suffix := strings.Join(folded[i:], "/")
		for _, denied := range append([]string{".docker/config.json", ".config/gh", ".config/gcloud", ".git/config"}, harnesscredentials.StandardSecretPaths()...) {
			if suffix == denied || strings.HasPrefix(suffix, denied+"/") {
				return "", terminalFileRefusal(403, "unsafe_path", "credential and secret paths cannot be downloaded")
			}
		}
	}
	home, _ := os.UserHomeDir()
	abs := filepath.Join(root, path)
	for _, credential := range harnesscredentials.ConfiguredSecretPaths(home) {
		if runtime.GOOS == "darwin" {
			credential = strings.ToLower(credential)
			abs = strings.ToLower(abs)
		}
		if abs == credential {
			return "", terminalFileRefusal(403, "unsafe_path", "harness credentials cannot be downloaded")
		}
	}
	return path, nil
}

func openTerminalFile(root *os.File, rootPath, path string) (*os.File, error) {
	return openTerminalProjectPath(root, rootPath, path, false)
}

func openTerminalProjectPath(root *os.File, rootPath, path string, directory bool) (*os.File, error) {
	rel, err := terminalFileRelative(rootPath, path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	defer func() { _ = unix.Close(fd) }()
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 || directory {
			flags |= unix.O_DIRECTORY
		}
		next, e := unix.Openat(fd, part, flags, 0)
		if e != nil {
			if errors.Is(e, unix.ENOENT) {
				return nil, terminalFileRefusal(404, "not_found", "file not found")
			}
			return nil, terminalFileRefusal(403, "unsafe_path", "symlinks and nonregular file paths are refused")
		}
		_ = unix.Close(fd)
		fd = next
	}
	f := os.NewFile(uintptr(fd), rel)
	fd = -1
	info, err := f.Stat()
	if err != nil || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		_ = f.Close()
		return nil, terminalFileRefusal(403, "unsafe_path", "only regular files can be downloaded")
	}
	if !directory && info.Size() > terminalFileMaxBytes {
		_ = f.Close()
		return nil, terminalFileRefusal(413, "file_too_large", "file exceeds the 32 MiB download cap")
	}
	return f, nil
}
