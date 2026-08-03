package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// Copilot hook installation.
//
// Everything in this file rests on runtime evidence from the pinned 1.0.77
// binary running credential-free in pkg/claude/harness/copilotfixture, not on
// published documentation — GitHub documents neither the drop-in hooks
// directory nor the compatibility dialect. Two verified contracts carry the
// design, and the fixtures under copilotfixture/testdata/<version>/hooks are
// what pin them:
//
//  1. A tclaude-OWNED FILE at <COPILOT_HOME>/hooks/tclaude.json fires. Copilot
//     merges it with any hooks in the user's config.json rather than one
//     shadowing the other. That is the difference between owning a file and
//     read-modify-writing someone else's: COPILOT_HOME/config.json is shared
//     with unrelated settings, is written by the CLI itself ("This file is
//     managed automatically"), and is not even strict JSON — a stock 1.0.77
//     config.json opens with two `//` banner lines. tclaude would have had to
//     preserve a JSONC banner, key order and every unrelated key, and fail
//     closed whenever it could not. An owned file removes that entire class of
//     risk. (<COPILOT_HOME>/config/hooks/ — the path the CLI's own internals
//     name — was tested and does NOT fire.)
//
//  2. Copilot accepts CLAUDE CODE'S event names and, when they are used, emits
//     Claude Code's payload: snake_case fields, hook_event_name, session_id,
//     ISO-8601 timestamps, tool_input as an object, and even Claude's tool
//     NAMES ("Bash", not Copilot's "bash"). Registering camelCase Copilot names
//     instead yields a camelCase Copilot payload. tclaude therefore installs
//     the Claude dialect and needs no translator at all: the existing
//     HookCallbackInput decodes Copilot field-for-field, exactly as it already
//     does for Codex.
//
// If either contract changes in a later CLI, the fixture tests fail rather
// than tclaude silently installing hooks that never fire.

var installCopilotHooksMu sync.Mutex

const (
	installCopilotHooksLockTimeout = 5 * time.Second
	installCopilotHooksLockRetry   = 10 * time.Millisecond
)

// CopilotHookEvents is the set of events tclaude registers, spelled in Claude
// Code's vocabulary because that is what selects the compatible payload
// dialect. Every one of them was observed firing from the real binary.
//
// Two events that DO fire are deliberately not installed:
//
//   - UserPromptTransformed fires for the same turn as UserPromptSubmit, so
//     registering both would double-count one prompt. Its payload also carries
//     the model-facing rendering of the prompt (injected reminder blocks and
//     all), which is strictly more than the user typed, for no gain.
//   - PermissionRequest is not what its name suggests, and it is the one event
//     that does NOT honor the dialect: registered under its Claude name it
//     still emits the raw camelCase Copilot payload (sessionId, a millisecond
//     number timestamp, Copilot's lowercase tool names, no hook_event_name).
//     It also fires under --allow-all-tools, i.e. on decisions no human was
//     ever asked about, because it runs the permission rules engine rather
//     than a prompt. Mapping it would park every Copilot session in
//     "awaiting permission" on every tool call and raise a needs-attention
//     notification each time. Whether Copilot exposes a real "a human must
//     answer this" signal to hooks is unverified; tclaude claims nothing until
//     it is. Tracked for TCL-976.
//
// The set is FIXED — no standing-order extras, unlike the Codex installer.
// A selectable Copilot hook event would be a promise that Copilot reads a
// hook's response, and no evidence of a response channel exists yet.
var CopilotHookEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"Stop",
	"SessionEnd",
}

// CopilotHomeEnvVar is Copilot CLI's own override for its home directory.
// tclaude honors it so an operator who relocates COPILOT_HOME (and the
// fixture lab, which always sets it) gets hooks in the directory the CLI will
// actually read.
const CopilotHomeEnvVar = "COPILOT_HOME"

// copilotHome resolves COPILOT_HOME, falling back to the documented default
// ~/.copilot. Empty means "cannot determine", which every caller treats as a
// hard failure rather than guessing a path.
func copilotHome() string {
	if dir := os.Getenv(CopilotHomeEnvVar); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot")
}

// copilotHooksPath is the tclaude-owned drop-in file. The name is tclaude's
// own: a user's hooks live in their own files in the same directory (or in
// config.json) and are never read, written, or parsed by this installer.
func copilotHooksPath() string {
	home := copilotHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "hooks", "tclaude.json")
}

