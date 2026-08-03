package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedCopilotHome points the installer at a disposable COPILOT_HOME and makes
// the installed command a real absolute path whose basename is "tclaude" — the
// same problem seedTclaudeOnPath solves for Codex: under `go test` the running
// binary is "harness.test", so a freshly installed hook would not be
// recognised as tclaude's own by the basename match every installer uses.
func seedCopilotHome(t *testing.T) (home, command string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv(CopilotHomeEnvVar, home)

	bin := filepath.Join(t.TempDir(), "tclaude")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	old := copilotHookCommandString
	copilotHookCommandString = func() string { return bin + " session hook-callback" }
	t.Cleanup(func() { copilotHookCommandString = old })
	// The installed command is the callback PLUS the stdout sink; tests assert
	// against what actually lands in the file.
	return home, copilotHookCommandStr()
}

// TestCopilotHookCommand_RedirectsStdout pins the safety property behind the
// whole design: Copilot treats a hook's stdout as a CONTROL CHANNEL (a Stop
// hook printing {"decision":"block"} makes the agent keep working), so every
// installed command must discard it.
func TestCopilotHookCommand_RedirectsStdout(t *testing.T) {
	_, command := seedCopilotHome(t)
	assert.True(t, strings.HasSuffix(command, " >/dev/null"),
		"installed command must discard stdout, got %q", command)
	assert.True(t, isTclaudeHookCommand(command),
		"the redirect must not stop tclaude recognising its own hook for repair")

	require.NoError(t, copilotHookInstaller{}.Install())
	file := readCopilotHooksFileForTest(t, copilotHookInstaller{}.ConfigTarget())
	for _, event := range CopilotHookEvents {
		assert.Equal(t, command, file.Hooks[event][0].Command, "event %s", event)
	}
}

// readCopilotHooksFileForTest reads the installed drop-in file as a plain
// document, so assertions see exactly the bytes Copilot's loader would.
func readCopilotHooksFileForTest(t *testing.T, path string) struct {
	Version int `json:"version"`
	Hooks   map[string][]struct {
		Type       string `json:"type"`
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeoutSec"`
	} `json:"hooks"`
} {
	t.Helper()
	var file struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Type       string `json:"type"`
			Command    string `json:"command"`
			TimeoutSec int    `json:"timeoutSec"`
		} `json:"hooks"`
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &file))
	return file
}

// TestCopilotHookInstaller_InstallAndCheck installs into a temp COPILOT_HOME
// and pins the on-disk shape against the one verified to fire on the real
// 1.0.77 binary: {"version":1,"hooks":{"<Event>":[{"type":"command",
// "command":…}]}} in <COPILOT_HOME>/hooks/tclaude.json.
func TestCopilotHookInstaller_InstallAndCheck(t *testing.T) {
	home, command := seedCopilotHome(t)

	inst := copilotHookInstaller{}
	require.Equal(t, filepath.Join(home, "hooks", "tclaude.json"), inst.ConfigTarget())

	installed, missing, needsRepair := inst.Check()
	require.False(t, installed, "a fresh COPILOT_HOME has no hooks yet")
	assert.Equal(t, []string{"all"}, missing)
	assert.False(t, needsRepair, "an absent file is missing, not broken")

	require.NoError(t, inst.Install())

	installed, missing, needsRepair = inst.Check()
	assert.True(t, installed, "all events installed; missing=%v", missing)
	assert.Empty(t, missing)
	assert.False(t, needsRepair)

	file := readCopilotHooksFileForTest(t, inst.ConfigTarget())
	assert.Equal(t, 1, file.Version)
	require.Len(t, file.Hooks, len(CopilotHookEvents))
	for _, event := range CopilotHookEvents {
		entries := file.Hooks[event]
		require.Len(t, entries, 1, "event %s", event)
		assert.Equal(t, "command", entries[0].Type, "event %s", event)
		assert.Equal(t, command, entries[0].Command, "event %s", event)
		// Copilot blocks the turn on each hook and defaults to a 30-SECOND
		// timeout, so an explicit small bound is a correctness property of
		// the install, not a nicety.
		assert.Equal(t, 2, entries[0].TimeoutSec, "event %s", event)
	}

	// The events tclaude deliberately does NOT register, each for its own
	// reason: PreToolUse because a non-zero callback exit DENIES the user's
	// tool call; PermissionRequest because it fires on every tool decision
	// (even under --allow-all-tools) and emits an untranslated camelCase
	// payload; UserPromptTransformed because it double-counts a turn.
	for _, unwanted := range []string{"PreToolUse", "PermissionRequest", "UserPromptTransformed", "Notification", "PreCompact"} {
		assert.NotContains(t, file.Hooks, unwanted,
			"registering %s would change Copilot's behavior for the operator", unwanted)
	}
}

