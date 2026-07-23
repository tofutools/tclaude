package harness

import (
	"fmt"
	"strings"
)

const (
	// OpenCodeSandboxAccessControl applies a tclaude-generated OpenCode
	// permission ruleset. It limits the built-in read/edit tools with validated
	// lexical path patterns, but it is not an OS sandbox: symlink traversal and
	// disk access through tools without path-scoped permission keys are not
	// contained.
	OpenCodeSandboxAccessControl = "access-control"

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
	return []string{OpenCodeSandboxAccessControl, OpenCodeSandboxOff}
}

func (openCodeSandbox) ModeHelp(mode string) string {
	switch strings.TrimSpace(mode) {
	case OpenCodeSandboxAccessControl:
		return "Lexical soft disk access control: built-in reads/edits follow relative path rules, while tools remain enabled. This is not an OS sandbox: it does not resolve or contain symlink targets, and bash/glob/grep can reach disk outside those lexical path rules."
	case OpenCodeSandboxOff:
		return "⚠ No directory scoping or OS containment. Filesystem/network sandbox profiles are incompatible and fail the launch. The selected tool approval policy still applies; bash is never auto-approved."
	default:
		return ""
	}
}

func (openCodeSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", OpenCodeSandboxAccessControl, OpenCodeSandboxOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid opencode sandbox mode %q (want %s|%s)",
			mode, OpenCodeSandboxAccessControl, OpenCodeSandboxOff)
	}
}

// openCodeSandboxWarnings returns the operator-facing line for an OpenCode
// launch whose selected sandbox mode gives a false impression of containment,
// or nil when the mode makes the lack of a sandbox self-evident.
//
// It fires for the `access-control` mode — OpenCode's DEFAULT, and the mode a
// blank spawn resolves to — because that mode reads like a sandbox but is not
// one. tclaude's filesystem/network sandbox profiles, when attached to an
// OpenCode agent, are compiled into these same soft rules rather than into an
// OS sandbox (bubblewrap/Seatbelt) the way Claude Code's and Codex's are, so
// "sandboxing" an OpenCode agent does not confine it. The `off` mode does not
// warn here: it already carries its own ⚠ in ModeHelp and the operator has
// explicitly disabled scoping, so there is no false sense of security to
// correct.
//
// The concrete failure modes named here — shell redirection, symlinks, and
// subprocess binaries reaching disk outside the allowed paths — match the
// OpenCode shell implementation, which only inspects path arguments of a fixed
// built-in command set and omits redirection targets.
func openCodeSandboxWarnings(sandboxMode string) []string {
	if strings.TrimSpace(sandboxMode) != OpenCodeSandboxAccessControl {
		return nil
	}
	return []string{
		"⚠ OpenCode has no built-in OS sandbox. The \"access-control\" mode only " +
			"lexically checks path arguments of a fixed set of built-in tool commands, " +
			"so shell redirection, symlinks, and subprocesses still reach files and the " +
			"network outside the allowed directories. Treat this agent as effectively " +
			"unsandboxed — use a container or a restricted OS account for real isolation " +
			"— until tclaude adds an OpenCode sandbox layer.",
	}
}