// copilotHookFileVersion is the schema version Copilot's hook loader expects
// in a drop-in file. Verified accepted at 1.0.77.
const copilotHookFileVersion = 1

// copilotCommandHook is one entry in an event's array. Copilot's hook config
// is an internally-tagged union; "command" is the shell form. The optional
// fields (timeoutSec, matcher) are omitted: tclaude wants every event
// regardless of tool, and the callback is a fast local write.
type copilotCommandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// copilotHookFile is the whole drop-in document.
type copilotHookFile struct {
	Version int                          `json:"version"`
	Hooks   map[string][]json.RawMessage `json:"hooks"`
	Extra   map[string]json.RawMessage   `json:"-"`
}

// copilotHookCommandString is the callback command tclaude installs: the same
// harness-agnostic `tclaude session hook-callback` every other harness
// invokes. No per-harness flag is needed because Copilot's compatible dialect
// IS the canonical payload. Kept behind a var so tests can pin it.
var copilotHookCommandString = func() string {
	return clcommon.DetectAbsoluteCmd("session", "hook-callback")
}

func copilotHookCommandStr() string { return copilotHookCommandString() }

// copilotHookInstaller installs the tclaude callback into the owned drop-in
// file. It implements plain HookInstaller and deliberately NOT
// TrustedHookInstaller: no separate executable-trust store gates Copilot's
// user hooks — the drop-in fired with no trust dialog, no trustedFolders
// entry, and outside a git repo. Asserting a trust contract tclaude cannot
// honor is the failure mode copilot.go's header warns about.
type copilotHookInstaller struct{}

func (copilotHookInstaller) ConfigTarget() string { return copilotHooksPath() }

// TrustNote is empty: setup completes everything: there is no manual enable
// step for Copilot user hooks.
func (copilotHookInstaller) TrustNote() string { return "" }

// Check reports whether the tclaude callback is installed for every event with
// the CURRENT binary path. missing lists events that lack it; needsRepair is
// true when the file carries a stale (wrong-binary) or duplicate tclaude hook,
// or is structurally unusable.
func (copilotHookInstaller) Check() (installed bool, missing []string, needsRepair bool) {
	path := copilotHooksPath()
	if path == "" {
		return false, []string{"all"}, false
	}
	file, err := readCopilotHookFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, []string{"all"}, false
		}
		// Unparseable: report it as needing repair rather than merely
		// missing, so `setup --check` says something true. Install refuses to
		// overwrite it, so the two surfaces agree that a human must look.
		return false, []string{"all (" + err.Error() + ")"}, true
	}

	want := copilotHookCommandStr()
	if file.Version != copilotHookFileVersion {
		needsRepair = true
	}
	for _, entries := range file.Hooks {
		if copilotHooksNeedCleanup(entries, want) {
			needsRepair = true
			break
		}
	}
	for _, event := range CopilotHookEvents {
		if !copilotHooksContain(file.Hooks[event], want) {
			missing = append(missing, event)
		}
	}
	return len(missing) == 0, missing, needsRepair
}

