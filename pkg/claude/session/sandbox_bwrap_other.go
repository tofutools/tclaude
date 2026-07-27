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

func resolveBwrapServerBinary(sandboxpolicy.NetworkPosture) (string, error) {
	return "", fmt.Errorf(
		"tclaude-layer server wrapping requires Linux and bubblewrap")
}

func tclaudeLayerCommand(
	string,
	[]string,
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	sandboxpolicy.MountPlan,
	string,
) (string, error) {
	return "", fmt.Errorf("tclaude-layer is not supported on this platform")
}

func tclaudeLayerStackedCommand(
	string,
	[]string,
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	sandboxpolicy.MountPlan,
	string,
	string,
	string,
	bool,
	string,
) (string, error) {
	return "", fmt.Errorf("stacked tclaude-layer is not supported on this platform")
}

func tclaudeLayerServerCommand(
	string,
	[]string,
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	sandboxpolicy.MountPlan,
	string,
) (string, error) {
	return "", fmt.Errorf(
		"tclaude-layer server wrapping requires Linux and bubblewrap")
}

func tclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State:  "off",
		Source: "tclaude-layer unavailable",
	}
}

func validateTclaudeLayerHarness(harnessName string) error {
	if harnessName == harness.OpenCodeName {
		return fmt.Errorf(
			"tclaude-layer does not support OpenCode on this platform: agentd-owned server wrapping requires Linux and bubblewrap")
	}
	return nil
}
