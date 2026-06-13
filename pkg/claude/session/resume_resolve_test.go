package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestResolveResumeConv_Claude pins that the Claude Code harness still
// resolves a --resume id through the established cwd-indexed resolver: a
// conv seeded in its project dir resolves by prefix to its full id + cwd,
// and an unknown id errors.
func TestResolveResumeConv_Claude(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	cwd := "/home/u/proj"
	convID := "abcdef01-2345-6789-abcd-ef0123456789"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  convops.GetClaudeProjectPath(cwd),
		ProjectPath: cwd,
		Created:     "2026-01-01T00:00:00Z",
	}))

	full, proj, err := resolveResumeConv(harness.Default(), "abcdef01", false, cwd)
	require.NoError(t, err)
	assert.Equal(t, convID, full)
	assert.Equal(t, cwd, proj)

	_, _, err = resolveResumeConv(harness.Default(), "zzzzzzzz", false, cwd)
	require.Error(t, err, "an unknown conv must error")
}

// TestResolveResumeConv_CodexUsesConvStore pins that a non-Claude harness
// resolves through its ConvStore, not the CC ~/.claude/projects resolver:
// with no Codex storage in the temp HOME the lookup finds nothing and
// returns a clean error (never silently falling back to the CC resolver).
func TestResolveResumeConv_CodexUsesConvStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	h, err := harness.Resolve("codex")
	require.NoError(t, err)
	require.True(t, h.SupportsConvs())

	_, _, err = resolveResumeConv(h, "bogus", false, "/home/u/proj")
	require.Error(t, err, "an unresolvable codex conv must error via the ConvStore path")
}