// Install installs or repairs the tclaude callback for every event.
// Idempotent: a second call with the hooks already present rewrites the same
// bytes, never a duplicate entry.
//
// The file is tclaude's, but the merge is still surgical. A hook entry that is
// not tclaude's — an operator who added one to this file by hand — is carried
// through as json.RawMessage with its optional fields intact, and so is any
// unrecognized top-level key. Owning a file is a reason to be able to rewrite
// it, not a reason to discard what someone else put in it.
func (copilotHookInstaller) Install() error {
	return withCopilotHooksInstallLock(func() error {
		path := copilotHooksPath()
		out, err := planCopilotHookInstall(path)
		if err != nil {
			return err
		}
		if err := atomicWritePreservingMode(path, out, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	})
}

// withCopilotHooksInstallLock serializes every read-modify-write of the
// drop-in file. The process mutex handles goroutines; the flock coordinates
// independent tclaude processes (two panes launching at once).
func withCopilotHooksInstallLock(fn func() error) error {
	installCopilotHooksMu.Lock()
	defer installCopilotHooksMu.Unlock()

	path := copilotHooksPath()
	if path == "" {
		return fmt.Errorf("cannot determine Copilot hooks path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create Copilot hooks directory: %w", err)
	}
	fileLock := flock.New(path + ".tclaude.lock")
	lockCtx, cancelLock := context.WithTimeout(
		context.Background(), installCopilotHooksLockTimeout)
	defer cancelLock()
	locked, err := fileLock.TryLockContext(lockCtx, installCopilotHooksLockRetry)
	if err != nil {
		return fmt.Errorf("lock Copilot hooks: %w", err)
	}
	if !locked {
		return fmt.Errorf("lock Copilot hooks: timed out")
	}
	defer func() { _ = fileLock.Unlock() }()
	return fn()
}

// planCopilotHookInstall is the pure half of Install: read the current file,
// strip every tclaude entry (clearing stale binaries and duplicates), then add
// exactly one current entry per required event.
func planCopilotHookInstall(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("cannot determine Copilot hooks path")
	}
	file, err := readCopilotHookFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if file.Hooks == nil {
		file.Hooks = map[string][]json.RawMessage{}
	}

	want := copilotHookCommandStr()
	entry, err := json.Marshal(copilotCommandHook{Type: "command", Command: want})
	if err != nil {
		return nil, err
	}

	for event, entries := range file.Hooks {
		kept := removeOurCopilotHooks(entries)
		if len(kept) == 0 {
			delete(file.Hooks, event)
			continue
		}
		file.Hooks[event] = kept
	}
	for _, event := range CopilotHookEvents {
		file.Hooks[event] = append(file.Hooks[event], entry)
	}

	top := map[string]json.RawMessage{}
	maps.Copy(top, file.Extra)
	version, err := json.Marshal(copilotHookFileVersion)
	if err != nil {
		return nil, err
	}
	top["version"] = version
	hooks, err := json.Marshal(file.Hooks)
	if err != nil {
		return nil, err
	}
	top["hooks"] = hooks
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// readCopilotHookFile reads the drop-in file. A missing file is returned as an
// os.IsNotExist error so callers can distinguish "nothing installed yet" from
// "installed but broken"; an empty or whitespace-only file counts as the
// former, since tclaude owns this path and an empty file there carries no
// operator intent to preserve. Malformed JSON is an ERROR — that file might be
// a hand-written config whose meaning tclaude cannot reproduce, and silently
// starting from empty would delete it.
func readCopilotHookFile(path string) (copilotHookFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return copilotHookFile{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return copilotHookFile{}, emptyFileAsNotExist(path)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return copilotHookFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	out := copilotHookFile{Extra: map[string]json.RawMessage{}}
	for key, value := range top {
		switch key {
		case "version":
			if err := json.Unmarshal(value, &out.Version); err != nil {
				return copilotHookFile{}, fmt.Errorf("parse version in %s: %w", path, err)
			}
		case "hooks":
			if err := json.Unmarshal(value, &out.Hooks); err != nil {
				return copilotHookFile{}, fmt.Errorf("parse hooks in %s: %w", path, err)
			}
		default:
			out.Extra[key] = value
		}
	}
	return out, nil
}

// emptyFileAsNotExist builds a not-exist error for a path that exists but is
// empty, so one branch in every caller covers both "no file" and "no content".
func emptyFileAsNotExist(path string) error {
	return &os.PathError{Op: "read", Path: path, Err: os.ErrNotExist}
}

// copilotHooksContain reports whether an event's entries already include the
// tclaude callback with the current command.
func copilotHooksContain(entries []json.RawMessage, want string) bool {
	for _, raw := range entries {
		if copilotHookCommandOf(raw) == want {
			return true
		}
	}
	return false
}

// copilotHooksNeedCleanup reports whether an event's entries carry a stale
// (wrong-binary) tclaude hook or a duplicate of the current one.
func copilotHooksNeedCleanup(entries []json.RawMessage, want string) bool {
	ours := 0
	for _, raw := range entries {
		command := copilotHookCommandOf(raw)
		if !isTclaudeHookCommand(command) {
			continue
		}
		if command != want {
			return true
		}
		ours++
	}
	return ours > 1
}

// removeOurCopilotHooks strips every tclaude entry from an event's entries.
// Non-tclaude entries are carried through as raw bytes, so a co-resident hook
// keeps every field tclaude does not model.
func removeOurCopilotHooks(entries []json.RawMessage) []json.RawMessage {
	var kept []json.RawMessage
	for _, raw := range entries {
		if isTclaudeHookCommand(copilotHookCommandOf(raw)) {
			continue
		}
		kept = append(kept, raw)
	}
	return kept
}

// copilotHookCommandOf peeks an entry's "command" field. A non-object entry,
// or one of Copilot's other hook forms (exec/http/prompt), has none and is
// therefore never treated as tclaude's.
func copilotHookCommandOf(raw json.RawMessage) string {
	var probe struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Command
}
