package harness

import (
	"fmt"
	"runtime"
	"slices"
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
//     It also exports NODE_USE_ENV_PROXY=1 (geminiNodeEnvProxyVar).
//   - seatbelt: macOS only, the harness-builtin posture. The launch exports
//     GEMINI_SANDBOX=sandbox-exec and SEATBELT_PROFILE=permissive-open, so
//     Gemini re-executes itself under its own Seatbelt profile: writes are
//     confined to the project, the temp and cache directories and
//     ~/.npm, ~/.gemini and the credential files are write-protected, and
//     outbound network stays open. Two consequences tclaude accounts for:
//     Gemini's hooks run INSIDE that profile, so their callbacks are
//     brokered through agentd (the database is not writable from in
//     there), and Gemini keeps its chats under ~/.cache/.gemini instead
//     of ~/.gemini (Storage.getGlobalRuntimeDir), which the conversation
//     store also reads.
//
// `--sandbox` itself is refused as a pass-through argument (see
// gemini_extra_args.go), so a launch cannot contradict the recorded mode.
const (
	GeminiSandboxInherit  = "inherit"
	GeminiSandboxOff      = "off"
	GeminiSandboxSeatbelt = "seatbelt"

	// geminiSeatbeltCommand and geminiSeatbeltProfile are the GEMINI_SANDBOX
	// and SEATBELT_PROFILE values the seatbelt mode pins.
	geminiSeatbeltCommand    = "sandbox-exec"
	geminiSeatbeltProfile    = "permissive-open"
	geminiSeatbeltProfileVar = "SEATBELT_PROFILE"

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

// GeminiBuiltinOSSandboxAbsenceReason is the harness-builtin refusal on a host
// without sandbox-exec.
const GeminiBuiltinOSSandboxAbsenceReason = "Gemini CLI's own OS sandbox here is a container " +
	"(docker/podman), which runs the agent outside tclaude's hooks and reasoning; its Seatbelt " +
	"sandbox exists only on macOS. Use --sandbox-impl tclaude-layer for an OS sandbox"

// geminiSandbox is Gemini CLI's SandboxCatalog. DefaultMode is `inherit` for
// the reason Claude Code's and Copilot's are: the operator's own configuration
// is the trust root for a plain launch, and the secure posture for a daemon
// spawn is tclaude's outer layer, selected on the implementation axis.
type geminiSandbox struct{}

func (geminiSandbox) DefaultMode() string { return GeminiSandboxInherit }

// geminiSeatbeltAvailable reports whether Gemini's Seatbelt sandbox exists on
// this host: sandbox-exec is macOS's.
var geminiSeatbeltAvailable = runtime.GOOS == "darwin"

// SetGeminiSeatbeltAvailableForTest overrides Seatbelt availability for the
// mode catalog and returns the restore func, so a test that pins catalog
// output (the dashboard's mode-help fixture) reads the same on every host.
func SetGeminiSeatbeltAvailableForTest(available bool) (restore func()) {
	prior := geminiSeatbeltAvailable
	geminiSeatbeltAvailable = available
	return func() { geminiSeatbeltAvailable = prior }
}

func geminiSandboxModes() []string {
	if geminiSeatbeltAvailable {
		return []string{GeminiSandboxInherit, GeminiSandboxOff, GeminiSandboxSeatbelt}
	}
	return []string{GeminiSandboxInherit, GeminiSandboxOff}
}

func (geminiSandbox) Modes() []string { return geminiSandboxModes() }

func (geminiSandbox) ValidateMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	switch {
	case mode == "":
		return "", nil
	case slices.Contains(geminiSandboxModes(), mode):
		return mode, nil
	case mode == GeminiSandboxSeatbelt:
		return "", fmt.Errorf("gemini sandbox mode %q is Gemini CLI's macOS Seatbelt sandbox and "+
			"is not available on %s; use --sandbox-impl tclaude-layer for an OS sandbox here", mode, runtime.GOOS)
	default:
		return "", fmt.Errorf("invalid gemini sandbox mode %q (want %s)",
			mode, strings.Join(geminiSandboxModes(), "|"))
	}
}

var geminiSandboxModeHelp = map[string]string{
	GeminiSandboxInherit: "Use your Gemini CLI sandbox settings as-is (settings.json `tools.sandbox` or GEMINI_SANDBOX). tclaude makes no containment claim for this mode; note that a container sandbox re-runs Gemini inside docker/podman, out of reach of tclaude's hooks.",
	GeminiSandboxOff:     "Gemini CLI's own sandbox is forced OFF for this launch (GEMINI_SANDBOX=false outranks settings.json), so tclaude’s built-in sandbox can be the single enforcement boundary. On its own, without tclaude’s sandbox, nothing confines the agent.",
	GeminiSandboxSeatbelt: "Gemini CLI's own macOS Seatbelt sandbox (sandbox-exec, profile permissive-open): writes are confined to the project, temp and cache directories, Gemini's config and credential files are write-protected, and outbound network stays open. Needs an API-key or Vertex AI sign-in: the profile hides Google sign-in credentials from the sandboxed CLI. " +
		"⚠ The profile does not restrict Unix sockets, so the agent can reach tclaude's tmux server and run commands outside the sandbox through it, and it can read ~/.tclaude; git writes to a linked worktree's main repository are denied; and chats live under ~/.cache/.gemini, so a conversation started in another mode is not resumable in this one. Use --sandbox-impl tclaude-layer for a sandbox tclaude enforces.",
}

func (geminiSandbox) ModeHelp(mode string) string {
	return geminiSandboxModeHelp[strings.TrimSpace(mode)]
}

// geminiNodeEnvProxyVar makes Node's built-in fetch honour HTTP(S)_PROXY.
// Gemini means to honour those variables (it installs a proxy dispatcher from
// them), but measured with 0.62.0 on Node 26 its model client ignores them
// unless this is set, so behind tclaude's proxy network engine it dials
// directly into the empty namespace and every model call fails. Set for the
// `off` mode only, the one every tclaude-layer launch uses, so it never
// reaches a pane outside tclaude's sandbox through a mode it did not ask for.
// It is inherited by the agent's own Node commands, and it has no effect
// unless a proxy variable is set.
const geminiNodeEnvProxyVar = "NODE_USE_ENV_PROXY"

// geminiSandboxEnvPrefix renders the environment a mode needs, placed directly
// before the binary so nothing earlier in the launch line can override it.
func geminiSandboxEnvPrefix(mode string) string {
	switch strings.TrimSpace(mode) {
	case GeminiSandboxOff:
		return "export " + GeminiSandboxEnvVar + "=false; export " + geminiInSandboxEnvVar + "=; export " +
			geminiNodeEnvProxyVar + "=1; "
	case GeminiSandboxSeatbelt:
		// SANDBOX is exported empty for the same reason as in `off`: unset,
		// a .env could refill it, and a set SANDBOX tells Gemini it is
		// already inside its sandbox — so it would never start one while
		// tclaude recorded the launch as confined.
		return "export " + GeminiSandboxEnvVar + "=" + geminiSeatbeltCommand +
			"; export " + geminiSeatbeltProfileVar + "=" + geminiSeatbeltProfile +
			"; export " + geminiInSandboxEnvVar + "=; "
	default:
		return ""
	}
}

// HooksRunInsideBuiltinSandbox reports whether a harness-builtin launch runs
// its hook commands inside the harness's own OS sandbox, where tclaude's
// database is not writable — so the launch must broker hook callbacks through
// agentd exactly as a tclaude-layer launch does. Claude Code and Codex run
// hooks outside their command sandbox; Gemini runs them in its Seatbelt child.
func HooksRunInsideBuiltinSandbox(h *Harness, mode string) bool {
	return h != nil && h.Name == GeminiName && strings.TrimSpace(mode) == GeminiSandboxSeatbelt
}
