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
// tclaude-launched. Copilot CLI 1.0.77 takes none of those on terms tclaude can
// use: its command sandbox (Microsoft Execution Containers — Seatbelt on macOS,
// bubblewrap on Linux) is configured by the `sandbox` key of TWO files under
// COPILOT_HOME — `settings.json` and the legacy `config.json`, the latter
// winning (see CopilotConfigFileName) — and by the in-pane
// `/sandbox enable|disable` command, which is itself only registered when
// experimental features are on.
//
// There ARE hidden per-launch flags, and the earlier form of this comment was
// wrong to say otherwise: `--sandbox` and `--no-sandbox` were added in 1.0.70
// and are absent from `copilot --help`. They do not change the conclusion,
// because of the gate measured in TCL-1011 (see
// copilotfixture/sandbox_native_flags_smoke_test.go): without `--experimental`
// both flags are parsed and then IGNORED, in both directions, and WITH it they
// override the settings file for one launch without persisting. Since
// `--experimental` is also what registers `/sandbox enable|disable`, the only
// argv that selects a posture is the same argv that lets the pane revoke it —
// which is precisely what CopilotTclaudeLayerExtraArgRefusal below refuses. No
// environment variable turns the sandbox on or off (`copilot help environment`,
// `copilot help sandbox`, pinned 1.0.77).
//
// So tclaude cannot force Copilot's inner wall off for one launch without also
// handing the pane a lever to raise it again. What it can
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
// TclaudeLayerHarnessBuiltinMode.
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

var copilotHarnessBuiltinModeHelp = map[string]string{
	CopilotSandboxInherit: "Use your Copilot `sandbox` posture as-is. Copilot's own command sandbox is experimental and off by default, and tclaude makes no containment claim for this mode. Its only per-launch flags require `--experimental`, which also lets the pane change the posture mid-session, so tclaude does not enable or disable it per session.",
	CopilotSandboxOff:     "Copilot's own (experimental, MXC) command sandbox is asserted NOT engaged, so tclaude's built-in OS sandbox is the single enforcement boundary. The launch is REFUSED — not silently downgraded — when Copilot's settings.json or its legacy config.json (which wins) enables that sandbox, is unreadable or ambiguous, or leaves experimental features on (which registers the in-pane `/sandbox enable` command).",
}

