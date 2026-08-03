package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GitHub Copilot CLI launch-containment modes (TCL-978).
//
// Copilot is unlike every harness tclaude already models, and the difference
// decides the whole shape of this file. Claude Code takes a per-session
// `--settings` override; Codex takes a `--sandbox` flag; OpenCode's server is
// tclaude-launched. Copilot CLI 1.0.77 takes NONE of those: its command
// sandbox (Microsoft Execution Containers — Seatbelt on macOS, bubblewrap on
// Linux) is configured only by the `sandbox` key of
// `<COPILOT_HOME>/settings.json` and by the in-pane `/sandbox enable|disable`
// command, which is itself only registered when experimental features are on.
// There is no launch flag and no environment variable that turns it on or off
// (`copilot --help`, `copilot help sandbox`, `copilot help environment`,
// pinned 1.0.77).
//
// So tclaude cannot FORCE Copilot's inner wall off for one launch. What it can
// do — and what these modes mean — is ASSERT that the wall is off and refuse
// the launch when it cannot prove that:
//
//   - inherit : tclaude asserts nothing about Copilot's own sandbox. The
//     operator's settings.json posture applies unchanged, and tclaude
//     makes no containment claim for it. This is the default, so a
//     plain `--harness copilot` spawn behaves exactly as before.
//   - off     : the inner MXC sandbox is proven NOT engaged for this launch, so
//     a tclaude-layer launch has exactly one claimed enforcement
//     boundary — tclaude's own. Selecting it runs the assert-off
//     contract below; a conflicting or unreadable Copilot config
//     REFUSES the launch rather than launching with two claimed walls
//     or with a wall tclaude cannot see.
//
// There is deliberately no `on` mode. Enabling Copilot's own sandbox is
// TCL-977's subject, it needs a settings write tclaude does not perform here,
// and a mode that claimed to enable a boundary tclaude has no lever for would
// be exactly the lie the SandboxCatalog contract exists to prevent.
const (
	CopilotSandboxInherit = "inherit"
	CopilotSandboxOff     = "off"
)

// copilotSandbox is Copilot CLI's SandboxCatalog.
//
// DefaultMode is `inherit` for the same reason Claude Code's is: the human's
// own configuration is the trust root, and a daemon spawn that silently
// asserted a posture the operator never chose would either refuse launches
// that used to work or claim containment nobody configured. The secure
// posture for an agentd-spawned Copilot agent is tclaude's OUTER layer, which
// is selected by the sandbox IMPLEMENTATION axis (`--sandbox-impl
// tclaude-layer`) and resolves this catalog to `off` through
// TclaudeLayerSandboxMode.
type copilotSandbox struct{}

func (copilotSandbox) DefaultMode() string { return CopilotSandboxInherit }

func (copilotSandbox) Modes() []string {
	return []string{CopilotSandboxInherit, CopilotSandboxOff}
}

func (copilotSandbox) ValidateMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "":
		return "", nil
	case CopilotSandboxInherit:
		return CopilotSandboxInherit, nil
	case CopilotSandboxOff:
		return CopilotSandboxOff, nil
	default:
		return "", fmt.Errorf("invalid copilot sandbox mode %q (want %s|%s)",
			mode, CopilotSandboxInherit, CopilotSandboxOff)
	}
}

var copilotSandboxModeHelp = map[string]string{
	CopilotSandboxInherit: "Use your Copilot settings.json `sandbox` posture as-is. Copilot's own command sandbox is experimental and off by default, and tclaude makes no containment claim for this mode. Copilot exposes no launch flag for it, so tclaude can neither enable nor disable it per session.",
	CopilotSandboxOff:     "Copilot's own (experimental, MXC) command sandbox is asserted NOT engaged, so tclaude's built-in OS sandbox is the single enforcement boundary. The launch is REFUSED — not silently downgraded — when Copilot's settings.json enables that sandbox, is unreadable or ambiguous, or leaves experimental features on (which registers the in-pane `/sandbox enable` command).",
}

func (copilotSandbox) ModeHelp(mode string) string {
	return copilotSandboxModeHelp[strings.TrimSpace(mode)]
}

// CopilotSettingsFileName is the file Copilot stores its configuration in,
// under COPILOT_HOME. Named here rather than inlined because the assert-off
// contract, its refusal messages, and the sandbox baseline's state-directory
// row all have to agree on it.
const CopilotSettingsFileName = "settings.json"

