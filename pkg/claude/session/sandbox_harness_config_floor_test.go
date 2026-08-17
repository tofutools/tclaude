package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// claudeFloorFixture gives each case its own HOME so the floor resolves against
// a scratch state root rather than the developer's real ~/.claude.
func claudeFloorFixture(t *testing.T) (home string, cwd string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cwd = filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o700))
	return home, cwd
}

func buildClaudeFloorSpec(
	t *testing.T,
	cwd string,
	effective sandboxpolicy.EffectiveProfile,
) TclaudeLayerLaunchSpec {
	t.Helper()
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective = effective
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName,
		Cwd:         cwd,
		Snapshot:    &snapshot,
	})
	require.NoError(t, err)
	return spec
}

func TestHarnessConfigFloorAppliesByDefault(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{})

	settings := filepath.Join(home, ".claude", "settings.json")
	hooks := filepath.Join(home, ".claude", "hooks")
	assert.Contains(t, spec.Contract.HarnessConfigFloor, settings)
	assert.Contains(t, spec.Contract.HarnessConfigFloor, hooks)
	assert.Contains(t, spec.Contract.HarnessConfigFloorDirs, hooks)
	assert.NotContains(t, spec.Contract.HarnessConfigFloorDirs, settings,
		"the settings file must not be materialized as a directory")

	// The state root itself stays writable — the harness genuinely needs it —
	// and it is bound in phase 0, before the mount plan, so the floor's
	// read-only entries land on top of it rather than under it.
	assert.Equal(t, filepath.Join(home, ".claude"), spec.Contract.StateRoot)

	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))
	for _, path := range spec.Contract.HarnessConfigFloor {
		_, err := os.Lstat(path)
		require.NoErrorf(t, err, "floor path %q must be materialized", path)
		access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, path)
		require.Truef(t, covered, "floor path %q has no rendered rule", path)
		assert.Equalf(t, sandboxpolicy.AccessRead, access,
			"floor path %q must render read-only", path)
	}
}

// A missing surface is the one that matters most: without materialization the
// agent simply creates ~/.claude/hooks under the writable state root.
func TestHarnessConfigFloorMaterializesMissingSurfaces(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	hooks := filepath.Join(home, ".claude", "hooks")
	settings := filepath.Join(home, ".claude", "settings.json")
	require.NoFileExists(t, settings)

	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{})
	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))

	info, err := os.Stat(hooks)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	// NOT an empty file: every JSON reader in this repo treats a missing file
	// as "{}" but hands an existing empty one to json.Unmarshal, which fails.
	body, err := os.ReadFile(settings)
	require.NoError(t, err)
	assert.JSONEq(t, "{}", string(body))
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed),
		"a materialized settings file must parse for tclaude's own readers")
}

// Materialization must never clobber a real settings file.
func TestHarnessConfigFloorLeavesExistingContentAlone(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	require.NoError(t, os.WriteFile(settings, []byte(`{"sandbox":{"enabled":true}}`), 0o600))

	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{})
	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))

	body, err := os.ReadFile(settings)
	require.NoError(t, err)
	assert.Equal(t, `{"sandbox":{"enabled":true}}`, string(body))
}

func TestHarnessConfigFloorOptOut(t *testing.T) {
	_, cwd := claudeFloorFixture(t)
	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{
		HarnessConfig: sandboxpolicy.HarnessConfigAccessWrite,
	})
	assert.Empty(t, spec.Contract.HarnessConfigFloor,
		"an explicit write posture restores the pre-floor behavior")
}

// The per-path escape hatch: naming one surface reopens exactly that surface.
func TestHarnessConfigFloorExplicitWriteRowReopensOneEntry(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	plugins := filepath.Join(home, ".claude", "plugins")
	require.NoError(t, os.MkdirAll(plugins, 0o700))

	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: plugins, Access: sandboxpolicy.AccessWrite},
		},
	})
	assert.NotContains(t, spec.Contract.HarnessConfigFloor, plugins)
	assert.Contains(t, spec.Contract.HarnessConfigFloor,
		filepath.Join(home, ".claude", "hooks"),
		"reopening one surface must not disarm the rest")
	access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, plugins)
	require.True(t, covered)
	assert.Equal(t, sandboxpolicy.AccessWrite, access)
}

// A broad grant is the ordinary shape of an unrelated profile, so it must NOT
// read as the operator taking responsibility for the config surface.
func TestHarnessConfigFloorSurvivesBroadWriteGrant(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: filepath.Join(home, ".claude"), Access: sandboxpolicy.AccessWrite},
		},
	})
	assert.Contains(t, spec.Contract.HarnessConfigFloor,
		filepath.Join(home, ".claude", "settings.json"))
	assert.Contains(t, spec.Contract.HarnessConfigFloor,
		filepath.Join(home, ".claude", "hooks"))
}

