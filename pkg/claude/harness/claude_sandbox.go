package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	ClaudeSandboxInherit: "Use your Claude Code settings.json sandbox config as-is, including any tclaude hardening already installed.",
	ClaudeSandboxOn:      "Force Claude Code's OS sandbox ON for this session, even if settings.json leaves it off. Bash is confined (working dir writable, $HOME read-only); the agentd socket stays reachable and ~/.tclaude/data (all daemon state) is hidden, so the agent can still run `tclaude agent` but can't read other agents' state.",
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
// agent needs: the agent-reachable agentd Unix socket (~/.tclaude/api/…) stays
// reachable (network allowlist + filesystem read allowance) so the agent can
// still run `tclaude agent`, and
// ~/.tclaude/data and ~/.claude/sessions are denied (read + write) so a
// sandboxed agent can neither tamper with nor snoop on shared daemon/Claude
// session state. ~/.codex remains readable because it also contains the Codex
// runtime itself; denying that whole root can strand the harness.
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
			"allowUnixSockets":    tclaudeAgentdSocketTildes(),
			"allowAllUnixSockets": true,
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
func claudeSandboxBlock(mode string) map[string]any {
	return claudeSandboxBlockWithBreakGlass(mode, nil, nil)
}

// claudeSandboxBlockWithBreakGlass builds the `on`/`off` block, omitting any
// tclaude protected deny that an acknowledged break-glass rule reaches.
//
// The suppression is mandatory, not cosmetic. Claude applies deny directories
// shallowest-first and re-masks after re-binding allows, so a denyRead sitting
// at the SAME path as the break-glass grant makes the outcome order-sensitive.
// Dropping exactly the covered deny leaves an unambiguous policy: the operator
// acknowledged this path, so tclaude stops denying it — and keeps denying the
// protected paths they did NOT acknowledge.
func claudeSandboxBlockWithBreakGlass(mode string, breakGlassRead, breakGlassWrite []string) map[string]any {
	switch strings.TrimSpace(mode) {
	case ClaudeSandboxOn:
		block := ClaudeSandboxOnBlock()
		if len(breakGlassRead) == 0 && len(breakGlassWrite) == 0 {
			return block
		}
		filesystem, _ := block["filesystem"].(map[string]any)
		if filesystem == nil {
			return block
		}
		// Read and write are suppressed INDEPENDENTLY. Dropping denyWrite for a
		// read-only acknowledgement would be a silent privilege escalation: the
		// deny is what stops an unrelated allowWrite root (a workspace or Git
		// grant that happens to contain the protected path) from making it
		// writable, since Claude's write policy is allowlist-shaped and allows
		// win over denies. A read acknowledgement must never enable a write.
		for key, grants := range map[string][]string{
			"denyRead":  append(append([]string{}, breakGlassRead...), breakGlassWrite...),
			"denyWrite": breakGlassWrite,
		} {
			if len(grants) == 0 {
				continue
			}
			existing, _ := filesystem[key].([]any)
			kept := make([]any, 0, len(existing))
			for _, value := range existing {
				path, ok := value.(string)
				if ok && breakGlassCoversTilde(grants, path) {
					continue
				}
				kept = append(kept, value)
			}
			filesystem[key] = kept
		}
		return block
	case ClaudeSandboxOff:
		return ClaudeSandboxOffBlock()
	default:
		return nil
	}
}

// breakGlassCoversTilde resolves tclaude's own "~/…"-spelled protected deny
// entries against the real home directory before comparing them with the
// canonical, fully-resolved break-glass paths.
func breakGlassCoversTilde(breakGlass []string, denyPath string) bool {
	if strings.HasPrefix(denyPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		denyPath = filepath.Join(home, denyPath[len("~/"):])
	}
	if !filepath.IsAbs(denyPath) {
		return false
	}
	// A protected root can itself be reached through a symlinked home; compare
	// on the resolved form so an alias cannot defeat the suppression.
	if resolved, err := filepath.EvalSymlinks(denyPath); err == nil {
		denyPath = resolved
	}
	return breakGlassCoversPath(breakGlass, denyPath)
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
