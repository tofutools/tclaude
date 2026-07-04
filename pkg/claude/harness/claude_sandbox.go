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
//     reachable and ~/.tclaude is hidden.
//   - off     : force the OS sandbox OFF for this session via `--settings`,
//     even if settings.json enables it.
const (
	ClaudeSandboxInherit = "inherit"
	ClaudeSandboxOn      = "on"
	ClaudeSandboxOff     = "off"
)

// tclaudeAgentdSocketTilde is the agentd Unix socket as a ~-relative path, the
// form Claude Code's settings.json sandbox rules expect (it expands ~ itself).
// Mirrors agent.SocketPath()'s ~/.tclaude/agentd.sock; kept as a literal here
// rather than importing the agent package (which would pull a heavier dep into
// the harness seam for one constant).
const tclaudeAgentdSocketTilde = "~/.tclaude/agentd.sock"

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
	ClaudeSandboxInherit: "Recommended. No per-session override — the agent uses your Claude Code settings.json sandbox config (global / project) as-is, including any `tclaude setup --install-sandbox-hardening` you've applied.",
	ClaudeSandboxOn:      "Force Claude Code's OS sandbox ON for this session, even if settings.json leaves it off. Bash is confined (working dir writable, $HOME read-only); the agentd socket stays reachable and ~/.tclaude is hidden, so the agent can still run `tclaude agent` but can't read other agents' state.",
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
// It enables the sandbox AND preserves the two properties a daemon-spawned
// agent needs: the agentd Unix socket stays reachable (network allowlist +
// filesystem read allowance) so the agent can still run `tclaude agent`, and
// ~/.tclaude / ~/.claude/sessions are denied (read + write) so a sandboxed
// agent can neither tamper with nor snoop on the shared daemon state. The
// block is cross-platform: macOS honors per-path `allowUnixSockets`, Linux/WSL2
// the broader `allowAllUnixSockets` — listing both keeps one block valid on
// either (the inert key is harmless).
//
// Arrays are []any (not []string) so the setup merge engine compares and
// appends them uniformly against values decoded from a user's settings file
// (where every JSON array decodes to []any); json.Marshal handles []any the
// same as []string for the spawner's `--settings` payload. A fresh map each
// call so the setup merge can mutate it in place without aliasing.
func ClaudeSandboxOnBlock() map[string]any {
	return map[string]any{
		"enabled": true,
		"network": map[string]any{
			"allowUnixSockets":    []any{tclaudeAgentdSocketTilde},
			"allowAllUnixSockets": true,
		},
		"filesystem": map[string]any{
			"denyWrite": []any{"~/.tclaude", "~/.claude/sessions"},
			"denyRead":  []any{"~/.tclaude", "~/.claude/sessions"},
			"allowRead": []any{tclaudeAgentdSocketTilde},
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
