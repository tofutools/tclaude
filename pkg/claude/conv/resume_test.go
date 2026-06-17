package conv

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-218 — `tclaude conv resume <id>` used to resolve only through CC's
// conv_index (clcommon.ResolveConvID), so a Codex conv id was never found and
// the resume path was unreachable for non-claude convs. resolveConvForResume
// fuses CC's rich resolver with every non-CC harness's ConvStore.Resolve, so a
// Codex id now resolves and carries its harness onward to resumeLaunchCmd.
//
// These reuse the conv-package test helpers from harness_entries_test.go
// (setupHarnessTestHome / writeClaudeConv / writeCodexRollout), which lay down
// real CC `.jsonl` and Codex rollout files under a throwaway HOME — no
// testharness import (which would be an import cycle from package conv).

// A Claude Code conv resolves through the conv_index path and is tagged with
// the default harness.
func TestResolveConvForResume_ClaudeHit(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	convID := "11111111-1111-1111-1111-111111111111"
	writeClaudeConv(t, cwd, convID, "hello from claude")
	// Populate conv_index from disk — clcommon.ResolveConvID reads the cache,
	// not the raw .jsonl.
	_, err := LoadSessionsIndex(GetClaudeProjectPath(cwd))
	require.NoError(t, err)

	rc, err := resolveConvForResume(convID[:8], false, cwd)
	require.NoError(t, err)
	require.NotNil(t, rc)

	assert.Equal(t, "claude", rc.Harness, "a CC conv is tagged with the default harness")
	assert.Equal(t, convID, rc.ConvID, "the short prefix resolves to the full conv id")
	assert.Equal(t, cwd, rc.ProjectPath, "resume targets the conv's real working dir")
	assert.Equal(t, "hello from claude", rc.DisplayName)
}

// A Codex conv — invisible to clcommon.ResolveConvID — resolves through the
// Codex ConvStore fallback and is tagged "codex".
func TestResolveConvForResume_CodexHit(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	convID := "22222222-2222-2222-2222-222222222222"
	writeCodexRollout(t, home, convID, cwd, "hello from codex")

	rc, err := resolveConvForResume(convID[:8], false, cwd)
	require.NoError(t, err)
	require.NotNil(t, rc, "a Codex conv must be found via the ConvStore fallback")

	assert.Equal(t, "codex", rc.Harness, "a Codex conv carries its own harness so resume uses `codex resume`")
	assert.Equal(t, convID, rc.ConvID)
	assert.Equal(t, cwd, rc.ProjectPath)
	assert.Equal(t, "hello from codex", rc.DisplayName, "DisplayName comes from ConvStore.Title")
}

// An ambiguous prefix in a non-CC store surfaces as an error, never collapsed
// into "not found" — ConvStore.Resolve's tri-state contract.
func TestResolveConvForResume_AmbiguousPrefixError(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	writeCodexRollout(t, home, "abcd1111-1111-1111-1111-111111111111", cwd, "one")
	writeCodexRollout(t, home, "abcd2222-2222-2222-2222-222222222222", cwd, "two")

	rc, err := resolveConvForResume("abcd", false, cwd)
	require.Error(t, err, "an ambiguous prefix must surface, not be swallowed as not-found")
	assert.Nil(t, rc)
	assert.Contains(t, err.Error(), "ambiguous")
}

// A prefix no harness recognises returns (nil, nil), which RunResume renders
// as the friendly "not found" message.
func TestResolveConvForResume_NotFound(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")

	rc, err := resolveConvForResume("nomatch", false, cwd)
	require.NoError(t, err, "nothing-anywhere is a clean miss, not an error")
	assert.Nil(t, rc)
}

// An exact Codex id resolves even when other ids share its prefix — the
// ConvStore exact-match-wins rule must survive the conv-level wrapper.
func TestResolveConvForResume_CodexExactIDBeatsAmbiguity(t *testing.T) {
	home := setupHarnessTestHome(t)
	cwd := filepath.Join(home, "proj")
	exact := "abcd1111-1111-1111-1111-111111111111"
	writeCodexRollout(t, home, exact, cwd, "one")
	writeCodexRollout(t, home, "abcd2222-2222-2222-2222-222222222222", cwd, "two")

	rc, err := resolveConvForResume(exact, false, cwd)
	require.NoError(t, err)
	require.NotNil(t, rc)
	assert.Equal(t, exact, rc.ConvID)
	assert.Equal(t, "codex", rc.Harness)
}
