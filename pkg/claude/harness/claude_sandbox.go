package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Claude Code launch-containment modes. Unlike Codex — whose sandbox is a
// fixed `--sandbox <mode>` enum — Claude Code's OS sandbox lives in
// settings.json under a `sandbox` key, with no dedicated launch flag. The
// per-session lever is `claude --settings '<json>'`, which merges a settings
// block over the user/project files (only managed/policy settings outrank it).
// So tclaude models a small tri-state and translates it to a `--settings`
// override in claudeSpawner.BuildCommand:
//
//   - inherit : add no override — the agent uses the human's settings.json
//     sandbox config (global / project) exactly as-is. This is the
//     default, so a tclaude-spawned Claude agent's containment is
//     whatever the operator already configured (incl. the global
//     `tclaude setup --install-sandbox-hardening`), never silently
//     changed. It NORMALIZES to "" (omit) — see ValidateMode.
//   - on      : force the OS sandbox ON for this session via `--settings`,
//     even if settings.json leaves it off. Reuses the hardening
//     block (ClaudeSandboxOnBlock) so the agentd socket stays
//     reachable and ~/.tclaude/data is hidden.
//   - off     : force the OS sandbox OFF for this session via `--settings`,
//     even if settings.json enables it.
const (
	ClaudeSandboxInherit = "inherit"
	ClaudeSandboxOn      = "on"
	ClaudeSandboxOff     = "off"
)

// tclaude sandbox path tokens as ~-relative strings, the form Claude Code's
// settings.json sandbox rules expect (it expands ~ itself).
//
// The canonical agentd socket lives under ~/.tclaude/api — an agent-reachable
// surface OUTSIDE the denied private-state subtree ~/.tclaude/data — so the
// socket stays reachable while all daemon state stays hidden under one deny
// rule. The two legacy sockets are kept allowlisted for the migration window;
// both sit outside ~/.tclaude/data, so the deny does not cover them.
const (
	tclaudeAgentdSocketTilde      = "~/.tclaude/api/agentd.sock"
	tclaudeLegacyHomeSocketTilde  = "~/.tclaude-agentd.sock"
	tclaudeLegacyRootSocketTilde  = "~/.tclaude/agentd.sock"
	tclaudePrivateStateDirTilde   = "~/.tclaude/data"
	tclaudeClaudeSessionsDirTilde = "~/.claude/sessions"
)

// tclaudeAgentdSocketTildes lists every agentd socket a sandboxed agent may need
// to reach: the canonical api/ socket plus the retained legacy endpoints.
func tclaudeAgentdSocketTildes() []any {
	return []any{tclaudeAgentdSocketTilde, tclaudeLegacyHomeSocketTilde, tclaudeLegacyRootSocketTilde}
}

// claudeSandbox is Claude Code's SandboxCatalog. The default is `inherit`: a
// tclaude-spawned Claude agent's containment is whatever the operator already
// configured in settings.json (JOH-decision: "inherit = no behavior change"),
// never silently overridden — unlike Codex, where no flag means no sandbox at
// all so the daemon must impose a secure default. `on` / `off` are the explicit
// per-session overrides.
type claudeSandbox struct{}

// DefaultMode is `inherit` — the dropdown's recommended option (the dashboard
// marks DefaultMode() "(recommended)"). `inherit` is a FIRST-CLASS value
// (ValidateMode returns it unchanged, NOT ""): it means "use the operator's own
// settings.json sandbox config AND don't let a profile/group default override
// that". It collapses to "no override" only at the final block emission (see
// claudeSandboxBlock), so a spawn that explicitly chose inherit is not silently
// re-filled by an overlay.
func (claudeSandbox) DefaultMode() string { return ClaudeSandboxInherit }

// Modes lists the selectable modes for spawn UIs: inherit (the default /
// recommended), then the two explicit overrides. A fresh slice each call so a
// caller can't mutate the set.
func (claudeSandbox) Modes() []string {
	return []string{ClaudeSandboxInherit, ClaudeSandboxOn, ClaudeSandboxOff}
}

// ValidateMode normalizes and validates a requested mode, preserving the
// tri-state the overlay sites depend on:
//
//   - ""      → "" (OMITTED — a higher level, e.g. a group default profile, may
//     fill it; if nothing does, the launch boundary applies the harness default).
//   - inherit → "inherit" (ACTIVELY chosen — carried through as a first-class
//     sentinel so an overlay treats it as "already set" and does NOT overwrite
//     it; the final block emission collapses it to "no override").
//   - on / off → themselves.
//   - anything else → an error naming the valid set.
//
// The old behaviour collapsed inherit to "" here, which made an explicit inherit
// indistinguishable from omitted so a profile/group default silently won;
// keeping inherit distinct is the fix. `inherit` still emits no `--settings`
// sandbox block and records no badge (see claudeSandboxBlock / sandboxBadge).
func (claudeSandbox) ValidateMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "":
		return "", nil
	case ClaudeSandboxInherit:
		return ClaudeSandboxInherit, nil
	case ClaudeSandboxOn:
		return ClaudeSandboxOn, nil
	case ClaudeSandboxOff:
		return ClaudeSandboxOff, nil
	default:
		return "", fmt.Errorf("invalid claude sandbox mode %q (want %s|%s|%s)",
			mode, ClaudeSandboxInherit, ClaudeSandboxOn, ClaudeSandboxOff)
	}
}

