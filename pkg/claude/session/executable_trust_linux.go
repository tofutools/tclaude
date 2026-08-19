//go:build linux

package session

import (
	"fmt"
	"os"
	"path/filepath"
)

// The trust walk applied to the host executables a tclaude-layer launch
// resolves from PATH to build and police its sandbox: bubblewrap itself, and
// the filtered-network helpers (pasta, nft, nsenter). They are canonicalized
// and walked before use rather than taken from PATH as-is.
//
// This is not a claim about everything a launch execs — the harness binary,
// /bin/sh, tmux and tclaude's own subcommands run unwalked, and their
// trustworthiness rests elsewhere.

var (
	trustWalkLstat                = os.Lstat
	trustWalkEvalSymlinks         = filepath.EvalSymlinks
	validateTrustedExecutablePath = validateTrustedExecutable
)

// resolveTrustedExecutablePath canonicalizes an executable already resolved
// from PATH and runs the trust walk over the result. It returns the resolved
// path, which is what the caller should exec: the walk describes that path, not
// the symlink chain that led to it.
func resolveTrustedExecutablePath(name, path string) (string, error) {
	path, err := trustWalkEvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", name, err)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s resolved to non-absolute path %q", name, path)
	}
	if err := validateTrustedExecutablePath(path); err != nil {
		return "", fmt.Errorf("%s executable %q is not trusted: %w", name, path, err)
	}
	return path, nil
}

// validateTrustedExecutable walks path and every parent directory up to the
// filesystem root: no component may be group/world writable, the target must be
// a regular executable, and every parent must be a directory.
//
// Ownership is deliberately not checked. These executables are commonly
// installed from a user-owned prefix (a local build, a per-user package
// manager), so the walk rests on the writability bound alone — which means one
// owned by another local user is accepted, and that owner can still swap the
// binary.
func validateTrustedExecutable(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := trustWalkLstat(current)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("path component %q is group/world writable", current)
		}
		if current == path {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				return fmt.Errorf("path is not a regular executable")
			}
		} else if !info.IsDir() {
			return fmt.Errorf("parent %q is not a directory", current)
		}
		if current == string(filepath.Separator) {
			break
		}
	}
	return nil
}