// SandboxCapabilityCopilotInnerSandbox is the stable wire vocabulary for a
// refusal caused by Copilot's own sandbox posture rather than by tclaude's.
// Kept distinct from the baseline's kinds so a daemon can render the specific
// remedy (edit Copilot's settings) instead of the generic one.
const SandboxCapabilityCopilotInnerSandbox = "copilot-inner-sandbox-conflict"

// CopilotInnerSandboxState is what tclaude could read about Copilot's own
// command sandbox before a launch.
//
// Present distinguishes "the operator has no settings.json" (the common case,
// and an unambiguous off — the CLI documents the sandbox as disabled by
// default) from "the file exists and says nothing about the sandbox". Both are
// off, but only the second proves the operator looked.
type CopilotInnerSandboxState struct {
	// SettingsPath is the file that was inspected, whether or not it existed.
	SettingsPath string
	// Present reports whether SettingsPath exists.
	Present bool
	// Enabled is the value of `sandbox.enabled`, defaulting to false — the
	// CLI's own documented default.
	Enabled bool
	// Experimental is the value of the top-level `experimental` key. It
	// matters because `/sandbox` (and therefore `/sandbox enable`) is only
	// registered when experimental features are on, so it is the difference
	// between a launch whose inner wall cannot be turned on from the pane and
	// one whose can.
	Experimental bool
}

// ResolveCopilotInnerSandbox reads Copilot's own sandbox posture from the
// settings file the launch will actually use.
//
// getenv/home resolve COPILOT_HOME exactly the way copilotStateDir does, so
// this function and the sandbox baseline can never disagree about which
// directory a launch is confined to and which settings file governs it.
//
// The error cases are all AMBIGUITY, not absence: a missing file is a clean
// "off", while a file that cannot be read, cannot be parsed, or carries a
// `sandbox` block of an unexpected shape leaves tclaude unable to say whether
// a second wall is about to be engaged. Those return a *SandboxCapabilityError
// so the caller can surface a stable code rather than a read error.
func ResolveCopilotInnerSandbox(
	getenv func(string) string,
	home string,
) (CopilotInnerSandboxState, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "" || home == "." || !filepath.IsAbs(home) {
		return CopilotInnerSandboxState{}, copilotInnerSandboxError(
			fmt.Sprintf("Copilot's sandbox posture needs an absolute home directory to resolve "+
				"COPILOT_HOME from, got %q", home))
	}
	stateDir, _ := copilotStateDir(getenv, home)
	state := CopilotInnerSandboxState{
		SettingsPath: filepath.Join(stateDir, CopilotSettingsFileName),
	}
	data, err := os.ReadFile(state.SettingsPath)
	if errors.Is(err, os.ErrNotExist) {
		// No settings file at all. Copilot documents its command sandbox as
		// disabled by default, so this is a determinate off — and it is by far
		// the most common shape, since the sandbox is experimental.
		return state, nil
	}
	if err != nil {
		return CopilotInnerSandboxState{}, copilotInnerSandboxError(fmt.Sprintf(
			"cannot read Copilot settings %s: %v; tclaude cannot tell whether Copilot's own "+
				"command sandbox would also be engaged, so the launch is refused rather than "+
				"claiming a single boundary it did not verify", state.SettingsPath, err))
	}
	state.Present = true

	// Decoded into RawMessage rather than a typed struct so an unexpected SHAPE
	// (`"sandbox": true`, `"sandbox": "on"`, `enabled: "true"`) is an explicit
	// refusal instead of silently unmarshalling to the zero value — which for a
	// boolean named `enabled` would read as "off" and let the very config this
	// gate exists to catch through.
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return CopilotInnerSandboxState{}, copilotInnerSandboxError(fmt.Sprintf(
			"cannot parse Copilot settings %s as a JSON object: %v; fix the file or use "+
				"another sandbox posture", state.SettingsPath, err))
	}
	if raw, found := settings["experimental"]; found {
		if err := json.Unmarshal(raw, &state.Experimental); err != nil {
			return CopilotInnerSandboxState{}, copilotInnerSandboxError(fmt.Sprintf(
				"Copilot settings %s has a non-boolean `experimental` value; fix the file or use "+
					"another sandbox posture", state.SettingsPath))
		}
	}
	raw, found := settings["sandbox"]
	if !found {
		return state, nil
	}
	var sandbox map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sandbox); err != nil {
		return CopilotInnerSandboxState{}, copilotInnerSandboxError(fmt.Sprintf(
			"Copilot settings %s has a `sandbox` value that is not an object; tclaude cannot "+
				"determine whether Copilot's own command sandbox is engaged", state.SettingsPath))
	}
	if raw, found := sandbox["enabled"]; found {
		if err := json.Unmarshal(raw, &state.Enabled); err != nil {
			return CopilotInnerSandboxState{}, copilotInnerSandboxError(fmt.Sprintf(
				"Copilot settings %s has a non-boolean `sandbox.enabled` value; tclaude cannot "+
					"determine whether Copilot's own command sandbox is engaged", state.SettingsPath))
		}
	}
	return state, nil
}

