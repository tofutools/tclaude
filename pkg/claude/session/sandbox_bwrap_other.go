//go:build !linux && !darwin

package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func resolveBwrapBinary(sandboxpolicy.NetworkPosture) (string, error) {
	return "", fmt.Errorf("tclaude-layer requires Linux and bubblewrap; this platform is not supported")
}

func tclaudeLayerCommand(
	string,
	[]string,
	[]string,
	sandboxpolicy.MountPlan,
	string,
) (string, error) {
	return "", fmt.Errorf("tclaude-layer is not supported on this platform")
}

func tclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State:  "off",
		Source: "tclaude-layer unavailable",
	}
}
