package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/hookevents"
)

func TestCodexHookInstaller_TrustsEnabledNativeSelectorHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	_, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:         "trusted-codex-session-end",
		TargetKind:   db.StandingTargetGroup,
		GroupID:      1,
		Summary:      "Close out the session.",
		TriggerEvent: db.StandingTriggerHookEvent,
		HookSelectors: []hookevents.Selector{{
			Harness: hookevents.HarnessCodex,
			Event:   "SessionEnd",
		}},
		Timing: db.StandingTimingNextTurn, Cadence: db.StandingCadenceAlways,
		Enabled: true,
	})
	require.NoError(t, err)

	inst := codexHookInstaller{}
	require.NoError(t, inst.InstallTrusted())
	assert.True(t, inst.Trusted())
	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), ":session_end:")
	assert.Contains(t, string(config), fmt.Sprintf("sha256:%064x", len(desiredCodexHookEvents())))
}

func TestPlanCodexHookTrust_AddsPreservesAndIsIdempotent(t *testing.T) {
	entries := []codexHookTrustEntry{
		{Key: "/home/me/.codex/hooks.json:session_start:1:0", Hash: "sha256:new-session"},
		{Key: "/home/me/.codex/hooks.json:stop:0:0", Hash: "sha256:new-stop"},
	}
	existing := "" +
		"model = \"gpt-5\" # keep me\n" +
		"\n" +
		"[hooks.state.\"/home/me/.codex/hooks.json:session_start:1:0\"]\n" +
		"enabled = false\n" +
		"trusted_hash = \"sha256:old\"\n"

	changed, out, err := planCodexHookTrust([]byte(existing), entries)
	require.NoError(t, err)
	require.True(t, changed)
	s := string(out)
	assert.Contains(t, s, `model = "gpt-5" # keep me`)
	assert.Contains(t, s, "enabled = false", "an explicit disabled state is preserved")
	assert.Contains(t, s, `trusted_hash = "sha256:new-session"`)
	assert.Contains(t, s, `[hooks.state."/home/me/.codex/hooks.json:stop:0:0"]`)
	assert.Contains(t, s, `trusted_hash = "sha256:new-stop"`)
	assert.NotContains(t, s, "sha256:old")

	changed, again, err := planCodexHookTrust(out, entries)
	require.NoError(t, err)
	assert.False(t, changed, "the second trust plan must be a clean no-op")
	assert.Equal(t, out, again)
}

func TestPlanCodexHookTrust_RefusesConflictingInlineState(t *testing.T) {
	entry := codexHookTrustEntry{Key: "/x/hooks.json:stop:0:0", Hash: "sha256:abc"}
	for _, existing := range []string{
		`hooks = { state = {} }`,
		"[hooks]\nstate = {}\n",
		`hooks.state = {}`,
	} {
		changed, out, err := planCodexHookTrust([]byte(existing), []codexHookTrustEntry{entry})
		require.Error(t, err, "config %q must be refused", existing)
		assert.False(t, changed)
		assert.Nil(t, out)
	}
}

func TestPlanCodexHookTrust_RefusesAlternateSpellingOfExactKey(t *testing.T) {
	entry := codexHookTrustEntry{Key: "/x/hooks.json:stop:0:0", Hash: "sha256:abc"}
	// Semantically this is the same table Codex normally writes with a basic
	// double-quoted key. Appending the standard spelling would duplicate it.
	existing := "[hooks.state.'/x/hooks.json:stop:0:0']\ntrusted_hash = 'sha256:old'\n"
	changed, out, err := planCodexHookTrust([]byte(existing), []codexHookTrustEntry{entry})
	require.Error(t, err)
	assert.False(t, changed)
	assert.Nil(t, out)
}

func TestPlanCodexHookTrust_PreservesHeaderTextInsideMultilineString(t *testing.T) {
	entry := codexHookTrustEntry{Key: "/x/hooks.json:stop:0:0", Hash: "sha256:new"}
	existing := "instructions = \"\"\"\n" +
		"[hooks.state.\"/x/hooks.json:stop:0:0\"]\n" +
		"trusted_hash = \"sha256:not-structure\"\n" +
		"\"\"\"\n"
	changed, out, err := planCodexHookTrust([]byte(existing), []codexHookTrustEntry{entry})
	require.NoError(t, err)
	require.True(t, changed)
	assert.Contains(t, string(out), existing, "multiline instruction text must remain byte-for-byte")
	assert.Equal(t, 2, strings.Count(string(out), `[hooks.state."/x/hooks.json:stop:0:0"]`),
		"one header-looking string line plus one real appended table")
}