// ValidateCopilotTclaudeLayerInnerSandbox is the assert-off gate: it accepts a
// launch only when Copilot's own command sandbox is provably not engaged and
// cannot be engaged from the pane's launch posture.
//
// The two refusals are different failures and say so:
//
//   - `sandbox.enabled: true` is a SECOND WALL. Launching anyway would stack a
//     Copilot-authored policy inside tclaude's, so the operator's confinement
//     would be the intersection of two policies neither of which was reviewed
//     against the other, and the recorded posture would name only one of them.
//   - `experimental: true` REGISTERS `/sandbox`, so `/sandbox enable` can turn
//     the inner wall on mid-session. tclaude has no immutable per-launch
//     override to prevent that: `--no-experimental` exists as a flag, but
//     Copilot documents flags of that family as PERSISTING the preference to
//     the operator's configuration, and this contract does not write operator
//     state. So the launch is refused rather than started on an assertion that
//     stops holding the moment someone types six characters.
//
// The refusal names the file and the exact key, because "your Copilot config
// conflicts" is not something an operator can act on and "set sandbox.enabled
// to false in ~/.copilot/settings.json" is.
func ValidateCopilotTclaudeLayerInnerSandbox(state CopilotInnerSandboxState) error {
	if state.Enabled {
		return copilotInnerSandboxError(fmt.Sprintf(
			"Copilot's own command sandbox is enabled by `sandbox.enabled` in %s, and tclaude's "+
				"outer layer is already the enforcement boundary for this launch; two stacked "+
				"policies would make the effective confinement the unreviewed intersection of "+
				"both. Set `sandbox.enabled` to false (or run `/sandbox disable` once) to launch "+
				"under tclaude's boundary, or launch with the harness-builtin sandbox "+
				"implementation to use Copilot's own", state.SettingsPath))
	}
	if state.Experimental {
		return copilotInnerSandboxError(fmt.Sprintf(
			"Copilot experimental features are enabled by `experimental` in %s, which registers "+
				"the in-pane `/sandbox` command, so Copilot's own command sandbox could be turned "+
				"on inside tclaude's boundary mid-session. Copilot exposes no per-launch override "+
				"tclaude can apply without writing your configuration, so the launch is refused "+
				"rather than asserting a single boundary that stops holding when someone types "+
				"`/sandbox enable`. Set `experimental` to false to launch under tclaude's "+
				"boundary", state.SettingsPath))
	}
	return nil
}

// CopilotTclaudeLayerExtraArgRefusal refuses pass-through launch arguments that
// would defeat the assert-off contract from the argv side.
//
// The settings gate above reads a FILE; `--experimental` re-enables the same
// in-pane `/sandbox` command from the command line, so a gate that inspected
// only the file would be trivially bypassed by a spawn that passed the flag.
// `--yolo` / `--allow-all` are deliberately NOT refused here: they widen
// Copilot's own permission prompts, which is an autonomy choice the outer wall
// still contains, not a second enforcement boundary.
func CopilotTclaudeLayerExtraArgRefusal(extraArgs []string) error {
	for _, argument := range extraArgs {
		name := argument
		if before, _, found := strings.Cut(argument, "="); found {
			name = before
		}
		if strings.TrimSpace(name) == "--experimental" {
			return copilotInnerSandboxError(
				"pass-through argument `--experimental` registers Copilot's in-pane `/sandbox` " +
					"command, which could engage Copilot's own command sandbox inside tclaude's " +
					"boundary; remove the argument to launch under tclaude's boundary")
		}
	}
	return nil
}

func copilotInnerSandboxError(message string) *SandboxCapabilityError {
	return &SandboxCapabilityError{
		Harness: CopilotName,
		Kind:    SandboxCapabilityCopilotInnerSandbox,
		Message: message,
	}
}
