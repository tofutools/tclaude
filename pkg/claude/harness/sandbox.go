package harness

import (
	"fmt"
	"strings"
	"unicode"
)

// SandboxCatalog is the optional capability for a harness that takes a
// launch-time sandbox-mode flag (Codex's `--sandbox`). A harness whose
// sandbox is configured out of band — Claude Code, whose sandbox lives in
// settings.json, not a launch flag — leaves Harness.Sandbox nil, so the
// spawn path performs no sandbox handling for it (SupportsSandbox() is
// false; passing a mode is an error the caller surfaces).
//
// The contract is deliberately small: name the secure default mode, and
// validate/normalize a requested one. The cwd-safety check (a sandboxed
// agent's cwd must not expose $HOME) is a separate, boundary-level concern
// because it needs the resolved cwd — see CodexSandboxCwdConflict.
type SandboxCatalog interface {
	// DefaultMode is the mode a tclaude-spawned agent runs under when the
	// caller didn't choose one. It must be a *sandboxed* mode (never a
	// full-access one): unspecified means "secure by default".
	DefaultMode() string
	// ValidateMode normalizes and validates a requested mode. The empty
	// string is returned unchanged (callers substitute DefaultMode where a
	// default is wanted, via ResolveSandboxMode); any other value is either
	// a recognized mode (returned trimmed) or an error naming the valid set.
	ValidateMode(mode string) (string, error)
	// Modes lists the selectable sandbox modes for spawn UIs, in ascending
	// order of permissiveness (read-only … danger-full-access). The
	// dashboard spawn dialog drives its sandbox <select> off this so a
	// harness owns its own mode set — the SandboxCatalog parallel to
	// ModelCatalog.Models / EffortLevels.
	Modes() []string
	// ModeHelp returns a one-line human description of a mode for spawn UIs
	// — notably its agentd-socket reachability, the property that surprises
	// operators (a raw `--sandbox` mode blocks the socket, so the agent
	// can't run `tclaude agent …`) — or "" for an unrecognized mode. The
	// copy lives here, beside the modes it describes, so the dashboard
	// renders it verbatim and it can't drift from what Modes() lists.
	ModeHelp(mode string) string
}

// ResolveSandboxMode is the entry point the *daemon* spawn boundaries
// (agentd spawn/resume/clone/reincarnate, `tclaude agent spawn`) use to turn
// a requested sandbox mode into the value to thread into
// SpawnSpec.SandboxMode. It applies the secure default, because an
// agentd-spawned agent is the untrusted party that must be sandboxed:
//
//   - Harness with no sandbox catalog: an explicit mode is an error; an empty
//     request resolves to "" (omit).
//   - Codex: an empty request resolves to the secure DefaultMode (the managed
//     profile); any explicit mode is validated.
//   - Claude Code: an empty request resolves to its DefaultMode (inherit), which
//     ValidateMode normalizes back to "" — so an un-chosen Claude spawn imposes
//     no `--settings` override and keeps the operator's settings.json posture.
//   - OpenCode: an empty daemon request resolves to its DefaultMode
//     (access-control) — a soft, tool-level access policy, not an OS sandbox.
//     SpawnSandboxWarnings surfaces that distinction so the mode is not mistaken
//     for real containment.
//
// requested is trimmed first, so surrounding whitespace never leaks into
// the flag.
func ResolveSandboxMode(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" && h.SupportsSandbox() {
		requested = h.Sandbox.DefaultMode()
	}
	return ValidateSandboxMode(h, requested)
}

// ValidateSandboxMode validates a requested mode WITHOUT applying the
// harness default — empty stays empty (omit the flag). It is the direct
// `tclaude session new` path's entry point: the human running session new is
// the trust root, so tclaude must not silently override their own config
// (Codex's config.toml sandbox_mode, Claude Code's settings.json) — it emits a
// sandbox value only when they pass one explicitly (the daemon spawn path uses
// ResolveSandboxMode for the secure default instead). An explicit mode for a
// harness with no sandbox catalog is still an error.
func ValidateSandboxMode(h *Harness, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	if !h.SupportsSandbox() {
		return "", fmt.Errorf("harness %q has no launch-time sandbox mode "+
			"(its sandbox is configured out of band, not via --sandbox)", h.Name)
	}
	return h.Sandbox.ValidateMode(requested)
}

// SandboxOffMode returns the harness-native mode that deliberately disables
// confinement for a temporary operator unlock. Keeping this mapping beside the
// catalogs prevents dashboard lifecycle code from guessing which spelling
// means "off" for each harness.
func SandboxOffMode(h *Harness) (string, error) {
	if h == nil {
		return "", fmt.Errorf("nil harness")
	}
	var mode string
	switch normalizeLineageHarness(h.Name) {
	case DefaultName:
		mode = ClaudeSandboxOff
	case CodexName:
		mode = SandboxDangerFull
	case OpenCodeName:
		mode = OpenCodeSandboxOff
	default:
		return "", fmt.Errorf("harness %q has no sandbox-off mode", h.Name)
	}
	return ValidateSandboxMode(h, mode)
}