func TestPlanCodexHookTrust_RefusesEscapedEquivalentKey(t *testing.T) {
	entry := codexHookTrustEntry{Key: "/x/hooks.json:stop:0:0", Hash: "sha256:new"}
	existing := `[hooks.state."/x/hooks\u002ejson:stop:0:0"]` + "\ntrusted_hash = \"sha256:old\"\n"
	changed, out, err := planCodexHookTrust([]byte(existing), []codexHookTrustEntry{entry})
	require.Error(t, err)
	assert.False(t, changed)
	assert.Nil(t, out)
}

func TestEnsureCodexHookTrustInFile_PreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0o600))
	entry := codexHookTrustEntry{Key: "/x/hooks.json:stop:0:0", Hash: "sha256:abc"}

	require.NoError(t, ensureCodexHookTrustInFile(path, []codexHookTrustEntry{entry}))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestEnsureCodexHookTrustInFile_PreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed.toml")
	link := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(target, []byte("model = \"gpt-5\"\n"), 0o600))
	require.NoError(t, os.Symlink(target, link))
	entry := codexHookTrustEntry{Key: "/x/hooks.json:stop:0:0", Hash: "sha256:abc"}

	require.NoError(t, ensureCodexHookTrustInFile(link, []codexHookTrustEntry{entry}))
	fi, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "config.toml must remain a symlink")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Contains(t, string(data), `trusted_hash = "sha256:abc"`)
}

func TestCodexHookTrustHasNoVersionGate(t *testing.T) {
	oldLookPath := codexLookPath
	codexLookPath = func(file string) (string, error) { return "/fixture/codex-any-version", nil }
	t.Cleanup(func() { codexLookPath = oldLookPath })
	ok, reason := (codexHookInstaller{}).AutoTrustSupported()
	assert.True(t, ok)
	assert.Empty(t, reason)
}

func TestCodexHookInstaller_RefusesToTrustNonPortableRelativeExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldCommand := codexHookCommandString
	t.Cleanup(func() {
		codexHookCommandString = oldCommand
	})
	codexHookCommandString = func() string { return "bin/tclaude session hook-callback" }

	err := (codexHookInstaller{}).InstallTrusted()
	require.ErrorContains(t, err, "non-portable relative executable")
	assert.NoFileExists(t, filepath.Join(home, ".codex", "hooks.json"))
	assert.NoFileExists(t, filepath.Join(home, ".codex", "config.toml"))
}

func TestCodexHookInstaller_RepairsMissingTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	inst := codexHookInstaller{}
	require.NoError(t, inst.InstallTrusted())

	configPath := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, len(codexHookEvents), strings.Count(string(data), "trusted_hash = "))
	installed, missing, needsRepair := inst.Check()
	assert.True(t, installed)
	assert.Empty(t, missing)
	assert.False(t, needsRepair)

	// Model an install from before TCL-3: hooks.json exists and is current,
	// but config.toml has no trust state. Check must make setup repair it.
	require.NoError(t, os.WriteFile(configPath, []byte("model = \"gpt-5\"\n"), 0o644))
	installed, missing, needsRepair = inst.Check()
	assert.True(t, installed)
	assert.Empty(t, missing)
	assert.False(t, needsRepair, "hook declarations remain current")
	assert.False(t, inst.Trusted())

	require.NoError(t, inst.TrustInstalled())
	assert.True(t, inst.Trusted())
}