func (copilotSandbox) ModeHelp(mode string) string {
	return copilotHarnessBuiltinModeHelp[strings.TrimSpace(mode)]
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
// The migration is SHALLOW, at the top level. A `sandbox` object in config.json
// REPLACES settings.json's whole `sandbox` object rather than merging into it,
// while unrelated top-level keys survive. Measured:
//
//	settings.json {"sandbox":{"enabled":true},"theme":"dark"}
//	config.json   {"sandbox":{"addCurrentWorkingDirectory":true}}
//	  -> merged   {"sandbox":{"addCurrentWorkingDirectory":true},"theme":"dark"}
//
// The `enabled: true` is gone, so that launch has ONE boundary. Merging the two
// files per sub-key instead would carry the canonical file's `true` forward and
// refuse a launch that is in fact exactly what the assert-off contract wants.
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
	// COPILOT_HOME supersedes the home directory validated above, and it is not
	// validated by copilotStateDir. A RELATIVE value would make the settings
	// paths relative too, so os.ReadFile would resolve them against tclaude's
	// cwd — almost certainly missing, which this reader correctly reads as
	// absence, which allows the launch. Copilot meanwhile resolves the same
	// value against its OWN cwd and may be reading a file that raises the wall.
	// That is the one shape here where a read failure means "asked the wrong
	// question" rather than "the file is not there", so it refuses.
	if !filepath.IsAbs(stateDir) {
		return CopilotInnerSandboxState{}, copilotInnerSandboxError(fmt.Sprintf(
			"%s is relative (%q), so tclaude and Copilot would resolve it against different "+
				"working directories and inspect different settings files; set an absolute "+
				"path to launch under tclaude's boundary", CopilotHomeEnvVar, stateDir))
	}
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
		if file.sandboxSet {
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

// copilotSandboxFile is one file's contribution to the effective posture.
//
// The *Set flags are what make precedence expressible: "this file did not
// mention the key" and "this file said false" are different inputs to a merge,
// and collapsing them would let the weaker file's explicit false be silently
// re-asserted over the stronger file's true.
//
// sandboxSet — not an `enabledSet` — because the migration is SHALLOW at the
// TOP level: a `sandbox` object in the legacy file replaces the canonical
// file's whole `sandbox` object rather than merging into it. Measured against
// 1.0.77, `settings.json {"sandbox":{"enabled":true}}` plus
// `config.json {"sandbox":{"addCurrentWorkingDirectory":true}}` leaves a
// merged `sandbox` block with NO `enabled` key — the wall comes down. Keying
// the merge on `enabled` alone would carry the canonical file's `true` forward
// into a launch that does not have it, refusing a launch that is in fact
// single-boundary. Unrelated top-level keys are untouched by the replacement.
type copilotSandboxFile struct {
	// sandboxSet reports a top-level `sandbox` key, which is what REPLACES;
	// enabled is that block's `sandbox.enabled`, false when the block omits it.
	sandboxSet, enabled           bool
	experimental, experimentalSet bool
}

// readCopilotSandboxFile parses one of Copilot's two settings files. A missing
// file is (zero, false, nil) — absence is not ambiguity, and the CLI documents
// the sandbox as off by default. Everything else that stops tclaude from
// reading a determinate answer is a refusal.
//
// This is the sibling of ResolveCopilotMergedSettings, not a competitor to it:
// both implement the same two-file precedence, and this one exists because the
// sandbox gate must report WHICH file enabled the inner sandbox (and must treat
// a `sandbox` block that omits `enabled` as a real replacement) rather than
// only take a winning value. Keep the two in step — the precedence semantics
// are one contract with two implementations.
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
	// The whole block counts as set, even when it omits `enabled` — that is the
	// case a per-key merge gets wrong, because the block still REPLACES the
	// other file's, taking its `enabled` with it.
	out.sandboxSet = true
	if raw, found := sandbox["enabled"]; found {
		if err := json.Unmarshal(raw, &out.enabled); err != nil {
			return out, false, copilotInnerSandboxError(fmt.Sprintf(
				"Copilot settings %s has a non-boolean `sandbox.enabled` value; tclaude cannot "+
					"determine whether Copilot's own command sandbox is engaged", path))
		}
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
//
// This is deliberately NARROWER than whatever JSONC-ish parser the CLI itself
// uses: a hand-edited file with a trailing comma or a /* */ block will parse
// for Copilot and refuse here. That direction is chosen on purpose — the error
// is an over-refusal an operator can see and fix, whereas a lenient parser that
// guessed wrong about a file it half-understood would be asserting a single
// boundary on a reading nobody verified.
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

// CopilotSettingsSource is one merged top-level settings key: its raw value and
// the file that supplied it.
type CopilotSettingsSource struct {
	// Raw is the key's value as written in the winning file.
	Raw json.RawMessage
	// Path is the file the value came from, so a refusal can name the file an
	// operator has to edit rather than the one being overwritten.
	Path string
}

// ResolveCopilotMergedSettings returns Copilot's effective top-level settings —
// settings.json overlaid by the legacy config.json, whole key by whole key.
//
// This is where the two-file precedence contract is written down, and the
// model-transport route gate reads through it. Any NEW code deciding something
// from a Copilot settings key belongs here too rather than opening
// settings.json directly, because every property that makes the naive read
// wrong lives in this function: the second file, its precedence, the shallow
// whole-key replacement, and the comment-led managed stub. A reader that got
// any of those wrong would not fail loudly — it would quietly answer a question
// about a file the launch is not going to use. That is exactly how the
// model-transport route gate came to admit a legacy `copilotUrl` override that
// then broke against the network wall instead of being refused with a reason.
//
// It is NOT yet the only reader: ResolveCopilotInnerSandbox still walks the
// same two files through readCopilotSandboxFile, because it needs to know which
// file set a key rather than only that key's winning value. Those semantics
// match this function's today and are pinned by the same precedence tests, but
// they are two implementations of one contract — change one and check the
// other.
//
// The returned map is keyed by the top-level settings key. A missing file
// contributes nothing; an unreadable or unparsable one is an error, since a
// launch decided from a file tclaude could not read is a launch decided on a
// guess.
func ResolveCopilotMergedSettings(
	getenv func(string) string,
	home string,
) (map[string]CopilotSettingsSource, error) {
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	if err != nil {
		return nil, err
	}
	merged := map[string]CopilotSettingsSource{}
	// Weakest first: the legacy file overwrites, whole key by whole key.
	for _, path := range []string{
		filepath.Join(stateDir, CopilotSettingsFileName),
		filepath.Join(stateDir, CopilotConfigFileName),
	} {
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, copilotInnerSandboxError(fmt.Sprintf(
				"cannot read Copilot settings %s: %v; tclaude cannot tell what this launch is "+
					"configured to do, so it is refused rather than started on an unverified "+
					"reading", path, readErr))
		}
		var file map[string]json.RawMessage
		if err := json.Unmarshal(stripCopilotLineComments(data), &file); err != nil {
			return nil, copilotInnerSandboxError(fmt.Sprintf(
				"cannot parse Copilot settings %s as a JSON object: %v; fix the file or use "+
					"another posture", path, err))
		}
		for key, raw := range file {
			merged[key] = CopilotSettingsSource{Raw: raw, Path: path}
		}
	}
	return merged, nil
}

// CopilotStateDirForLaunch resolves the COPILOT_HOME a launch will use, with
// the absoluteness check that copilotStateDir deliberately does not perform.
//
// Exported because more than one gate needs the same directory AND the same
// refusal: a relative COPILOT_HOME makes tclaude and Copilot resolve the same
// value against different working directories, so tclaude would be inspecting
// files the launch never opens and reading their absence as consent.
func CopilotStateDirForLaunch(getenv func(string) string, home string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "" || home == "." || !filepath.IsAbs(home) {
		return "", copilotInnerSandboxError(fmt.Sprintf(
			"Copilot's configuration needs an absolute home directory to resolve %s from, got %q",
			CopilotHomeEnvVar, home))
	}
	stateDir, _ := copilotStateDir(getenv, home)
	if !filepath.IsAbs(stateDir) {
		return "", copilotInnerSandboxError(fmt.Sprintf(
			"%s is relative (%q), so tclaude and Copilot would resolve it against different "+
				"working directories and inspect different settings files; set an absolute path",
			CopilotHomeEnvVar, stateDir))
	}
	return stateDir, nil
}

// CopilotRouteMovingSettingsKeys are the settings keys that move Copilot's
// model endpoint away from the first-party route.
//
// The two are NOT equally established, and saying so matters for how a reader
// weighs the refusal:
//
//   - `proxyUrl` is the live, material one. Measured on 1.0.77, it really does
//     send the launch's model traffic somewhere the authored allow list never
//     approved.
//   - `copilotUrl` appears inert and is undocumented at this version. It is
//     refused CONSERVATIVELY — it is named in the shipped runtime and would be
//     the obvious lever if it were wired up, and a refusal costs an operator
//     one setting while a miss costs the network wall its meaning.
func CopilotRouteMovingSettingsKeys() []string {
	return []string{"copilotUrl", "proxyUrl"}
}

// CopilotRouteMovingSettingsKey reports the merged settings key that moves the
// endpoint, with the file that set it, or "" when the launch keeps the default
// route.
//
// Presence alone is not enough: an explicit null or empty string leaves the
// default route in place, and refusing over one would send an operator hunting
// for a setting that changes nothing. A non-string value is ambiguous rather
// than absent and is refused by NAME.
func CopilotRouteMovingSettingsKey(
	merged map[string]CopilotSettingsSource,
) (key, path string) {
	for _, candidate := range CopilotRouteMovingSettingsKeys() {
		source, found := merged[candidate]
		if !found {
			continue
		}
		var value string
		if err := json.Unmarshal(source.Raw, &value); err != nil {
			return candidate, source.Path
		}
		if strings.TrimSpace(value) != "" {
			return candidate, source.Path
		}
	}
	return "", ""
}
