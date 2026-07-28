//go:build linux

package session

import (
	"os/exec"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var filteredNetworkLookPath = exec.LookPath

func probeFilteredNetworkPrerequisite() FilteredNetworkPrerequisite {
	if _, err := resolveBwrapServerBinary(sandboxpolicy.NetworkIsolatedWithAgentd); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "bubblewrap/user/network namespace probe failed: " + err.Error(),
		}
	}
	if _, err := filteredNetworkLookPath("pasta"); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "rootless pasta is required on PATH: " + err.Error(),
		}
	}
	if _, err := filteredNetworkLookPath("nft"); err != nil {
		return FilteredNetworkPrerequisite{
			Detail: "nftables (`nft`) is required on PATH: " + err.Error(),
		}
	}
	return FilteredNetworkPrerequisite{
		Available: true,
		Detail:    "bubblewrap user/network namespaces, pasta, and nftables are available",
	}
}
