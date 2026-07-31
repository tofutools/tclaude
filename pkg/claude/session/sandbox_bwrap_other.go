//go:build !linux && !darwin

package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func resolveBwrapBinary(
	sandboxpolicy.NetworkPosture,
	sandboxpolicy.RootPosture,
) (string, error) {
	return "", fmt.Errorf("tclaude-layer requires Linux/bubblewrap or macOS/Seatbelt; this platform is not supported")
}

func resolveBwrapServerBinary(
	sandboxpolicy.NetworkPosture,
	sandboxpolicy.RootPosture,
) (string, error) {
	return "", fmt.Errorf(
		"tclaude-layer server wrapping requires Linux/bubblewrap or macOS/Seatbelt")
}

// tclaudeLayerToolingPresence mirrors resolveBwrapBinary's refusal: there is no
// tooling to look for on an unsupported platform.
func tclaudeLayerToolingPresence(bool) error {
	return fmt.Errorf("tclaude-layer requires Linux/bubblewrap or macOS/Seatbelt; this platform is not supported")
}

func tclaudeLayerCommand(
	string,
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	[]string,
	[]TclaudeLayerReadOnlyBind,
	[]string,
	sandboxpolicy.MountPlan,
	string,
) (string, error) {
	return "", fmt.Errorf("tclaude-layer is not supported on this platform")
}

func tclaudeLayerStackedCommand(
	string,
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	[]string,
	[]TclaudeLayerReadOnlyBind,
	[]string,
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
	[]TclaudeLayerPrivateWriteDir,
	[]string,
	[]TclaudeLayerReadOnlyBind,
	[]string,
	sandboxpolicy.MountPlan,
	string,
) (string, error) {
	return "", fmt.Errorf(
		"tclaude-layer server wrapping requires Linux/bubblewrap or macOS/Seatbelt")
}

func tclaudeLayerUnixRelayServerCommandArgs(
	_ TclaudeLayerLaunchSpec,
	bwrapArgv []string,
) ([]string, error) {
	return bwrapArgv, nil
}

func tclaudeLayerOpenCodeLaunchOSSandbox() harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State:  "off",
		Source: "tclaude-layer unavailable",
	}
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
			"tclaude-layer does not support OpenCode on this platform: agentd-owned server wrapping requires Linux/bubblewrap or macOS/Seatbelt")
	}
	return nil
}