func TestCodexHookInstaller_TrustKeyUsesFinalGroupPosition(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"user-tool"}]}]}}`,
	), 0o644))

	require.NoError(t, codexHookInstaller{}.InstallTrusted())
	config, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), filepath.Join(dir, "hooks.json")+`:session_start:1:0`,
		"the preserved user group occupies index 0; tclaude trust must target its appended group at index 1")
}

func TestCodexHookInstaller_PreflightsTrustBeforeChangingHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	hooksPath := filepath.Join(dir, "hooks.json")
	original := []byte(`{"description":"leave unchanged"}`)
	require.NoError(t, os.WriteFile(hooksPath, original, 0o644))
	// Valid TOML, but an inline hooks table cannot be extended by the standard
	// [hooks.state."..."] table without redefining hooks.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("hooks = { state = {} }\n"), 0o644))

	require.Error(t, codexHookInstaller{}.InstallTrusted())
	after, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	assert.Equal(t, original, after, "hooks.json must not change when trust preflight fails")
}

func TestCodexHookInstaller_RollsBackHooksWhenDiscoveryFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	hooksPath := filepath.Join(dir, "hooks.json")
	original := []byte(`{"description":"no startup gate"}`)
	require.NoError(t, os.WriteFile(hooksPath, original, 0o600))

	oldDiscovery := discoverCodexHookTrustEntries
	discoverCodexHookTrustEntries = func(path, want string) ([]codexHookTrustEntry, error) {
		return nil, fmt.Errorf("hooks/list unavailable")
	}
	t.Cleanup(func() { discoverCodexHookTrustEntries = oldDiscovery })

	require.ErrorContains(t, (codexHookInstaller{}).InstallTrusted(), "hooks/list unavailable")
	after, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	assert.Equal(t, original, after)
	info, err := os.Stat(hooksPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestCodexHookInstaller_RefusesModifiedManagedShapeBeforeTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	inst := codexHookInstaller{}
	require.NoError(t, inst.Install())

	path := inst.ConfigTarget()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), `"type": "command"`, `"type": "command", "async": true`, 1))
	require.NoError(t, os.WriteFile(path, data, 0o644))

	require.ErrorContains(t, inst.TrustInstalled(), "exact managed shape")
	assert.False(t, inst.Trusted())
	assert.NoFileExists(t, filepath.Join(home, ".codex", "config.toml"))
}

func TestCodexHookInstaller_RefusesStaleManagedEventBeforeTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTclaudeOnPath(t)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	inst := codexHookInstaller{}
	require.NoError(t, inst.Install())

	path := inst.ConfigTarget()
	hooks, top, err := readCodexHooks(path)
	require.NoError(t, err)
	hooks["SessionEnd"] = append(json.RawMessage(nil), hooks["SessionStart"]...)
	top["hooks"], err = json.Marshal(hooks)
	require.NoError(t, err)
	data, err := json.MarshalIndent(top, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	installed, _, needsRepair := inst.Check()
	assert.True(t, installed, "all currently required events are still present")
	assert.True(t, needsRepair, "the no-longer-required tclaude event must be removed")
	require.ErrorContains(t, inst.TrustInstalled(), "non-required Codex event SessionEnd")
	assert.False(t, inst.Trusted())
}

func TestCodexHooksRollbackRefusesConcurrentMetadataChanges(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hooks.json")
		require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
		snapshot, err := snapshotCodexHooksFile(path)
		require.NoError(t, err)
		require.NoError(t, atomicWritePreservingMode(path, []byte("installed"), 0o644))
		require.NoError(t, snapshot.validateInstalledState())
		require.NoError(t, os.Chmod(path, 0o640))
		require.ErrorContains(t, snapshot.restoreIfUnchanged([]byte("installed")), "mode changed")
	})

	t.Run("symlink target", func(t *testing.T) {
		dir := t.TempDir()
		first := filepath.Join(dir, "first.json")
		second := filepath.Join(dir, "second.json")
		link := filepath.Join(dir, "hooks.json")
		require.NoError(t, os.WriteFile(first, []byte("old"), 0o600))
		require.NoError(t, os.WriteFile(second, []byte("other"), 0o600))
		require.NoError(t, os.Symlink(first, link))
		snapshot, err := snapshotCodexHooksFile(link)
		require.NoError(t, err)
		require.NoError(t, atomicWritePreservingMode(link, []byte("installed"), 0o644))
		require.NoError(t, snapshot.validateInstalledState())
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(second, link))
		require.ErrorContains(t, snapshot.restoreIfUnchanged([]byte("installed")), "target changed")
	})
}