// SpawnSandboxWarnings is the single harness-neutral entry point every spawn
// surface uses to describe a launch posture whose sandboxing is weaker than it
// looks — the HTTP effective-sandbox probe behind the spawn dialog and the
// profile/role editors, template/wave deploys, `tclaude session new`, and the
// daemon spawn response. Routing all of them through one function keeps every
// surface saying the same sentence for the same inputs.
//
// It dispatches by harness because the "your sandbox may not be doing what you
// think" failure mode is harness-specific:
//
//   - Claude Code: an unattended command-running permission mode paired with an
//     OS sandbox tclaude cannot prove is active (TCL-586) — see
//     UnsandboxedAutonomyWarnings, which also reads settings.json under cwd.
//   - OpenCode: `access-control` only does soft lexical path matching, while
//     `tclaude-layer` confines the tool executor but leaves the attach/control
//     boundary outside — see openCodeSandboxWarnings.
//
// Codex resolves autonomy and sandbox together against its managed profile, so
// it has no such gap and returns nil. approvalPolicy and sandboxMode must be
// the FINAL resolved values (after profile overlay and ResolveApprovalPolicy /
// ResolveSandboxMode), so a blank select is judged for the posture it resolves
// to, not for "nothing chosen".
func SpawnSandboxWarnings(h *Harness, approvalPolicy, sandboxMode, cwd string) []string {
	if h == nil {
		return nil
	}
	switch normalizeLineageHarness(h.Name) {
	case OpenCodeName:
		return openCodeSandboxWarnings(sandboxMode)
	default:
		return UnsandboxedAutonomyWarnings(h, approvalPolicy, sandboxMode, cwd)
	}
}

// LaunchOSSandbox is the OS-sandbox verdict a launch boundary RECORDS on its
// session row (sessions.os_sandbox_state / os_sandbox_source), so a later read
// surface — the dashboard's per-agent badge — can say whether the agent is
// actually confined instead of inferring it from the requested mode.
//
// State is "on", "off", or "unconfigured" (nothing tclaude can read enables it);
// Source names whatever decided that. Both are "" when tclaude recorded no
// verdict for this launch — see ResolveLaunchOSSandbox.
type LaunchOSSandbox struct {
	State  string
	Source string
	// Unverified marks a verdict tclaude could not fully establish. For a
	// harness-owned sandbox this means an outranking settings file could not be
	// read; experimental outer layers may also use it to record a known partial
	// enforcement boundary in Source.
	//
	// It is recorded because the badge is a durable claim about containment, and
	// the one thing worse than no badge is a padlock on an agent nothing confines.
	// ResolveClaudeSandboxEnabled walks tiers most-authoritative-first and stops
	// at the first that decides, so ANY diagnostic it collected necessarily came
	// from a HIGHER-precedence tier than the one that answered — which is exactly
	// the set of files that could overturn the verdict.
	//
	// The spawn-time warning already surfaces the unread file, but only on the
	// stderr of the launch and only when the approval policy runs commands
	// unattended. The badge is read later, by someone deciding whether to trust
	// this agent, so the doubt has to travel with it.
	Unverified bool
}

