package harness

import (
	"fmt"
	"strings"
)

// Gemini CLI launch-containment modes.
//
// Gemini's own sandbox is selected by, in falling precedence
// (packages/cli/src/config/sandboxConfig.ts at GeminiPinnedVersion):
//
//  1. the GEMINI_SANDBOX environment variable — it OVERRIDES the flag;
//  2. the `--sandbox`/`-s` flag;
//  3. settings.json `tools.sandbox`.
//
// A truthy value re-executes the CLI inside docker/podman/runsc/lxc, or under
// macOS sandbox-exec. Two more environment facts matter: a set SANDBOX
// variable tells the CLI it is ALREADY inside its sandbox (it then never
// starts one, and on macOS relocates its runtime state), and nothing inside a
// running pane can change the posture — Gemini has no in-pane sandbox toggle.
//
// That gives tclaude a real per-launch lever, unlike Copilot's:
//
//   - inherit : tclaude sets nothing; the operator's settings and environment
//     decide. tclaude makes no containment claim for this mode.
//   - off     : the launch exports GEMINI_SANDBOX=false and an EMPTY SANDBOX, so
//     Gemini's own sandbox is OFF whatever settings.json says. This is
//     the posture tclaude-layer launches under, so tclaude's outer wall is
//     the single enforcement boundary. A container re-exec would move the
//     process — and the hook callback — out of the wall tclaude built.
//
// `--sandbox` itself is refused as a pass-through argument (see
// gemini_extra_args.go), so a launch cannot contradict the recorded mode.
const (
	GeminiSandboxInherit = "inherit"
	GeminiSandboxOff     = "off"

	// GeminiSandboxEnvVar selects Gemini CLI's own sandbox and outranks the
	// flag and settings.json.
	GeminiSandboxEnvVar = "GEMINI_SANDBOX"
	// geminiInSandboxEnvVar is set by Gemini inside its own sandbox. tclaude
	// exports it EMPTY rather than unsetting it: Gemini's .env loader only
	// fills variables that are absent, so an unset SANDBOX could be refilled
	// by a .env in the writable workspace (on macOS that relocates Gemini's
	// state out of the store tclaude reads), while an empty one reads as
	// false everywhere the CLI checks it.
	geminiInSandboxEnvVar = "SANDBOX"
)

// geminiSandbox is Gemini CLI's SandboxCatalog. DefaultMode is `inherit` for
// the reason Claude Code's and Copilot's are: the operator's own configuration
// is the trust root for a plain launch, and the secure posture for a daemon
// spawn is tclaude's outer layer, selected on the implementation axis.
type geminiSandbox struct{}

func (geminiSandbox) DefaultMode() string { return GeminiSandboxInherit }

func (geminiSandbox) Modes() []string {
	return []string{GeminiSandboxInherit, GeminiSandboxOff}
}

func (geminiSandbox) ValidateMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "":
		return "", nil
	case GeminiSandboxInherit:
		return GeminiSandboxInherit, nil
	case GeminiSandboxOff:
		return GeminiSandboxOff, nil
	default:
		return "", fmt.Errorf("invalid gemini sandbox mode %q (want %s|%s)",
			mode, GeminiSandboxInherit, GeminiSandboxOff)
	}
}

var geminiSandboxModeHelp = map[string]string{
	GeminiSandboxInherit: "Use your Gemini CLI sandbox settings as-is (settings.json `tools.sandbox` or GEMINI_SANDBOX). tclaude makes no containment claim for this mode; note that a container sandbox re-runs Gemini inside docker/podman, out of reach of tclaude's hooks.",
	GeminiSandboxOff:     "Gemini CLI's own sandbox is forced OFF for this launch (GEMINI_SANDBOX=false outranks settings.json), so tclaude’s built-in sandbox can be the single enforcement boundary. On its own, without tclaude’s sandbox, nothing confines the agent.",
}

func (geminiSandbox) ModeHelp(mode string) string {
	return geminiSandboxModeHelp[strings.TrimSpace(mode)]
}

// geminiSandboxEnvPrefix renders the environment a mode needs, placed directly
// before the binary so nothing earlier in the launch line can override it.
func geminiSandboxEnvPrefix(mode string) string {
	if strings.TrimSpace(mode) != GeminiSandboxOff {
		return ""
	}
	return "export " + GeminiSandboxEnvVar + "=false; export " + geminiInSandboxEnvVar + "=; "
}
