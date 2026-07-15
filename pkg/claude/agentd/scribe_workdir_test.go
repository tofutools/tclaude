package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Internal (package agentd) coverage for the scribe-workdir resolution +
// trust-seeding wiring (JOH-369). The full HTTP summon is flow-tested in
// scribe_summon_flow_test.go (the CC path end-to-end); these pin the two
// unexported helpers the summon leans on — including the Codex branch, which
// the flow test's bare scribe group never reaches.

func TestEnsureScribeWorkdir_CreatesUnderTclaudeHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	cwd, fail := ensureScribeWorkdir()
	require.Nil(t, fail)
	assert.Equal(t, filepath.Join(dir, ".tclaude", "scribe"), cwd, "shared flat workdir under ~/.tclaude")
	fi, err := os.Stat(cwd)
	require.NoError(t, err, "the workdir was created")
	assert.True(t, fi.IsDir())
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), "workdir is private 0700")
}

func TestScribeSpawnHarness_DefaultsToClaude_CodexFromProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	// A bare scribe group (no default profile) → the default harness (Claude).
	bareID, err := db.CreateAgentGroup("bare-scribe", scribeGroupDescr)
	require.NoError(t, err)
	bare, err := db.GetAgentGroupByID(bareID)
	require.NoError(t, err)
	bareHarness, fail := scribeSpawnHarness(bare)
	require.Nil(t, fail)
	assert.Equal(t, harness.DefaultName, bareHarness, "bare group defaults to Claude")

	// A group whose default profile pins Codex → Codex (the summon follows the
	// group's default, so the dir is pre-trusted for the harness that reads it).
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "codex-prof", Harness: harness.CodexName})
	require.NoError(t, err)
	cxID, err := db.CreateAgentGroup("codex-scribe", scribeGroupDescr)
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("codex-scribe", "codex-prof")
	require.NoError(t, err)
	cx, err := db.GetAgentGroupByID(cxID)
	require.NoError(t, err)
	cxHarness, fail := scribeSpawnHarness(cx)
	require.Nil(t, fail)
	assert.Equal(t, harness.CodexName, cxHarness, "codex default profile → codex")
}

func TestScribeSpawnHarness_UsesGlobalDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "global-codex", Harness: harness.CodexName})
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardPref(dashboardDefaultProfilePrefKey, "global-codex"))
	groupID, err := db.CreateAgentGroup("bare-scribe", scribeGroupDescr)
	require.NoError(t, err)
	g, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	got, fail := scribeSpawnHarness(g)
	require.Nil(t, fail)
	assert.Equal(t, harness.CodexName, got,
		"trust seed harness must match the global profile executeSpawn adopts")
}

func TestScribeSpawnHarness_RejectsDisabledDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "paused", Harness: harness.CodexName, DisabledReason: "provider maintenance",
	})
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("paused-scribe", scribeGroupDescr)
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("paused-scribe", "paused")
	require.NoError(t, err)
	g, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)

	got, fail := scribeSpawnHarness(g)
	assert.Empty(t, got)
	require.NotNil(t, fail)
	assert.Equal(t, "profile_disabled", fail.Kind)
	assert.Contains(t, fail.Msg, "provider maintenance")
}

func TestSeedScribeDirTrust_SeedsThePerHarnessStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	scribeDir := filepath.Join(home, ".tclaude", "scribe")
	require.NoError(t, os.MkdirAll(scribeDir, 0o700))

	// Claude branch → ~/.claude.json gains the trust entry.
	seedScribeDirTrust(harness.DefaultName, scribeDir)
	claudeData, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	require.NoError(t, err, "the claude trust store was written")
	assert.Contains(t, string(claudeData), `"hasTrustDialogAccepted": true`)
	assert.Contains(t, string(claudeData), scribeDir)

	// Codex branch → ~/.codex/config.toml gains the [projects."<dir>"] table.
	seedScribeDirTrust(harness.CodexName, scribeDir)
	codexData, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	require.NoError(t, err, "the codex trust store was written")
	assert.Contains(t, string(codexData), `[projects."`+scribeDir+`"]`)
	assert.Contains(t, string(codexData), `trust_level = "trusted"`)
}

// An unrecognized harness (or one added later without a seeding path) must
// touch NEITHER harness's trust store — it logs and skips.
func TestSeedScribeDirTrust_UnknownHarnessSeedsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	scribeDir := filepath.Join(home, ".tclaude", "scribe")
	require.NoError(t, os.MkdirAll(scribeDir, 0o700))

	seedScribeDirTrust("some-future-harness", scribeDir)

	_, err := os.Stat(filepath.Join(home, ".claude.json"))
	assert.True(t, os.IsNotExist(err), "unknown harness must not create the claude trust store")
	_, err = os.Stat(filepath.Join(home, ".codex", "config.toml"))
	assert.True(t, os.IsNotExist(err), "unknown harness must not create the codex trust store")
}