// claudeSandboxModeHelp is the one-line description the spawn UI shows for each
// mode. `on` calls out the agentd-socket reachability + ~/.tclaude hiding (the
// properties that keep a sandboxed agent able to coordinate yet unable to read
// peers' state). Keyed by mode value.
var claudeSandboxModeHelp = map[string]string{
	ClaudeSandboxInherit: "Use your Claude Code settings.json enabled/disabled posture as-is, including any tclaude hardening already installed. When enabled, tclaude's per-launch deny also blocks the tmux server socket hosting agent panes.",
	ClaudeSandboxOn:      "Force Claude Code's OS sandbox ON for this session, even if settings.json leaves it off. Bash is confined (working dir writable, $HOME read-only); the agentd socket stays reachable while ~/.tclaude/data and the tmux server socket hosting agent panes are denied.",
	ClaudeSandboxOff:     "⚠ Force the OS sandbox OFF for this session, even if settings.json enables it. The agent's Bash runs unconfined.",
}

// ModeHelp returns a one-line description of a mode for spawn UIs, or "" for an
// unrecognized mode. The `inherit` help is keyed under its mode token even
// though ValidateMode collapses it to "" — the dashboard renders help off the
// raw Modes() tokens, not the validated value.
func (claudeSandbox) ModeHelp(mode string) string {
	return claudeSandboxModeHelp[strings.TrimSpace(mode)]
}

// ClaudeSandboxOnBlock is the value of the settings.json `sandbox` key the
// `on` mode injects via `--settings` — and the single source of truth the
// global `tclaude setup --install-sandbox-hardening` reuses for its own
// `sandbox` block, so the per-session override and the global hardening can
// never drift (docs/sandbox-hardening.md is the human-facing source of truth).
//
// It enables the sandbox AND preserves the properties a daemon-spawned agent
// needs: the agent-reachable agentd Unix socket (~/.tclaude/api/…) stays
// reachable (network allowlist + filesystem read allowance) so the agent can
// still run `tclaude agent`; GitHub and its API stay reachable so the agent can
// push branches, open PRs, and inspect checks without leaving the sandbox; and
// ~/.tclaude/data and ~/.claude/sessions are denied (read + write) so a
// sandboxed agent can neither tamper with nor snoop on shared daemon/Claude
// session state. The model-controlled dangerouslyDisableSandbox escape hatch
// is disabled so those boundaries cannot be skipped. ~/.codex remains readable
// because it also contains the Codex runtime itself; denying that whole root
// can strand the harness.
// block is cross-platform: macOS honors per-path `allowUnixSockets`;
// Linux/WSL2 require the broader `allowAllUnixSockets`, which macOS also
// honors. Listing both keeps one block functional on either platform, at the
// cost of the documented all-sockets exposure on macOS too.
//
// Arrays are []any (not []string) so the setup merge engine compares and
// appends them uniformly against values decoded from a user's settings file
// (where every JSON array decodes to []any); json.Marshal handles []any the
// same as []string for the spawner's `--settings` payload. A fresh map each
// call so the setup merge can mutate it in place without aliasing.
func ClaudeSandboxOnBlock() map[string]any {
	return map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"allowUnsandboxedCommands": false,
		"network": map[string]any{
			"allowUnixSockets":    tclaudeAgentdSocketTildes(),
			"allowAllUnixSockets": true,
			"allowedDomains":      []any{"github.com", "api.github.com"},
		},
		"filesystem": map[string]any{
			"denyWrite": []any{tclaudePrivateStateDirTilde, tclaudeClaudeSessionsDirTilde},
			"denyRead":  []any{tclaudePrivateStateDirTilde, tclaudeClaudeSessionsDirTilde},
			"allowRead": tclaudeAgentdSocketTildes(),
		},
	}
}

// ClaudeSandboxOffBlock is the value of the settings.json `sandbox` key the
// `off` mode injects via `--settings`: just `enabled: false`, which (as a CLI
// `--settings` override) outranks a user/project `enabled: true` and disables
// the sandbox for this session. The filesystem/network sub-keys are moot when
// disabled, so they are omitted.
func ClaudeSandboxOffBlock() map[string]any {
	return map[string]any{"enabled": false}
}

// claudeSandboxBlock returns the value of the settings.json `sandbox` key for a
// validated Claude sandbox mode, or nil when no override should be emitted
// (inherit / unset / unrecognized). It is the shared block-builder the spawner's
// merged `--settings` payload (claudeSettingsJSON) and the single-key
// claudeSandboxSettingsJSON both draw from, so the two can never drift.
//
// The tclaude protected denies in the `on` block are UNCONDITIONAL. This used
// to take break-glass grants and suppress exactly the denies they reached;
// TCL-791 removed break-glass, so there is no longer any input that can drop
// one. That is the point: an operator who must work without the wall turns the
// sandbox off rather than carving a hole in it.
func claudeSandboxBlock(mode string) map[string]any {
	switch strings.TrimSpace(mode) {
	case ClaudeSandboxOn:
		return ClaudeSandboxOnBlock()
	case ClaudeSandboxOff:
		return ClaudeSandboxOffBlock()
	default:
		return nil
	}
}

// claudeSandboxSettingsJSON returns the compact `--settings` JSON payload for a
// validated Claude sandbox mode ALONE, or "" when no override should be emitted
// (inherit / unset / unrecognized — the spawner omits the flag). The result
// wraps the on/off block under the top-level `sandbox` key Claude Code expects.
// json.Marshal sorts map keys, so the output is deterministic (testable). The
// live spawn path uses the merged claudeSettingsJSON instead; this single-key
// form is retained for the sandbox acceptance tests.
func claudeSandboxSettingsJSON(mode string) string {
	block := claudeSandboxBlock(mode)
	if block == nil {
		return ""
	}
	b, err := json.Marshal(map[string]any{"sandbox": block})
	if err != nil {
		// Unreachable for these static maps; never emit half-built JSON.
		return ""
	}
	return string(b)
}
