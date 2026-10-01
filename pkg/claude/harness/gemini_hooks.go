package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/gofrs/flock"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// Gemini CLI hook installation.
//
// Read from the source at GeminiPinnedVersion (packages/core/src/hooks and
// docs/hooks/reference.md), not observed from a live pane — no Gemini account
// was available. The contract it relies on:
//
//   - Hooks live in settings.json under `hooks`, keyed by Gemini's own event
//     names, each holding matcher groups `{matcher?, hooks: [{type, command,
//     name?, timeout?}]}` — Claude Code's shape, with the timeout in
//     MILLISECONDS. A group without a matcher matches every occurrence.
//   - The command runs through the user's shell with the hook payload as JSON
//     on stdin: snake_case session_id, transcript_path, cwd, hook_event_name,
//     timestamp, plus per-event fields. The CLI process environment is passed
//     through (Gemini only redacts names that look like secrets), so the
//     TCLAUDE_* variables a tclaude pane exports reach the callback.
//   - Event NAMES differ from Claude Code's (BeforeAgent, AfterAgent,
//     AfterTool, …); the callback maps them onto tclaude's vocabulary (see
//     session.normalizeGeminiHookEvent).
//   - Hooks from settings run only in a TRUSTED folder
//     (hookRegistry.processHooksFromConfig). In an untrusted folder Gemini
//     shows its trust dialog, and the hooks stay off until the folder is
//     trusted — which is why the TrustNote says so.
//
// The settings file is SHARED with every other Gemini setting, so the install
// is a surgical read-modify-write of the `hooks` key — the same approach as
// Claude Code's settings.json installer. Gemini itself reads settings.json as
// JSON-with-comments; a file that does not parse as strict JSON is refused
// rather than rewritten, since tclaude cannot round-trip the comments.

var installGeminiHooksMu sync.Mutex

const (
	installGeminiHooksLockTimeout = 5 * time.Second
	installGeminiHooksLockRetry   = 10 * time.Millisecond
)

// GeminiHookEvents is the set of Gemini events tclaude registers, in Gemini's
// own vocabulary.
//
//   - BeforeAgent / AfterAgent bracket one user turn: the working→idle
//     transition. AfterAgent fires once per turn after the final response.
//   - AfterTool keeps a long turn visibly alive and clears a resolved
//     permission wait.
//   - Notification fires with notification_type ToolPermission exactly when
//     Gemini shows a tool-confirmation dialog (scheduler/confirmation.ts) — a
//     real "a human must answer this" signal.
//   - SessionStart / SessionEnd. SessionEnd is best effort: the CLI does not
//     wait for it (see SessionEndBestEffort).
//
// Deliberately NOT installed:
//
//   - BeforeTool: Gemini reads exit code 2 from it as "block this tool". The
//     installed command is exit-neutral, but the operator's tool calls are not
//     worth a status detail AfterTool reports a moment later anyway.
//   - PreCompress: tclaude's PreCompact handling is a gate that may answer
//     "block", while Gemini's PreCompress is advisory and fired without
//     waiting. Mapping it would let tclaude believe a compaction was refused
//     while Gemini runs it regardless.
//   - BeforeModel / AfterModel / BeforeToolSelection: per-request and
//     per-chunk events that would run the callback on every streamed chunk.
var GeminiHookEvents = []string{
	"SessionStart",
	"BeforeAgent",
	"AfterTool",
	"AfterAgent",
	"Notification",
	"SessionEnd",
}

// geminiHookTimeoutMs bounds how long a Gemini turn waits on tclaude's
// callback. Gemini awaits BeforeAgent/AfterAgent/AfterTool hooks inside the
// turn and its default is 60 s, so a wedged callback would stall every turn.
// A killed hook is a benign failure (logged, the turn continues).
const geminiHookTimeoutMs = 5000

// geminiHookName labels the entry in Gemini's hook UI and is the token an
// operator would list in hooksConfig.disabled to switch it off.
const geminiHookName = "tclaude"

