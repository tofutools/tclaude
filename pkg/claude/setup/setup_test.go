package setup

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// tempHome points HOME (and USERPROFILE, which os.UserHomeDir reads on
// Windows) at a fresh temp dir so setup's writes — ~/.claude/settings.json,
// ~/.claude/skills/, ~/.tclaude/config.json — land in an isolated,
// throwaway tree instead of the developer's real home.
func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func assertSkillsInstalled(t *testing.T, home string) {
	t.Helper()
	assert.DirExists(t, filepath.Join(home, ".claude", "skills", "agent-coord"))
}

func assertNoSkills(t *testing.T, home string) {
	t.Helper()
	assert.NoDirExists(t, filepath.Join(home, ".claude", "skills"))
}

func assertBundledPermsGranted(t *testing.T) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent)
	for _, slug := range selfPermsForBundledSkills {
		assert.Contains(t, cfg.Agent.DefaultPermissions, slug)
	}
}

func assertBundledPermsNotGranted(t *testing.T) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Agent == nil {
		return
	}
	for _, slug := range selfPermsForBundledSkills {
		assert.NotContains(t, cfg.Agent.DefaultPermissions, slug)
	}
}

// With no --install-* flags, installExtras is a no-op: a baseline-only
// `tclaude setup` installs no skills and grants no default permissions.
func TestInstallExtras_NoFlags_NoOp(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{}))

	assertNoSkills(t, home)
	assertBundledPermsNotGranted(t)
}

// --install-agent-skills installs skills only — it does not grant
// default permissions.
func TestInstallExtras_SkillsOnly(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallAgentSkills: true}))

	assertSkillsInstalled(t, home)
	assertBundledPermsNotGranted(t)
}

// --install-default-agent-permissions grants permissions only — it does
// not install skills.
func TestInstallExtras_PermsOnly(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallDefaultAgentPerms: true}))

	assertNoSkills(t, home)
	assertBundledPermsGranted(t)
}

// --install-all installs every optional extra.
func TestInstallExtras_All(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallAll: true}))

	assertSkillsInstalled(t, home)
	assertBundledPermsGranted(t)
}

// --install-all must be equivalent to passing every individual
// --install-* flag.
func TestInstallExtras_AllEqualsBothFlags(t *testing.T) {
	t.Run("install-all", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{InstallAll: true}))
		assertSkillsInstalled(t, home)
		assertBundledPermsGranted(t)
	})
	t.Run("both-individual-flags", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{
			InstallAgentSkills:       true,
			InstallDefaultAgentPerms: true,
		}))
		assertSkillsInstalled(t, home)
		assertBundledPermsGranted(t)
	})
}

// Running installExtras twice must not duplicate granted permission
// slugs in the config.
func TestInstallExtras_Idempotent(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallAll: true}))
	require.NoError(t, installExtras(&Params{InstallAll: true}))

	assertSkillsInstalled(t, home)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent)
	for _, slug := range selfPermsForBundledSkills {
		count := 0
		for _, p := range cfg.Agent.DefaultPermissions {
			if p == slug {
				count++
			}
		}
		assert.Equalf(t, 1, count, "slug %s should appear exactly once, got %d", slug, count)
	}
}

// The baseline always runs, even when an --install-* flag is passed.
// Before this redesign, an --install-* flag triggered a "focused
// branch" that skipped the baseline entirely — so `tclaude setup
// --install-agent-skills` left a machine with skills but no hooks. This
// guards against that regression.
//
// runSetup is only side-effect-free to exercise end-to-end on native
// Linux: on macOS it may `brew install`, and on WSL it writes a Windows
// registry key. Elsewhere the test skips.
func TestRunSetup_BaselineRunsAlongsideExtras(t *testing.T) {
	if runtime.GOOS != "linux" || wsl.IsWSL() {
		t.Skip("runSetup is only safe to exercise end-to-end on native Linux")
	}
	if !isTmuxInstalled() {
		t.Skip("tmux missing: baseline halts at the prerequisite check")
	}
	home := tempHome(t)

	require.NoError(t, runSetup(&Params{Yes: true, InstallAgentSkills: true}))

	// Baseline ran despite the --install-* flag: hooks are installed.
	installed, missing, _ := session.CheckHooksInstalled()
	assert.True(t, installed, "baseline must install hooks alongside --install-agent-skills, missing: %v", missing)
	assert.FileExists(t, filepath.Join(home, ".claude", "settings.json"))
	// The requested extra ran too.
	assertSkillsInstalled(t, home)
}
