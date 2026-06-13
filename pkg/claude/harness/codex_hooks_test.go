package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexHookInstaller_InstallAndCheck installs into a temp ~/.codex and
// verifies Check reports installed for every Codex event, and the on-disk
// hooks.json matches the verified Codex format: {"hooks": {<Event>:
// [{"hooks": [{"type":"command","command": ...}]}]}}.
func TestCodexHookInstaller_InstallAndCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inst := codexHookInstaller{}
	require.Equal(t, filepath.Join(home, ".codex", "hooks.json"), inst.ConfigTarget())

	installed, _, _ := inst.Check()
	require.False(t, installed, "fresh temp home has no hooks yet")

	require.NoError(t, inst.Install())

	installed, missing, needsRepair := inst.Check()
	assert.True(t, installed, "all events installed; missing=%v", missing)
	assert.Empty(t, missing)
	assert.False(t, needsRepair)

	// On-disk shape.
	data, err := os.ReadFile(inst.ConfigTarget())
	require.NoError(t, err)
	var file struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	for _, ev := range codexHookEvents {
		groups := file.Hooks[ev]
		require.NotEmpty(t, groups, "event %s present", ev)
		require.NotEmpty(t, groups[0].Hooks)
		assert.Equal(t, "command", groups[0].Hooks[0].Type)
		assert.Equal(t, codexHookCommandStr(), groups[0].Hooks[0].Command)
	}
}

// TestCodexHookInstaller_Idempotent installs twice and confirms no
// duplicate tclaude hooks accumulate.
func TestCodexHookInstaller_Idempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	inst := codexHookInstaller{}
	require.NoError(t, inst.Install())
	require.NoError(t, inst.Install())

	_, _, needsRepair := inst.Check()
	assert.False(t, needsRepair, "a second install must not create duplicates")

	data, err := os.ReadFile(inst.ConfigTarget())
	require.NoError(t, err)
	var file struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	var groups []codexMatcherGroup
	require.NoError(t, json.Unmarshal(file.Hooks["SessionStart"], &groups))
	count := 0
	for _, g := range groups {
		for _, h := range g.Hooks {
			if isOurCodexHook(h.Command) {
				count++
			}
		}
	}
	assert.Equal(t, 1, count, "exactly one tclaude hook per event after re-install")
}

// TestCodexHookInstaller_PreservesUserContent confirms install is surgical:
// a non-tclaude hook and an unrelated top-level key survive.
func TestCodexHookInstaller_PreservesUserContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	seed := `{
	  "someOtherKey": {"a": 1},
	  "hooks": {
	    "SessionStart": [{"hooks": [{"type": "command", "command": "/usr/bin/user-tool run"}]}]
	  }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(seed), 0o644))

	require.NoError(t, codexHookInstaller{}.Install())

	data, err := os.ReadFile(filepath.Join(dir, "hooks.json"))
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &top))
	assert.Contains(t, top, "someOtherKey", "unrelated top-level key preserved")

	var hooks map[string][]codexMatcherGroup
	require.NoError(t, json.Unmarshal(top["hooks"], &hooks))
	// SessionStart now has the user's hook AND tclaude's.
	var sawUser, sawOurs bool
	for _, g := range hooks["SessionStart"] {
		for _, h := range g.Hooks {
			if h.Command == "/usr/bin/user-tool run" {
				sawUser = true
			}
			if isOurCodexHook(h.Command) {
				sawOurs = true
			}
		}
	}
	assert.True(t, sawUser, "the user's non-tclaude hook is preserved")
	assert.True(t, sawOurs, "tclaude's hook was added alongside")
}

// TestCodexHookInstaller_RepairsStale seeds a stale (wrong-binary) tclaude
// hook and confirms Check flags repair and Install replaces it.
func TestCodexHookInstaller_RepairsStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	seed := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "/old/path/tclaude session hook-callback"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(seed), 0o644))

	_, _, needsRepair := codexHookInstaller{}.Check()
	assert.True(t, needsRepair, "a stale tclaude binary must flag repair")

	require.NoError(t, codexHookInstaller{}.Install())
	_, _, needsRepair = codexHookInstaller{}.Check()
	assert.False(t, needsRepair, "install repairs the stale hook")
}

// TestCodexHarness_HasHooks pins the descriptor wiring.
func TestCodexHarness_HasHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h, err := Resolve("codex")
	require.NoError(t, err)
	require.True(t, h.SupportsHooks(), "codex harness must expose a HookInstaller")
	assert.Equal(t, filepath.Join(home, ".codex", "hooks.json"), h.Hooks.ConfigTarget())
	assert.NotEmpty(t, h.Hooks.TrustNote(), "codex requires a trust step, so TrustNote is non-empty")
}