// geminiHookOutputSink makes the installed command inert towards Gemini.
//
// Gemini reads a hook's STDOUT as a control document — and when stdout is
// empty it parses STDERR instead (hookRunner: `stdout.trim() ||
// stderr.trim()`), showing plain text as a system message. Exit code 2 blocks
// (BeforeAgent discards the prompt; AfterAgent forces a retry turn) and other
// non-zero codes surface as warnings. Sinking both streams and forcing a zero
// exit means tclaude can never steer, retry, block or clutter the operator's
// Gemini session, whatever the callback does on a bad day. The callback's own
// diagnostics go to tclaude's log, not to these streams.
const geminiHookOutputSink = " >/dev/null 2>&1 || true"

var geminiHookCommandString = func() string {
	return clcommon.HookCallbackCommand
}

func geminiHookCommandStr() string {
	return geminiHookCommandString() + geminiHookOutputSink
}

// geminiSettingsPath is the user-level settings file Gemini merges hooks from.
func geminiSettingsPath() string {
	dir := geminiDir()
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, "settings.json")
}

type geminiCommandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Name    string `json:"name"`
	Timeout int    `json:"timeout"`
}

type geminiHookGroup struct {
	Hooks []geminiCommandHook `json:"hooks"`
}

// geminiHookInstaller installs the tclaude callback into Gemini's user
// settings. Plain HookInstaller: user-level hooks have no executable-trust
// store (trusted_hooks.json covers PROJECT hooks only); the gate is folder
// trust, which TrustNote describes.
type geminiHookInstaller struct{}

func (geminiHookInstaller) ConfigTarget() string { return geminiSettingsPath() }

func (geminiHookInstaller) TrustNote() string {
	return "Gemini CLI runs settings hooks only in trusted folders; trust each project " +
		"directory once (Gemini's trust dialog, or `--trust-dir` on a tclaude launch) " +
		"for live status to work there"
}

func (geminiHookInstaller) Check() (installed bool, missing []string, needsRepair bool) {
	path := geminiSettingsPath()
	if path == "" {
		return false, []string{"all"}, false
	}
	_, hooks, err := readGeminiSettingsHooks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, []string{"all"}, false
		}
		return false, []string{"all (" + err.Error() + ")"}, true
	}
	want := geminiDesiredHookGroup()
	wanted := map[string]bool{}
	for _, event := range GeminiHookEvents {
		wanted[event] = true
	}
	for event, groups := range hooks {
		ours, current := geminiCountOurGroups(groups, want)
		if ours > current || current > 1 || (!wanted[event] && ours > 0) {
			needsRepair = true
		}
	}
	for _, event := range GeminiHookEvents {
		if _, current := geminiCountOurGroups(hooks[event], want); current == 0 {
			missing = append(missing, event)
		}
	}
	return len(missing) == 0, missing, needsRepair
}

