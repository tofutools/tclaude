//go:build linux

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var (
	filteredNetworkLookPath           = exec.LookPath
	filteredNetworkEvalSymlinks       = filepath.EvalSymlinks
	validateFilteredNetworkExecutable = validateRootOwnedExecutable
)

type filteredNetworkExecutables struct {
	Pasta string
	NFT   string
}

func resolveFilteredNetworkExecutables() (filteredNetworkExecutables, error) {
	pasta, err := resolveFilteredNetworkExecutable("pasta")
	if err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf("rootless pasta is required: %w", err)
	}
	nft, err := resolveFilteredNetworkExecutable("nft")
	if err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf("nftables (`nft`) is required: %w", err)
	}
	return filteredNetworkExecutables{Pasta: pasta, NFT: nft}, nil
}

func resolveFilteredNetworkExecutable(name string) (string, error) {
	path, err := filteredNetworkLookPath(name)
	if err != nil {
		return "", err
	}
	path, err = filteredNetworkEvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", name, err)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s resolved to non-absolute path %q", name, path)
	}
	if err := validateFilteredNetworkExecutable(path); err != nil {
		return "", fmt.Errorf("%s executable %q is not trusted: %w", name, path, err)
	}
	return path, nil
}

func validateRootOwnedExecutable(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return fmt.Errorf("path component %q is not root-owned", current)
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

func probeFilteredNetworkPrerequisite() FilteredNetworkPrerequisite {
	if _, err := resolveBwrapServerBinary(sandboxpolicy.NetworkIsolatedWithAgentd); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "bubblewrap/user/network namespace probe failed: " + err.Error(),
		}
	}
	if _, err := resolveFilteredNetworkExecutables(); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: err.Error(),
		}
	}
	return FilteredNetworkPrerequisite{
		Detected: true,
		Detail: "bubblewrap user/network namespace execution passed; trusted root-owned pasta and nft executables " +
			"were found; end-to-end gateway readiness is decided at the gated launch boundary",
	}
}