// A deny is stricter than the floor; the floor must step aside rather than
// materialize a host path the operator denied.
func TestHarnessConfigFloorYieldsToProfileDeny(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	hooks := filepath.Join(home, ".claude", "hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o700))

	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: hooks, Access: sandboxpolicy.AccessDeny},
		},
	})
	assert.NotContains(t, spec.Contract.HarnessConfigFloor, hooks)
	access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, hooks)
	require.True(t, covered)
	assert.Equal(t, sandboxpolicy.AccessDeny, access)
}

func TestHarnessConfigFloorCatalogCoversEveryHarness(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	for _, name := range []string{"XDG_CONFIG_HOME", "CODEX_HOME"} {
		t.Setenv(name, "")
	}
	// OpenCode is deliberately empty: agentd's own state layout already binds
	// its config tree read-only, so a catalog here would be redundant and
	// would aim at the wrong root under private state.
	openCode, err := harnessConfigFloorCatalog(
		harness.OpenCodeName, filepath.Join(home, ".opencode"))
	require.NoError(t, err)
	assert.Empty(t, openCode)
	for _, tc := range []struct {
		harness string
		root    string
		want    string
	}{
		{harness.DefaultName, filepath.Join(home, ".claude"),
			filepath.Join(home, ".claude", "settings.json")},
		{harness.CodexName, filepath.Join(home, ".codex"),
			filepath.Join(home, ".codex", "config.toml")},
		{harness.CopilotName, filepath.Join(home, ".copilot"),
			filepath.Join(home, ".copilot", "config.json")},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			entries, err := harnessConfigFloorCatalog(tc.harness, tc.root)
			require.NoError(t, err)
			paths := make([]string, 0, len(entries))
			for _, entry := range entries {
				paths = append(paths, entry.Path)
			}
			assert.Contains(t, paths, tc.want)
		})
	}
	_, err = harnessConfigFloorCatalog("nonesuch", filepath.Join(home, ".claude"))
	assert.Error(t, err)
}

// Codex's managed profile is its harness-builtin confinement, so it has to be
// in the floor rather than merely adjacent to it.
func TestHarnessConfigFloorCoversCodexManagedProfile(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	entries, err := harnessConfigFloorCatalog(harness.CodexName, filepath.Join(home, ".codex"))
	require.NoError(t, err)
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	assert.Contains(t, paths,
		filepath.Join(home, ".codex", harness.CodexAgentProfile+".config.toml"))
}

// A symlinked entry cannot be floored faithfully: binding the resolved target
// leaves the NAME an ordinary symlink inside the writable state root, which
// the agent can unlink and replace with a real directory. Skipping is the
// disclosed outcome; silently flooring the target would be a false claim.
func TestHarnessConfigFloorSkipsSymlinkedEntry(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	target := filepath.Join(home, "dotfiles", "skills")
	link := filepath.Join(home, ".claude", "skills")
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.Symlink(target, link))

	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{})
	assert.NotContains(t, spec.Contract.HarnessConfigFloor, link)
	assert.NotContains(t, spec.Contract.HarnessConfigFloor, target,
		"flooring the resolved target would leave the swappable name unprotected")
	assert.Contains(t, spec.Contract.HarnessConfigFloor,
		filepath.Join(home, ".claude", "hooks"),
		"one unfloorable entry must not disarm the rest")
	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))
}

// Non-symlinked entries must keep their LITERAL name, so the bind becomes a
// mountpoint the agent cannot unlink from inside the sandbox.
func TestHarnessConfigFloorKeepsLiteralNames(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{})
	for _, path := range spec.Contract.HarnessConfigFloor {
		assert.Truef(t, sandboxpolicy.PathContainsOrEqual(
			filepath.Join(home, ".claude"), path),
			"floor path %q escaped the state root", path)
	}
}

// A symlink swapped in after the spec was frozen must refuse rather than bind
// through to whatever it now points at.
func TestHarnessConfigFloorRefusesSymlinkSwappedAfterFreeze(t *testing.T) {
	home, cwd := claudeFloorFixture(t)
	hooks := filepath.Join(home, ".claude", "hooks")
	spec := buildClaudeFloorSpec(t, cwd, sandboxpolicy.EffectiveProfile{})
	require.Contains(t, spec.Contract.HarnessConfigFloor, hooks)

	elsewhere := filepath.Join(home, "elsewhere")
	require.NoError(t, os.MkdirAll(elsewhere, 0o700))
	require.NoError(t, os.Symlink(elsewhere, hooks))

	err := PrepareTclaudeLayerHarnessState(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
}