func (geminiHookInstaller) Install() error {
	installGeminiHooksMu.Lock()
	defer installGeminiHooksMu.Unlock()

	path := geminiSettingsPath()
	if path == "" {
		return fmt.Errorf("cannot determine Gemini settings path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create Gemini settings directory: %w", err)
	}
	target, err := atomicWriteTarget(path)
	if err != nil {
		return err
	}
	fileLock := flock.New(target + ".tclaude.lock")
	lockCtx, cancel := context.WithTimeout(context.Background(), installGeminiHooksLockTimeout)
	defer cancel()
	locked, err := fileLock.TryLockContext(lockCtx, installGeminiHooksLockRetry)
	if err != nil {
		return fmt.Errorf("lock Gemini settings: %w", err)
	}
	if !locked {
		return fmt.Errorf("lock Gemini settings: timed out")
	}
	defer func() { _ = fileLock.Unlock() }()

	out, err := planGeminiHookInstall(target)
	if err != nil {
		return err
	}
	if err := atomicWritePreservingMode(target, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// planGeminiHookInstall is the pure half of Install: strip every tclaude
// group from every event, then add exactly one current group per event.
// Every other settings key, and every non-tclaude hook, is carried through.
func planGeminiHookInstall(path string) ([]byte, error) {
	settings, hooks, err := readGeminiSettingsHooks(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	if hooks == nil {
		hooks = map[string][]json.RawMessage{}
	}
	for event, groups := range hooks {
		kept := removeOurGeminiHooks(groups)
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = kept
	}
	group, err := json.Marshal(geminiDesiredHookGroup())
	if err != nil {
		return nil, err
	}
	for _, event := range GeminiHookEvents {
		hooks[event] = append(hooks[event], group)
	}
	rawHooks, err := json.Marshal(hooks)
	if err != nil {
		return nil, err
	}
	settings["hooks"] = rawHooks
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// readGeminiSettingsHooks reads settings.json and its `hooks` object. A
// missing or empty file is os.IsNotExist. A file that is not strict JSON —
// including the comments Gemini itself tolerates — is an error: rewriting it
// would drop content tclaude cannot reproduce.
func readGeminiSettingsHooks(path string) (map[string]json.RawMessage, map[string][]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, emptyFileAsNotExist(path)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, nil, fmt.Errorf("parse %s (tclaude edits only strict JSON; remove comments "+
			"or add the hooks by hand): %w", path, err)
	}
	if settings == nil {
		return nil, nil, fmt.Errorf("parse %s: top level is not an object", path)
	}
	var hooks map[string][]json.RawMessage
	if raw, ok := settings["hooks"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, fmt.Errorf("parse hooks in %s: %w", path, err)
		}
	}
	return settings, hooks, nil
}

func geminiDesiredHookGroup() geminiHookGroup {
	return geminiHookGroup{Hooks: []geminiCommandHook{{
		Type:    "command",
		Command: geminiHookCommandStr(),
		Name:    geminiHookName,
		Timeout: geminiHookTimeoutMs,
	}}}
}

// geminiGroupIsOurs reports whether a matcher group holds a tclaude command.
func geminiGroupIsOurs(raw json.RawMessage) bool {
	var probe struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	for _, h := range probe.Hooks {
		if isTclaudeHookCommand(h.Command) {
			return true
		}
	}
	return false
}

// geminiCountOurGroups counts tclaude groups in one event, and how many of
// them are exactly the current desired group.
func geminiCountOurGroups(groups []json.RawMessage, want geminiHookGroup) (ours, current int) {
	wantRaw, err := json.Marshal(want)
	if err != nil {
		return 0, 0
	}
	var wantVal any
	_ = json.Unmarshal(wantRaw, &wantVal)
	for _, raw := range groups {
		if !geminiGroupIsOurs(raw) {
			continue
		}
		ours++
		var got any
		if json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, wantVal) {
			current++
		}
	}
	return ours, current
}

// removeOurGeminiHooks strips every tclaude command from an event's groups.
// A group left with no commands is dropped; a group that also carries someone
// else's command keeps it, with every field of the group and of that command
// carried through as raw JSON.
func removeOurGeminiHooks(groups []json.RawMessage) []json.RawMessage {
	var kept []json.RawMessage
	for _, raw := range groups {
		if !geminiGroupIsOurs(raw) {
			kept = append(kept, raw)
			continue
		}
		var group map[string]json.RawMessage
		var entries []json.RawMessage
		if json.Unmarshal(raw, &group) != nil || json.Unmarshal(group["hooks"], &entries) != nil {
			continue
		}
		var others []json.RawMessage
		for _, entry := range entries {
			var probe struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(entry, &probe) == nil && isTclaudeHookCommand(probe.Command) {
				continue
			}
			others = append(others, entry)
		}
		if len(others) == 0 {
			continue
		}
		rawOthers, err := json.Marshal(others)
		if err != nil {
			continue
		}
		group["hooks"] = rawOthers
		rebuilt, err := json.Marshal(group)
		if err != nil {
			continue
		}
		kept = append(kept, rebuilt)
	}
	return kept
}
