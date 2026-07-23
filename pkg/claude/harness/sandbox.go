package harness

import (
	"fmt"
	"strings"
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
//     request resolves to "" (omit). OpenCode currently takes this branch.
//   - Codex: an empty request resolves to the secure DefaultMode (the managed
//     profile); any explicit mode is validated.
//   - Claude Code: an empty request resolves to its DefaultMode (inherit), which
//     ValidateMode normalizes back to "" — so an un-chosen Claude spawn imposes
//     no `--settings` override and keeps the operator's settings.json posture.
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
// harness with no sandbox catalog is still an error (no shipped harness hits
// this — Claude Code now validates inherit/on/off and normalizes inherit to "").
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
