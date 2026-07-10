package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// codexHookEvents is the set of Codex hook events tclaude registers its
// callback for. These are Codex's event names (PascalCase, per
// HookEventsToml in openai/codex codex-rs/config/src/hook_config.rs at
// rust-v0.139.0) — a SUBSET of Claude Code's: Codex has no Notification,
// SessionEnd, StopFailure or PostToolUseFailure. The status state machine
// already tolerates a harness firing fewer events (a session with no
// SessionEnd is reaped via Stop + process-exit, the same fallback the CC
// path uses for interrupted turns).
var codexHookEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"Stop",
	"PreToolUse",
	"PostToolUse",
	"PermissionRequest",
	"PreCompact",
	"PostCompact",
	"SubagentStart",
	"SubagentStop",
}

// codexHooksPath is ~/.codex/hooks.json — Codex's dedicated hooks file
// (config_folder.join("hooks.json") in codex's discovery.rs). tclaude owns
// this file rather than editing the user's config.toml [hooks] table:
// Codex loads both and warns when they conflict, so a separate file keeps
// tclaude's hooks cleanly separable.
func codexHooksPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

// codexHookCommand mirrors Codex's HookHandlerConfig::Command variant
// (serde tag = "type"). tclaude only ever writes the command form; the
// optional commandWindows/timeout/async/statusMessage fields are omitted.
type codexHookCommand struct {
	Type    string `json:"type"` // always "command"
	Command string `json:"command"`
}

// codexMatcherGroup mirrors Codex's MatcherGroup. tclaude writes a single
// matcher-less (catch-all) group per event.
type codexMatcherGroup struct {
	Matcher string             `json:"matcher,omitempty"`
	Hooks   []codexHookCommand `json:"hooks"`
}

// codexHookInstaller installs the tclaude callback into ~/.codex/hooks.json.
// It manages tclaude's matcher groups surgically — preserving any other
// top-level keys and any non-tclaude matcher groups the user has — the
// same belt-and-suspenders approach session.InstallHooks uses for CC's
// settings.json.
type codexHookInstaller struct{}

func (codexHookInstaller) ConfigTarget() string { return codexHooksPath() }

// TrustNote explains the intentionally declaration-only auto-install path.
// Setup grants execution trust only when Codex is explicitly selected.
func (codexHookInstaller) TrustNote() string {
	return "Codex hooks are installed but not trusted. Run 'tclaude setup --harness codex' to trust only tclaude's installed hooks; unrelated hooks remain on Codex's normal review path."
}

// codexHookCommandStr is the callback command tclaude installs — the same
// `tclaude session hook-callback` every harness invokes (the callback
// reads a snake_case JSON payload from stdin; Codex's payload matches
// Claude Code's field-for-field).
var codexHookCommandString = func() string {
	return clcommon.DetectAbsoluteCmd("session", "hook-callback")
}

func codexHookCommandStr() string { return codexHookCommandString() }

// isOurCodexHook reports whether a hook command belongs to tclaude — any
// path whose basename is "tclaude" (mirrors session.isOurHook for CC). The
// basename match is deliberate: it lets a stale absolute-path tclaude hook
// be recognised and repaired. The trade-off is that ANY binary named
// "tclaude" is treated as ours; a user hook pointing at an unrelated tool
// that happens to share the name would be replaced on install (vanishingly
// unlikely, and the same assumption CC's installer makes).
func isOurCodexHook(command string) bool {
	first := firstShellCommandWord(command)
	if first == "" {
		return false
	}
	return filepath.Base(first) == "tclaude"
}

