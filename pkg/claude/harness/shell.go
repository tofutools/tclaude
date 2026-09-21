package harness

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// ShellName is the pseudo-harness that runs the user's ordinary shell. It has
// no model or native sandbox; registering it lets agent spawn reuse the normal
// enrollment, profile, worktree, and tclaude-layer launch pipeline.
const ShellName = "shell"

func init() {
	Register(&Harness{
		Name:             ShellName,
		DisplayName:      "Shell",
		Spawn:            shellSpawner{},
		Models:           shellModels{},
		Sandbox:          shellSandbox{},
		TclaudeLayerMode: ShellSandboxOff,
		TmuxScrollback:   true,
		LaunchEnrollment: true,
		CommandInput:     true,
	})
}

type shellSpawner struct{}

func (shellSpawner) Binary() string { return shellExecutable() }

func shellExecutable() string {
	if value := strings.TrimSpace(os.Getenv("SHELL")); value != "" {
		return value
	}
	return "/bin/sh"
}

func (shellSpawner) BuildCommand(spec SpawnSpec) string {
	prefix := spec.EnvExports + spec.PreLaunchScript
	shell := clcommon.ShellQuoteArg(shellExecutable())
	if spec.InitialPrompt == "" {
		// tclaude-layer deliberately starts a new session without a controlling
		// terminal. `script` allocates a nested PTY, so bash/zsh can establish
		// job control instead of warning that the terminal process group is
		// absent.
		interactive := "exec " + shell + " -i"
		if runtime.GOOS == "darwin" {
			return prefix + "exec script -q /dev/null " + shell + " -i"
		}
		return prefix + "exec script -qefc " + clcommon.ShellQuoteArg(interactive) + " /dev/null"
	}
	return prefix + "exec " + shell + " -c " + clcommon.ShellQuoteArg(spec.InitialPrompt)
}

type shellModels struct{}

func (shellModels) ValidateModel(value string) (string, error) {
	if strings.TrimSpace(value) != "" {
		return "", fmt.Errorf("shell harness has no model")
	}
	return "", nil
}

func (shellModels) ValidateEffort(value string) (string, error) {
	if strings.TrimSpace(value) != "" {
		return "", fmt.Errorf("shell harness has no reasoning effort")
	}
	return "", nil
}

func (shellModels) Models() []string       { return nil }
func (shellModels) EffortLevels() []string { return nil }

const ShellSandboxOff = "off"

// A shell has no harness-native confinement. This catalog provides the
// explicit off posture required by the shared launch machinery; selecting the
// tclaude-layer implementation still wraps the command in tclaude's OS wall.
type shellSandbox struct{}

func (shellSandbox) DefaultMode() string { return ShellSandboxOff }
func (shellSandbox) Modes() []string     { return []string{ShellSandboxOff} }
func (shellSandbox) ModeHelp(mode string) string {
	if strings.TrimSpace(mode) == ShellSandboxOff {
		return "No harness-native sandbox. Choose tclaude’s sandbox implementation for OS confinement."
	}
	return ""
}
func (shellSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" || mode == ShellSandboxOff {
		return mode, nil
	}
	return "", fmt.Errorf("invalid shell sandbox mode %q (want %s)", mode, ShellSandboxOff)
}
