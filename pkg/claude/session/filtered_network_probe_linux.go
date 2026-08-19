//go:build linux

package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var (
	filteredNetworkLookPath           = exec.LookPath
	filteredNetworkEvalSymlinks       = filepath.EvalSymlinks
	filteredNetworkLstat              = os.Lstat
	validateFilteredNetworkExecutable = validateTrustedExecutable
	inspectFilteredNetworkPasta       = inspectPastaCapabilities
	filteredNetworkPastaCommand       = exec.CommandContext
	filteredNetworkPastaProbeTimeout  = 5 * time.Second
	filteredNetworkPastaHelpLimit     = 64 << 10
)

type filteredNetworkExecutables struct {
	Pasta   string
	NFT     string
	Nsenter string
}

var requiredFilteredNetworkPastaOptions = []string{
	"--foreground",
	"--quiet",
	"--config-net",
	"--gateway",
	"--no-map-gw",
	"--map-guest-addr",
	"--map-host-loopback",
	"--tcp-ports",
	"--udp-ports",
	"--tcp-ns",
	"--udp-ns",
	"--no-splice",
	"--pid",
}

func resolveFilteredNetworkExecutables() (filteredNetworkExecutables, error) {
	pasta, err := resolveFilteredNetworkExecutable("pasta")
	if err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf("rootless pasta is required: %w", err)
	}
	if err := inspectFilteredNetworkPasta(pasta); err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf(
			"rootless pasta lacks the required filtered-network capabilities: %w", err)
	}
	nft, err := resolveFilteredNetworkExecutable("nft")
	if err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf("nftables (`nft`) is required: %w", err)
	}
	// The base policy is installed by the supervisor, which joins the sandbox
	// namespace through nsenter (setns(CLONE_NEWUSER) is illegal from the
	// multithreaded Go runtime). nsenter (util-linux) is therefore required.
	nsenter, err := resolveFilteredNetworkExecutable("nsenter")
	if err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf(
			"util-linux `nsenter` is required: %w", err)
	}
	return filteredNetworkExecutables{Pasta: pasta, NFT: nft, Nsenter: nsenter}, nil
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

func inspectPastaCapabilities(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), filteredNetworkPastaProbeTimeout)
	defer cancel()
	output := cappedFilteredNetworkOutput{limit: filteredNetworkPastaHelpLimit}
	cmd := filteredNetworkPastaCommand(ctx, path, "--help")
	cmd.Env = filteredNetworkHelperEnv()
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("inspect %q with --help: %w", path, ctxErr)
	}
	if err != nil {
		return fmt.Errorf("inspect %q with --help: %w", path, err)
	}
	if output.truncated {
		return fmt.Errorf(
			"inspect %q with --help: output exceeds %d bytes",
			path,
			filteredNetworkPastaHelpLimit,
		)
	}
	return validatePastaCapabilities(output.String())
}

type cappedFilteredNetworkOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (w *cappedFilteredNetworkOutput) Write(p []byte) (int, error) {
	originalLen := len(p)
	remaining := w.limit - w.buffer.Len()
	if remaining <= 0 {
		w.truncated = w.truncated || originalLen != 0
		return originalLen, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	_, err := w.buffer.Write(p)
	return originalLen, err
}

func (w *cappedFilteredNetworkOutput) String() string {
	return w.buffer.String()
}

func validatePastaCapabilities(help string) error {
	tokens := make(map[string]struct{})
	for _, field := range strings.Fields(help) {
		token, _, _ := strings.Cut(field, "=")
		tokens[token] = struct{}{}
	}
	var missing []string
	for _, option := range requiredFilteredNetworkPastaOptions {
		if _, ok := tokens[option]; !ok {
			missing = append(missing, option)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("missing options: %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateTrustedExecutable walks path and every parent directory up to the
// filesystem root and refuses anything another local user could swap out from
// under us. Ownership is deliberately not checked: these helpers are commonly
// installed from a user-owned prefix (a local build, a per-user package
// manager), so the trust walk rests on the group/world-writability bound
// instead.
func validateTrustedExecutable(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := filteredNetworkLstat(current)
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

func probeFilteredNetworkPrerequisite() FilteredNetworkPrerequisite {
	if _, err := resolveBwrapServerBinary(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "bubblewrap/user/network namespace probe failed: " + err.Error(),
		}
	}
	return FilteredNetworkPrerequisite{
		Detected: true,
		Detail: "bubblewrap user/network namespace execution passed; trusted pasta, nft, and nsenter executables " +
			"were found; the base nft policy is installed from the supervisor via nsenter, and end-to-end gateway readiness is decided at the gated launch boundary",
	}
}