// TestCopilotHookInstaller_RepairsMissingTimeout covers the other upgrade
// path: an entry written by a tclaude that predates the explicit timeout has
// the right command but the wrong shape. It must be reported as repairable and
// rewritten, not accepted as current — otherwise an upgraded tclaude would
// leave the operator on Copilot's 30-second default forever.
func TestCopilotHookInstaller_RepairsMissingTimeout(t *testing.T) {
	home, command := seedCopilotHome(t)

	seed := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"Stop": []any{map[string]any{"type": "command", "command": command}},
		},
	}
	writeCopilotHookSeed(t, filepath.Join(home, "hooks", "tclaude.json"), seed)

	inst := copilotHookInstaller{}
	installed, missing, needsRepair := inst.Check()
	assert.False(t, installed)
	assert.Contains(t, missing, "Stop", "a timeout-less entry is not the current entry")
	assert.True(t, needsRepair)

	require.NoError(t, inst.Install())

	file := readCopilotHooksFileForTest(t, inst.ConfigTarget())
	require.Len(t, file.Hooks["Stop"], 1, "the old entry is replaced, not duplicated")
	assert.Equal(t, 2, file.Hooks["Stop"][0].TimeoutSec)
}

// TestCopilotHookInstaller_RemovesRetiredEvents covers the safety-critical
// downgrade path. An earlier tclaude registered PreToolUse before the fixture
// lab established that a non-zero hook exit DENIES the user's tool call. An
// upgraded tclaude must actively remove that entry: leaving it behind would
// keep a failing callback able to block the operator's tools, and the entry is
// on an event the current installer never visits.
func TestCopilotHookInstaller_RemovesRetiredEvents(t *testing.T) {
	home, command := seedCopilotHome(t)

	seed := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{"type": "command", "command": command}},
			"Stop":       []any{map[string]any{"type": "command", "command": command}},
		},
	}
	writeCopilotHookSeed(t, filepath.Join(home, "hooks", "tclaude.json"), seed)

	require.NoError(t, copilotHookInstaller{}.Install())

	file := readCopilotHooksFileForTest(t, copilotHookInstaller{}.ConfigTarget())
	assert.NotContains(t, file.Hooks, "PreToolUse",
		"a tclaude entry on a retired event must be removed, not left running")

	// A retired event that still holds someone ELSE'S hook keeps that hook: the
	// removal is scoped to tclaude's own entries, not to the event.
	seed["hooks"].(map[string]any)["PreToolUse"] = []any{
		map[string]any{"type": "command", "command": command},
		map[string]any{"type": "command", "command": "/usr/local/bin/audit-logger"},
	}
	writeCopilotHookSeed(t, filepath.Join(home, "hooks", "tclaude.json"), seed)
	require.NoError(t, copilotHookInstaller{}.Install())

	file = readCopilotHooksFileForTest(t, copilotHookInstaller{}.ConfigTarget())
	require.Len(t, file.Hooks["PreToolUse"], 1)
	assert.Equal(t, "/usr/local/bin/audit-logger", file.Hooks["PreToolUse"][0].Command)
}

// TestCopilotHookInstaller_Idempotent installs twice and asserts the second
// run neither duplicates an entry nor reports a repair.
func TestCopilotHookInstaller_Idempotent(t *testing.T) {
	seedCopilotHome(t)
	inst := copilotHookInstaller{}

	require.NoError(t, inst.Install())
	first, err := os.ReadFile(inst.ConfigTarget())
	require.NoError(t, err)

	require.NoError(t, inst.Install())
	second, err := os.ReadFile(inst.ConfigTarget())
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second), "a repeat install must be byte-identical")

	file := readCopilotHooksFileForTest(t, inst.ConfigTarget())
	for _, event := range CopilotHookEvents {
		assert.Len(t, file.Hooks[event], 1, "event %s must not accumulate duplicates", event)
	}
}

// TestCopilotHookInstaller_RepairsStaleBinary covers the upgrade path: a hook
// installed by an older tclaude at a different absolute path must be
// recognised as ours, reported as needing repair, and replaced (not appended
// to) on the next install.
func TestCopilotHookInstaller_RepairsStaleBinary(t *testing.T) {
	home, command := seedCopilotHome(t)

	stale := filepath.Join(home, "old", "tclaude") + " session hook-callback"
	seed := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"type": "command", "command": stale}},
		},
	}
	writeCopilotHookSeed(t, filepath.Join(home, "hooks", "tclaude.json"), seed)

	inst := copilotHookInstaller{}
	installed, missing, needsRepair := inst.Check()
	assert.False(t, installed)
	assert.Contains(t, missing, "SessionStart")
	assert.True(t, needsRepair, "a stale tclaude path must be reported as repairable")

	require.NoError(t, inst.Install())

	file := readCopilotHooksFileForTest(t, inst.ConfigTarget())
	require.Len(t, file.Hooks["SessionStart"], 1, "the stale entry is replaced, not appended to")
	assert.Equal(t, command, file.Hooks["SessionStart"][0].Command)

	installed, _, needsRepair = inst.Check()
	assert.True(t, installed)
	assert.False(t, needsRepair)
}

