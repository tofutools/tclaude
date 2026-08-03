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
// Linux) is configured only by the `sandbox` key of TWO files under
// COPILOT_HOME — `settings.json` and the legacy `config.json`, the latter
// winning (see CopilotConfigFileName) — and by the in-pane
// `/sandbox enable|disable` command, which is itself only registered when
// experimental features are on. There is no launch flag and no environment
// variable that turns it on or off (`copilot --help`, `copilot help sandbox`,
// `copilot help environment`, pinned 1.0.77).
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
	CopilotSandboxInherit: "Use your Copilot `sandbox` posture as-is. Copilot's own command sandbox is experimental and off by default, and tclaude makes no containment claim for this mode. Copilot exposes no launch flag for it, so tclaude can neither enable nor disable it per session.",
	CopilotSandboxOff:     "Copilot's own (experimental, MXC) command sandbox is asserted NOT engaged, so tclaude's built-in OS sandbox is the single enforcement boundary. The launch is REFUSED — not silently downgraded — when Copilot's settings.json or its legacy config.json (which wins) enables that sandbox, is unreadable or ambiguous, or leaves experimental features on (which registers the in-pane `/sandbox enable` command).",
}

func (copilotSandbox) ModeHelp(mode string) string {
	return copilotSandboxModeHelp[strings.TrimSpace(mode)]
}

// CopilotSettingsFileName is the CANONICAL settings file under COPILOT_HOME.
// Named here rather than inlined because the assert-off contract, its refusal
// messages, and the sandbox baseline's state-directory row all have to agree
// on it.
const CopilotSettingsFileName = "settings.json"

// CopilotConfigFileName is the LEGACY settings file, and reading it is not
// optional — measured against the pinned 1.0.77 binary, it beats
// settings.json:
//
//	sandbox key only in config.json                 -> engaged
//	sandbox key only in settings.json               -> engaged
//	config.json true  + settings.json false         -> engaged
//	config.json false + settings.json true          -> NOT engaged
//
// The mechanism is a startup migration: the CLI moves user settings out of
// config.json INTO settings.json, overwriting what was there, then rewrites
// config.json to a managed stub. So config.json is not dead legacy — it is a
// pending mutation of settings.json that applies to the very launch tclaude is
// about to start.
//
// A gate reading only settings.json is therefore bypassable twice over: by
// dropping a config.json before an otherwise clean launch, and by leaving
// `sandbox.enabled: false` in settings.json where it reads as a determinate
// off while config.json is about to overwrite it.
const CopilotConfigFileName = "config.json"

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
	// SettingsPath is the canonical settings file, inspected whether or not it
	// exists.
	SettingsPath string
	// ConfigPath is the legacy settings file, inspected on the same terms. It
	// WINS over SettingsPath for any key both carry; see CopilotConfigFileName.
	ConfigPath string
	// Present reports whether either file exists.
	Present bool
	// Enabled is the effective value of `sandbox.enabled`, defaulting to false
	// — the CLI's own documented default.
	Enabled bool
	// EnabledSource is the file the effective Enabled value came from, so a
	// refusal can name the file an operator has to edit. Empty when no file
	// set the key.
	EnabledSource string
	// Experimental is the effective value of the top-level `experimental` key.
	// It matters because `/sandbox` (and therefore `/sandbox enable`) is only
	// registered when experimental features are on, so it is the difference
	// between a launch whose inner wall cannot be turned on from the pane and
	// one whose can.
	//
	// It is NOT evidence in the other direction: a settings-enabled sandbox
	// applies with no experimental flag anywhere, so `experimental: false`
	// proves nothing about whether the wall is up. That is what Enabled is for.
	Experimental bool
	// ExperimentalSource is the file the effective Experimental value came from.
	ExperimentalSource string
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
		ConfigPath:   filepath.Join(stateDir, CopilotConfigFileName),
	}

	// Read in PRECEDENCE ORDER, weakest first, so the stronger file's values
	// simply overwrite. config.json last, because it wins.
	for _, path := range []string{state.SettingsPath, state.ConfigPath} {
		file, found, err := readCopilotSandboxFile(path)
		if err != nil {
			return CopilotInnerSandboxState{}, err
		}
		if !found {
			continue
		}
		state.Present = true
		if file.enabledSet {
			state.Enabled = file.enabled
			state.EnabledSource = path
		}
		if file.experimentalSet {
			state.Experimental = file.experimental
			state.ExperimentalSource = path
		}
	}
	return state, nil
}