// firstShellCommandWord decodes the quoting forms emitted by ShellQuoteArg so
// an absolute tclaude path containing spaces/apostrophes is still recognized
// and repaired on upgrade. It intentionally parses only the first shell word.
func firstShellCommandWord(command string) string {
	command = strings.TrimSpace(command)
	var out strings.Builder
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote == 0 {
			switch c {
			case ' ', '\t', '\r', '\n':
				return out.String()
			case '\'', '"':
				quote = c
			case '\\':
				if i+1 < len(command) {
					i++
					out.WriteByte(command[i])
				}
			default:
				out.WriteByte(c)
			}
			continue
		}
		if c == quote {
			quote = 0
			continue
		}
		if quote == '"' && c == '\\' && i+1 < len(command) {
			i++
			out.WriteByte(command[i])
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}

// Check reports whether the tclaude callback is installed for every
// required Codex event with the current binary. missing lists events that
// lack it; needsRepair is true when a stale (wrong-binary) or duplicate
// tclaude hook is present.
func (codexHookInstaller) Check() (installed bool, missing []string, needsRepair bool) {
	path := codexHooksPath()
	if path == "" {
		return false, []string{"all"}, false
	}
	hooks, _, err := readCodexHooks(path)
	if err != nil {
		// Unreadable/missing file → everything is missing, nothing to repair.
		return false, []string{"all (" + err.Error() + ")"}, false
	}

	want := codexHookCommandStr()
	for _, groupsRaw := range hooks {
		if codexHooksNeedCleanup(groupsRaw, want) {
			needsRepair = true
			break
		}
	}
	for _, event := range codexHookEvents {
		if !codexHooksContain(hooks[event], want) {
			missing = append(missing, event)
		}
	}
	return len(missing) == 0, missing, needsRepair
}

type codexHookInstallPlan struct {
	path  string
	out   []byte
	hooks map[string]json.RawMessage
	want  string
}

// Install installs or repairs the tclaude callback for every required
// Codex event, preserving any other top-level keys and non-tclaude matcher
// groups. Idempotent.
func (codexHookInstaller) Install() error {
	plan, err := planCodexHookInstall()
	if err != nil {
		return err
	}
	if err := atomicWritePreservingMode(plan.path, plan.out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plan.path, err)
	}
	return nil
}

func planCodexHookInstall() (codexHookInstallPlan, error) {
	path := codexHooksPath()
	if path == "" {
		return codexHookInstallPlan{}, fmt.Errorf("cannot determine codex hooks path")
	}

	hooks, top, err := readCodexHooks(path)
	if err != nil && !os.IsNotExist(err) {
		return codexHookInstallPlan{}, err
	}
	if hooks == nil {
		hooks = map[string]json.RawMessage{}
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}

	want := codexHookCommandStr()

	// First pass: strip every tclaude hook from every event (prevents
	// duplicates / clears stale binaries).
	for event, groupsRaw := range hooks {
		cleaned, removed, err := removeOurCodexHooks(groupsRaw)
		if err != nil {
			return codexHookInstallPlan{}, fmt.Errorf("clean codex hooks for %s: %w", event, err)
		}
		if removed {
			if cleaned == nil {
				delete(hooks, event)
			} else {
				hooks[event] = cleaned
			}
		}
	}

	// Second pass: add the current tclaude group to each required event,
	// appending to any non-tclaude groups already there.
	group := codexMatcherGroup{Hooks: []codexHookCommand{{Type: "command", Command: want}}}
	groupJSON, err := json.Marshal(group)
	if err != nil {
		return codexHookInstallPlan{}, err
	}
	for _, event := range codexHookEvents {
		var groups []json.RawMessage
		if existing, ok := hooks[event]; ok {
			if err := json.Unmarshal(existing, &groups); err != nil {
				return codexHookInstallPlan{}, fmt.Errorf("parse codex hooks for %s: %w", event, err)
			}
		}
		groups = append(groups, groupJSON)
		merged, err := json.Marshal(groups)
		if err != nil {
			return codexHookInstallPlan{}, err
		}
		hooks[event] = merged
	}

	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		return codexHookInstallPlan{}, err
	}
	top["hooks"] = hooksJSON
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return codexHookInstallPlan{}, err
	}
	return codexHookInstallPlan{path: path, out: out, hooks: hooks, want: want}, nil
}

// readCodexHooks reads ~/.codex/hooks.json and returns the event→groups
// map (the "hooks" object) plus the full top-level object (so callers can
// preserve other keys on write). A missing file returns (nil, nil, err)
// with os.IsNotExist(err) true.
func readCodexHooks(path string) (hooks map[string]json.RawMessage, top map[string]json.RawMessage, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	// An empty or whitespace-only file is treated as "no hooks yet" rather
	// than a parse error, so Install can populate it and Check reports it
	// as missing (not unreadable).
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
	}
	top = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	hooks = map[string]json.RawMessage{}
	if raw, ok := top["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, top, fmt.Errorf("parse hooks in %s: %w", path, err)
		}
	}
	return hooks, top, nil
}

