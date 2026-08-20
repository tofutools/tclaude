//go:build linux

package session

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var (
	filteredNetworkLookPath          = exec.LookPath
	inspectFilteredNetworkPasta      = inspectPastaCapabilities
	filteredNetworkPastaCommand      = exec.CommandContext
	filteredNetworkPastaProbeTimeout = 5 * time.Second
	filteredNetworkPastaHelpLimit    = 64 << 10
)

type filteredNetworkExecutables struct {
	Pasta   string
	NFT     string
	Nsenter string
	// SyntheticLoopback reports whether Pasta can place host loopback at the
	// fixed, nft-filterable synthetic addresses. False means a base-tier pasta:
	// enough for a private routed namespace, not enough for an authored list.
	SyntheticLoopback bool
}

// baseFilteredNetworkPastaOptions are the controls every packet-gateway launch
// needs regardless of tier: foreground supervision, a configured namespace, no
// host-loopback mapping, and no port forwarding in either direction.
//
// The four "none" port specs matter more than they look. pasta defaults all
// four to "auto", which scans for bound ports and forwards them — and the
// namespace-to-init direction is exactly the splice bypass --no-splice turns
// off. With all four pinned to "none" pasta binds no local port, so the bypass
// has nothing to act on and --no-splice becomes redundant rather than missing.
var baseFilteredNetworkPastaOptions = []string{
	"--foreground",
	"--quiet",
	"--config-net",
	"--no-map-gw",
	"--tcp-ports",
	"--udp-ports",
	"--tcp-ns",
	"--udp-ns",
	"--pid",
}

// syntheticLoopbackFilteredNetworkPastaOptions are the additional controls that
// re-add host loopback at a fixed address an nft rule can name, which is what an
// authored network list needs and a private routed namespace does not.
//
// --gateway is in this tier even though far older pasta also accepts it. In
// pasta mode -g also means "do not copy the host's routes", and before upstream
// 2024-08-07 ("Make -g and -a skip route/addresses copy for matching IP
// version") that suppression was global rather than per-family — so passing an
// IPv6 gateway there also stops the IPv4 default route from being installed,
// and an IPv4-only host loses connectivity entirely. --map-host-loopback landed
// 2024-08-21, two weeks after that fix, so a pasta advertising it necessarily
// scopes the suppression per family and -g is safe to pass alongside it.
var syntheticLoopbackFilteredNetworkPastaOptions = []string{
	"--gateway",
	"--map-guest-addr",
	"--map-host-loopback",
	"--no-splice",
}

func resolveFilteredNetworkExecutables() (filteredNetworkExecutables, error) {
	pasta, err := resolveFilteredNetworkExecutable("pasta")
	if err != nil {
		return filteredNetworkExecutables{}, fmt.Errorf("rootless pasta is required: %w", err)
	}
	syntheticLoopback, err := inspectFilteredNetworkPasta(pasta)
	if err != nil {
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
	return filteredNetworkExecutables{
		Pasta:             pasta,
		NFT:               nft,
		Nsenter:           nsenter,
		SyntheticLoopback: syntheticLoopback,
	}, nil
}

func resolveFilteredNetworkExecutable(name string) (string, error) {
	path, err := filteredNetworkLookPath(name)
	if err != nil {
		return "", err
	}
	return resolveTrustedExecutablePath(name, path)
}

// inspectPastaCapabilities reports whether the binary supports the synthetic
// host-loopback tier. An error means it cannot carry any packet-gateway launch.
func inspectPastaCapabilities(path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), filteredNetworkPastaProbeTimeout)
	defer cancel()
	output := cappedFilteredNetworkOutput{limit: filteredNetworkPastaHelpLimit}
	cmd := filteredNetworkPastaCommand(ctx, path, "--help")
	cmd.Env = filteredNetworkHelperEnv()
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("inspect %q with --help: %w", path, ctxErr)
	}
	if err != nil {
		return false, fmt.Errorf("inspect %q with --help: %w", path, err)
	}
	if output.truncated {
		return false, fmt.Errorf(
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

// validatePastaCapabilities reports whether the synthetic host-loopback tier is
// available. A missing BASE option is an error — that binary cannot carry any
// packet-gateway launch. A missing synthetic option is not: it caps the binary
// at the private routed namespace, which the launch boundary enforces.
func validatePastaCapabilities(help string) (bool, error) {
	tokens := make(map[string]struct{})
	for _, field := range strings.Fields(help) {
		token, _, _ := strings.Cut(field, "=")
		tokens[token] = struct{}{}
	}
	var missing []string
	for _, option := range baseFilteredNetworkPastaOptions {
		if _, ok := tokens[option]; !ok {
			missing = append(missing, option)
		}
	}
	if len(missing) != 0 {
		return false, fmt.Errorf("missing options: %s", strings.Join(missing, ", "))
	}
	for _, option := range syntheticLoopbackFilteredNetworkPastaOptions {
		if _, ok := tokens[option]; !ok {
			return false, nil
		}
	}
	return true, nil
}

func probeFilteredNetworkPrerequisite() FilteredNetworkPrerequisite {
	if _, err := resolveBwrapServerBinary(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "bubblewrap/user/network namespace probe failed: " + err.Error(),
		}
	}
	// resolveBwrapServerBinary already required the base tier to get here, so
	// the private routed namespace is available either way. Re-resolving reads
	// the synthetic tier, which is what separates it from a full list launch.
	executables, err := resolveFilteredNetworkExecutables()
	if err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "filtered-network helper probe failed: " + err.Error(),
		}
	}
	if !executables.SyntheticLoopback {
		return FilteredNetworkPrerequisite{
			PrivateNamespaceDetected: true,
			Detail: "bubblewrap user/network namespace execution passed and trusted pasta, nft, and nsenter " +
				"executables were found, but this pasta predates the synthetic host-loopback controls (" +
				strings.Join(syntheticLoopbackFilteredNetworkPastaOptions, ", ") +
				"); it can carry a private routed namespace but not an authored network list",
		}
	}
	return FilteredNetworkPrerequisite{
		Detected:                 true,
		PrivateNamespaceDetected: true,
		Detail: "bubblewrap user/network namespace execution passed; trusted pasta, nft, and nsenter executables " +
			"were found; the base nft policy is installed from the supervisor via nsenter, and end-to-end gateway readiness is decided at the gated launch boundary",
	}
}