// copilotSandboxFile is one file's contribution to the effective posture. The
// *Set flags are what make precedence expressible: "this file did not mention
// the key" and "this file said false" are different inputs to a merge, and
// collapsing them would let the weaker file's explicit false be silently
// re-asserted over the stronger file's true.
type copilotSandboxFile struct {
	enabled, enabledSet           bool
	experimental, experimentalSet bool
}

// readCopilotSandboxFile parses one of Copilot's two settings files. A missing
// file is (zero, false, nil) — absence is not ambiguity, and the CLI documents
// the sandbox as off by default. Everything else that stops tclaude from
// reading a determinate answer is a refusal.
func readCopilotSandboxFile(path string) (copilotSandboxFile, bool, error) {
	var out copilotSandboxFile
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, false, nil
	}
	if err != nil {
		return out, false, copilotInnerSandboxError(fmt.Sprintf(
			"cannot read Copilot settings %s: %v; tclaude cannot tell whether Copilot's own "+
				"command sandbox would also be engaged, so the launch is refused rather than "+
				"claiming a single boundary it did not verify", path, err))
	}

	// Decoded into RawMessage rather than a typed struct so an unexpected SHAPE
	// (`"sandbox": true`, `"sandbox": "on"`, `enabled: "true"`) is an explicit
	// refusal instead of silently unmarshalling to the zero value — which for a
	// boolean named `enabled` would read as "off" and let the very config this
	// gate exists to catch through.
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(stripCopilotLineComments(data), &settings); err != nil {
		return out, false, copilotInnerSandboxError(fmt.Sprintf(
			"cannot parse Copilot settings %s as a JSON object: %v; fix the file or use "+
				"another sandbox posture", path, err))
	}
	if raw, found := settings["experimental"]; found {
		if err := json.Unmarshal(raw, &out.experimental); err != nil {
			return out, false, copilotInnerSandboxError(fmt.Sprintf(
				"Copilot settings %s has a non-boolean `experimental` value; fix the file or use "+
					"another sandbox posture", path))
		}
		out.experimentalSet = true
	}
	raw, found := settings["sandbox"]
	if !found {
		return out, true, nil
	}
	var sandbox map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sandbox); err != nil {
		return out, false, copilotInnerSandboxError(fmt.Sprintf(
			"Copilot settings %s has a `sandbox` value that is not an object; tclaude cannot "+
				"determine whether Copilot's own command sandbox is engaged", path))
	}
	if raw, found := sandbox["enabled"]; found {
		if err := json.Unmarshal(raw, &out.enabled); err != nil {
			return out, false, copilotInnerSandboxError(fmt.Sprintf(
				"Copilot settings %s has a non-boolean `sandbox.enabled` value; tclaude cannot "+
					"determine whether Copilot's own command sandbox is engaged", path))
		}
		out.enabledSet = true
	}
	return out, true, nil
}

// stripCopilotLineComments drops WHOLE-LINE `//` comments.
//
// This is not leniency for its own sake: after the startup migration the CLI
// rewrites config.json to a stub that opens with two such comment lines
// ("// User settings belong in settings.json."). That stub is what a normal,
// settled install has on disk, so a strict JSON parse would refuse the most
// common posture there is.
//
// Only lines whose first non-space characters are `//` are removed, so a `//`
// inside a string value — every https:// URL in the file — is untouched.
func stripCopilotLineComments(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
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
				"both. Set `sandbox.enabled` to false in THAT file (or run `/sandbox disable` "+
				"once) to launch under tclaude's boundary, or launch with the harness-builtin "+
				"sandbox implementation to use Copilot's own",
			copilotSourceOr(state.EnabledSource, state.SettingsPath)))
	}
	if state.Experimental {
		return copilotInnerSandboxError(fmt.Sprintf(
			"Copilot experimental features are enabled by `experimental` in %s, which registers "+
				"the in-pane `/sandbox` command, so Copilot's own command sandbox could be turned "+
				"on inside tclaude's boundary mid-session. Copilot exposes no per-launch override "+
				"tclaude can apply without writing your configuration, so the launch is refused "+
				"rather than asserting a single boundary that stops holding when someone types "+
				"`/sandbox enable`. Set `experimental` to false to launch under tclaude's "+
				"boundary", copilotSourceOr(state.ExperimentalSource, state.SettingsPath)))
	}
	return nil
}

// copilotSourceOr names the file that actually set the offending key. With two
// files in play and the legacy one winning, "edit settings.json" would send an
// operator to a file whose value is about to be overwritten.
func copilotSourceOr(source, fallback string) string {
	if strings.TrimSpace(source) != "" {
		return source
	}
	return fallback
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