// TestCopilotHookInstaller_PreservesForeignContent is the "we own the file but
// not what someone else put in it" guarantee: an unrelated hook entry keeps
// every field tclaude does not model, and an unrecognized top-level key
// survives the rewrite.
func TestCopilotHookInstaller_PreservesForeignContent(t *testing.T) {
	home, _ := seedCopilotHome(t)

	seed := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"type":       "command",
				"command":    "/usr/local/bin/audit-logger",
				"timeoutSec": 30,
				"matcher":    "Bash",
			}},
			"PreCompact": []any{map[string]any{"type": "command", "command": "/usr/local/bin/snapshot"}},
		},
		"disableAllHooks": false,
	}
	writeCopilotHookSeed(t, filepath.Join(home, "hooks", "tclaude.json"), seed)

	require.NoError(t, copilotHookInstaller{}.Install())

	data, err := os.ReadFile(filepath.Join(home, "hooks", "tclaude.json"))
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &got))

	assert.JSONEq(t, `false`, string(got["disableAllHooks"]), "unknown top-level keys survive")

	var hooks map[string][]map[string]any
	require.NoError(t, json.Unmarshal(got["hooks"], &hooks))

	require.Len(t, hooks["PreCompact"], 1, "an event tclaude does not register is left alone")
	assert.Equal(t, "/usr/local/bin/snapshot", hooks["PreCompact"][0]["command"])

	require.Len(t, hooks["SessionStart"], 2, "tclaude's entry is added alongside the foreign one")
	foreign := hooks["SessionStart"][0]
	assert.Equal(t, "/usr/local/bin/audit-logger", foreign["command"])
	assert.EqualValues(t, 30, foreign["timeoutSec"], "optional fields tclaude does not model survive")
	assert.Equal(t, "Bash", foreign["matcher"])
}

// TestCopilotHookInstaller_MalformedFileFailsClosed asserts tclaude refuses to
// overwrite a file it cannot parse. A drop-in file it cannot reproduce might
// be a hand-written config; rewriting it from scratch would delete work
// nobody asked tclaude to touch.
func TestCopilotHookInstaller_MalformedFileFailsClosed(t *testing.T) {
	home, _ := seedCopilotHome(t)
	path := filepath.Join(home, "hooks", "tclaude.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	original := []byte("{\"hooks\": {\"SessionStart\": [ truncated\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	inst := copilotHookInstaller{}
	installed, missing, needsRepair := inst.Check()
	assert.False(t, installed)
	assert.True(t, needsRepair, "an unparseable file needs a human, not a silent rewrite")
	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "all (")

	require.Error(t, inst.Install(), "install must refuse rather than clobber")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after), "the file is left untouched")
}

// TestCopilotHookInstaller_EmptyFileInstalls covers the one case an empty file
// is allowed to mean "nothing installed yet": tclaude owns this path, so a
// zero-length file there carries no operator intent to preserve.
func TestCopilotHookInstaller_EmptyFileInstalls(t *testing.T) {
	home, command := seedCopilotHome(t)
	path := filepath.Join(home, "hooks", "tclaude.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("  \n"), 0o600))

	require.NoError(t, copilotHookInstaller{}.Install())

	file := readCopilotHooksFileForTest(t, path)
	assert.Equal(t, command, file.Hooks["Stop"][0].Command)
}

// TestCopilotHookInstaller_PreservesFileMode asserts an existing file's
// permissions survive the rewrite — the drop-in lives beside Copilot's own
// 0600 state, and an install must not loosen what the operator chose.
func TestCopilotHookInstaller_PreservesFileMode(t *testing.T) {
	home, _ := seedCopilotHome(t)
	path := filepath.Join(home, "hooks", "tclaude.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"hooks":{}}`), 0o600))

	require.NoError(t, copilotHookInstaller{}.Install())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestCopilotHookInstaller_ConcurrentInstall drives several installs at once
// through the process mutex + flock, then asserts the result is a single clean
// set of entries rather than a torn or duplicated file.
func TestCopilotHookInstaller_ConcurrentInstall(t *testing.T) {
	seedCopilotHome(t)
	inst := copilotHookInstaller{}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = inst.Install()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	installed, missing, needsRepair := inst.Check()
	assert.True(t, installed, "missing=%v", missing)
	assert.False(t, needsRepair)
	file := readCopilotHooksFileForTest(t, inst.ConfigTarget())
	for _, event := range CopilotHookEvents {
		assert.Len(t, file.Hooks[event], 1, "event %s", event)
	}
}

// TestCopilotHookInstaller_Descriptor pins the capability wiring: the
// descriptor advertises hooks (which is what makes `tclaude setup` install and
// check them), and does NOT advertise a trust store, because Copilot's user
// hooks fired with no trust step of any kind.
func TestCopilotHookInstaller_Descriptor(t *testing.T) {
	h, ok := Get(CopilotName)
	require.True(t, ok)
	require.True(t, h.SupportsHooks())
	assert.Empty(t, h.Hooks.TrustNote(), "no manual enable step exists to report")
	_, trustCapable := h.Hooks.(TrustedHookInstaller)
	assert.False(t, trustCapable, "no separate executable-trust store gates Copilot user hooks")
}

func writeCopilotHookSeed(t *testing.T, path string, seed map[string]any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.MarshalIndent(seed, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
