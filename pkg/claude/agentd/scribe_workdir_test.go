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
	assert.Equal(t, harness.DefaultName, scribeSpawnHarness(bare), "bare group defaults to Claude")

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
	assert.Equal(t, harness.CodexName, scribeSpawnHarness(cx), "codex default profile → codex")
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
