package harness

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const (
	// OpenCodeSandboxAccessControl applies a tclaude-generated OpenCode
	// permission ruleset. It limits the built-in read/edit tools with validated
	// lexical path patterns, but it is not an OS sandbox: symlink traversal and
	// disk access through tools without path-scoped permission keys are not
	// contained.
	OpenCodeSandboxAccessControl = "access-control"

	// OpenCodeSandboxTclaudeLayer records that the agentd-owned, tool-executing
	// OpenCode server runs inside tclaude's OS boundary. OpenCode's ordered
	// permission rules remain active as defense in depth.
	OpenCodeSandboxTclaudeLayer = "tclaude-layer"

	// OpenCodeSandboxOff disables directory scoping. Approval policy still
	// applies, so selecting off does not erase the fail-closed tool posture.
	OpenCodeSandboxOff = "off"
)

// openCodeSandbox surfaces both tclaude's soft access-control policy and the
// explicit no-scoping posture. Keeping them in a real catalog makes the
// distinction visible in spawn/profile UIs and persistable in launch profiles
// without misrepresenting either one as an OS sandbox.
type openCodeSandbox struct{}

func (openCodeSandbox) DefaultMode() string { return OpenCodeSandboxAccessControl }

func (openCodeSandbox) Modes() []string {
	return []string{OpenCodeSandboxAccessControl, OpenCodeSandboxTclaudeLayer, OpenCodeSandboxOff}
}

func (openCodeSandbox) ModeHelp(mode string) string {
	switch strings.TrimSpace(mode) {
	case OpenCodeSandboxAccessControl:
		return "Lexical soft disk access control: built-in reads/edits follow relative path rules, while tools remain enabled. This is not an OS sandbox: it does not resolve or contain symlink targets, and bash/glob/grep can reach disk outside those lexical path rules."
	case OpenCodeSandboxTclaudeLayer:
		return "Linux/macOS OS containment for the agentd-owned, tool-executing OpenCode server. The attach pane stays outside the boundary; authenticated loopback control traffic, host networking, and ambient host Unix sockets remain reachable. Ordered OpenCode permission rules stay active as defense in depth."
	case OpenCodeSandboxOff:
		return "⚠ No directory scoping or OS containment. Filesystem/network sandbox profiles are incompatible and fail the launch. The selected tool approval policy still applies; bash is never auto-approved."
	default:
		return ""
	}
}

func (openCodeSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", OpenCodeSandboxAccessControl, OpenCodeSandboxTclaudeLayer, OpenCodeSandboxOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid opencode sandbox mode %q (want %s|%s|%s)",
			mode, OpenCodeSandboxAccessControl, OpenCodeSandboxTclaudeLayer, OpenCodeSandboxOff)
	}
}

// ResolveOpenCodeSandboxImplementationMode keeps OpenCode's persisted sandbox
// mode truthful about which component owns OS confinement. The normal
// access-control default becomes tclaude-layer when the outer implementation
// is selected; contradictory explicit pairs fail rather than silently choosing
// one axis.
func ResolveOpenCodeSandboxImplementationMode(
	harnessName, mode string,
	implementation sandboxpolicy.Implementation,
) (string, error) {
	mode = strings.TrimSpace(mode)
	if harnessName != OpenCodeName {
		return mode, nil
	}
	if implementation == sandboxpolicy.ImplementationTclaudeLayer {
		switch mode {
		case "", OpenCodeSandboxAccessControl, OpenCodeSandboxTclaudeLayer:
			return OpenCodeSandboxTclaudeLayer, nil
		case OpenCodeSandboxOff:
			return "", fmt.Errorf(
				"OpenCode sandbox %q is incompatible with sandbox implementation %q",
				mode, implementation)
		default:
			return "", fmt.Errorf("invalid opencode sandbox mode %q", mode)
		}
	}
	if mode == OpenCodeSandboxTclaudeLayer {
		return "", fmt.Errorf(
			"OpenCode sandbox %q requires sandbox implementation %q",
			mode, sandboxpolicy.ImplementationTclaudeLayer)
	}
	return mode, nil
}

// openCodeSandboxWarnings returns the operator-facing line for an OpenCode
// launch whose selected sandbox mode needs a containment caveat, or nil when
// the explicit off mode is already self-explanatory.
//
// It fires for the `access-control` mode — OpenCode's DEFAULT, and the mode a
// blank spawn resolves to — because that mode reads like a sandbox but is not
// one. tclaude's filesystem/network sandbox profiles alone compile into these
// same soft rules. The `tclaude-layer` mode instead names its split boundary:
// the tool-executing server is confined while the attach pane and control plane
// stay outside/reachable. The `off` mode already carries its own ⚠ in ModeHelp.
//
// The concrete failure modes named here — shell redirection, symlinks, and
// subprocess binaries reaching disk outside the allowed paths — match the
// OpenCode shell implementation, which only inspects path arguments of a fixed
// built-in command set and omits redirection targets.
func openCodeSandboxWarnings(sandboxMode string) []string {
	switch strings.TrimSpace(sandboxMode) {
	case OpenCodeSandboxTclaudeLayer:
		warnings := []string{
			"ℹ OpenCode's tool-executing server runs inside tclaude-layer; the attach pane remains outside the boundary, and the authenticated loopback control plane remains reachable.",
		}
		if runtime.GOOS == "darwin" {
			warnings = append(warnings,
				"⚠ On macOS, per-agent mutable XDG privacy covers OpenCode data/cache/state only; "+
					"the config base is not redirected, so non-OpenCode config writes inside the wall target the real host config base and remain governed by the filesystem policy.")
		}
		return warnings
	case OpenCodeSandboxAccessControl:
	default:
		return nil
	}
	return []string{
		"⚠ OpenCode has no built-in OS sandbox. The \"access-control\" mode is a command " +
			"filter, not confinement: it only lexically checks path arguments of a fixed set of built-in tool commands, " +
			"so shell redirection, symlinks, and subprocesses still reach files and the " +
			"network outside the allowed directories. Treat this agent as effectively " +
			"unsandboxed — use a container or a restricted OS account for real isolation " +
			"— or select the tclaude-layer sandbox implementation on Linux or macOS.",
	}
}