// ResolveLaunchOSSandbox answers "will the OS sandbox actually be active for
// this launch", for a launch boundary to persist beside the requested sandbox
// mode.
//
// It exists because the requested mode does not always answer that question.
// Claude Code's default and recommended mode is `inherit`, which deliberately
// emits no `--settings` override so the operator's own settings.json posture
// survives — so the recorded mode says "whatever your settings say", and a
// dashboard badge driven off the mode alone stays blank whether the agent is
// confined by a project/user/global `sandbox.enabled` or by nothing at all
// (TCL-729). Resolving it once here, at launch, is also the only way to get the
// answer RIGHT: it is a property of the settings files as they were when the
// harness read them, so a read surface that re-resolved later would report the
// operator's current config rather than what the running agent launched under.
//
// A harness whose recorded mode already states its posture records nothing (the
// zero value) and its badge behaves exactly as before:
//
//   - Codex spawns under an explicit `--sandbox` mode or the managed permission
//     profile, so the mode IS the verdict. (For a DAEMON spawn — ResolveSandboxMode
//     applies that default. A bare `tclaude session new --harness codex` records no
//     mode and its real posture lives in ~/.codex/config.toml, which tclaude does
//     not read; that gap is the Codex analogue of the one this fixes for Claude,
//     and is out of scope here.)
//   - OpenCode's `access-control` is a soft tool-level policy, not an OS
//     sandbox; claiming a verdict for it would dress it up as containment (the
//     distinction openCodeSandboxWarnings exists to make).
//
// sandboxMode must be the FINAL resolved mode and cwd the launch directory —
// the same inputs SpawnSandboxWarnings takes, so the recorded verdict and the
// warning the operator saw at spawn can never disagree.
//
// chosenBy names the resolution tier that supplied sandboxMode (an explicit
// flag, a spawn profile, a replay of the recorded posture) and is folded into
// the recorded Source when the LAUNCH is what decided the state. It answers a
// question the verdict alone cannot: an operator who never typed `--sandbox on`
// still gets one when a group or global default spawn profile carries it, and
// "forced ON for this launch" attributed that to them. Empty (a direct
// `session new`, or a caller with nothing to say) keeps the previous wording.
func ResolveLaunchOSSandbox(h *Harness, sandboxMode, chosenBy, cwd string) LaunchOSSandbox {
	if h == nil || normalizeLineageHarness(h.Name) != DefaultName {
		return LaunchOSSandbox{}
	}
	resolution := ResolveClaudeSandboxEnabled(sandboxMode, cwd)
	return LaunchOSSandbox{
		State:  resolution.State.String(),
		Source: attributeLaunchSandboxSource(resolution.Source, chosenBy),
		// Diagnostics are only ever collected from tiers walked BEFORE the one
		// that decided, so a non-empty list means something that outranks this
		// verdict was unreadable — including for an explicit on/off, where the
		// managed tier is consulted ahead of the launch flag precisely because it
		// outranks it.
		Unverified: len(resolution.Diagnostics) > 0,
	}
}

// SandboxChosenExplicitly is the attribution for a mode the caller typed
// themselves — `tclaude session new --sandbox on`, or a spawn request that
// carried an explicit field. It matches the daemon's own provenance vocabulary
// (agent.ProvExplicit) without importing it: the agent package depends on
// session, which depends on this one.
const SandboxChosenExplicitly = "explicit"

// launchDecidedSourcePrefix marks the resolutions whose deciding tier was the
// launch itself ("this launch (sandbox `on`)"). Only those can be attributed:
// where a settings file decided, WHO chose the launch mode did not affect the
// outcome, and naming a spawn profile there would credit it with a verdict it
// had no part in.
const launchDecidedSourcePrefix = "this launch ("

// launchDecidedActor is the part of that prefix an attribution replaces.
const launchDecidedActor = "this launch"

// maxSandboxChosenByLen bounds the attribution folded into the recorded source.
// The tier label embeds an operator-authored spawn-profile name, and this value
// is persisted, logged, and rendered; a name is a label, not a payload.
const maxSandboxChosenByLen = 120

// attributeLaunchSandboxSource names the ACTOR in a launch-decided source,
// leaving every other source untouched:
//
//	this launch (sandbox `on`)  +  global default profile "agents"
//	→ global default profile "agents" (sandbox `on`)
//
// It replaces "this launch" rather than appending to it, because the reader is
// asking who imposed the containment and "this launch" is the answer only when
// the caller typed the flag. An explicit choice is left as "this launch": it IS
// this launch, and naming the tier would add a word without adding a fact.
func attributeLaunchSandboxSource(source, chosenBy string) string {
	chosenBy = SanitizeSandboxChosenBy(chosenBy)
	if chosenBy == "" || chosenBy == SandboxChosenExplicitly {
		return source
	}
	if !strings.HasPrefix(source, launchDecidedSourcePrefix) {
		return source
	}
	return chosenBy + strings.TrimPrefix(source, launchDecidedActor)
}

// SanitizeSandboxChosenBy makes an attribution safe to persist and display. The
// spawn-profile name inside it is operator free text that reaches an argv, a DB
// column, a log line, and the dashboard, so control characters (which would
// forge line structure in a log) are dropped and the whole label is bounded.
// Truncation is marked, so a clipped label never reads as a complete name.
//
// Exported because the bound has to be applied where the value is RECORDED, not
// only where it is rendered: `session new` writes it to sessions, the durable
// relaunch profile projects it, and every later relaunch replays it into an
// argv. Scrubbing only the derived source would leave the unbounded original in
// all three.
func SanitizeSandboxChosenBy(chosenBy string) string {
	chosenBy = strings.TrimSpace(chosenBy)
	if chosenBy == "" {
		return ""
	}
	cleaned := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, chosenBy)
	cleaned = strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
	if len([]rune(cleaned)) > maxSandboxChosenByLen {
		cleaned = string([]rune(cleaned)[:maxSandboxChosenByLen]) + "…"
	}
	return cleaned
}