// codexHooksContain reports whether an event's groups already include the
// tclaude callback with the current command.
func codexHooksContain(groupsRaw json.RawMessage, want string) bool {
	if len(groupsRaw) == 0 {
		return false
	}
	var groups []codexMatcherGroup
	if err := json.Unmarshal(groupsRaw, &groups); err != nil {
		return false
	}
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Command == want {
				return true
			}
		}
	}
	return false
}

// codexHooksNeedCleanup reports whether an event's groups carry a stale
// (wrong-binary) tclaude hook or a duplicate of the current one — or are
// structurally unparseable. The last case is reported as needing cleanup
// so `Check` warns rather than silently calling the event merely
// "missing": Install's strip pass errors out on the same unparseable
// groups, so the two surfaces agree that the file needs attention.
func codexHooksNeedCleanup(groupsRaw json.RawMessage, want string) bool {
	var groups []codexMatcherGroup
	if err := json.Unmarshal(groupsRaw, &groups); err != nil {
		return true
	}
	ours := 0
	for _, g := range groups {
		for _, h := range g.Hooks {
			if isOurCodexHook(h.Command) {
				if h.Command != want {
					return true
				}
				ours++
			}
		}
	}
	return ours > 1
}

// removeOurCodexHooks strips every tclaude hook from an event's groups,
// dropping a group that becomes empty. Returns the new groups JSON (nil
// when no groups remain), whether anything was removed, and any error.
//
// Non-tclaude content is preserved BYTE-FOR-BYTE: groups and individual
// hooks are carried as json.RawMessage, never round-tripped through a
// typed struct, so a co-resident user hook keeps its optional fields
// (timeout/async/statusMessage/commandWindows) and any unknown keys. Only
// hooks whose command resolves to the tclaude binary are dropped.
func removeOurCodexHooks(groupsRaw json.RawMessage) (json.RawMessage, bool, error) {
	var groups []json.RawMessage
	if err := json.Unmarshal(groupsRaw, &groups); err != nil {
		return groupsRaw, false, err
	}
	var kept []json.RawMessage
	removed := false
	for _, groupRaw := range groups {
		// Parse the group as a generic object so every field other than
		// "hooks" survives untouched.
		var groupObj map[string]json.RawMessage
		if err := json.Unmarshal(groupRaw, &groupObj); err != nil {
			kept = append(kept, groupRaw) // not an object — keep verbatim
			continue
		}
		hooksRaw, ok := groupObj["hooks"]
		if !ok {
			kept = append(kept, groupRaw)
			continue
		}
		var hookList []json.RawMessage
		if err := json.Unmarshal(hooksRaw, &hookList); err != nil {
			kept = append(kept, groupRaw)
			continue
		}

		var keptHooks []json.RawMessage
		groupHadOurs := false
		for _, hookRaw := range hookList {
			if isOurCodexHookRaw(hookRaw) {
				removed = true
				groupHadOurs = true
			} else {
				keptHooks = append(keptHooks, hookRaw)
			}
		}
		if !groupHadOurs {
			kept = append(kept, groupRaw) // untouched group → byte-identical
			continue
		}
		if len(keptHooks) == 0 {
			continue // the whole group was ours → drop it
		}
		// Re-serialize only the hooks array; every other group field
		// (matcher, unknown keys) is preserved as-is.
		newHooks, err := json.Marshal(keptHooks)
		if err != nil {
			return groupsRaw, false, err
		}
		groupObj["hooks"] = newHooks
		rebuilt, err := json.Marshal(groupObj)
		if err != nil {
			return groupsRaw, false, err
		}
		kept = append(kept, rebuilt)
	}
	if !removed {
		return groupsRaw, false, nil
	}
	if len(kept) == 0 {
		return nil, true, nil
	}
	out, err := json.Marshal(kept)
	return out, true, err
}

// isOurCodexHookRaw reports whether a raw hook object is a tclaude command
// hook, peeking only its "command" field (a prompt/agent hook has none →
// not ours).
func isOurCodexHookRaw(hookRaw json.RawMessage) bool {
	var probe struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(hookRaw, &probe); err != nil {
		return false
	}
	return probe.Command != "" && isOurCodexHook(probe.Command)
}
