package testharness

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestWorldIsolatesInheritedCodexHome(t *testing.T) {
	outside := testutil.CanonicalTempDir(t)
	t.Setenv("CODEX_HOME", outside)
	world := New(t)
	cx := NewCodexSim(t, world.HomeDir, world.HomeDir)
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteThreadRow(CodexThreadSeed{Title: "before", Cwd: world.HomeDir}))
	h, ok := harness.Get(harness.CodexName)
	require.True(t, ok)
	require.NoError(t, h.Convs.SetTitle(cx.ConvID, "inside-world"))
	title, err := cx.ThreadTitle()
	require.NoError(t, err)
	require.Equal(t, "inside-world", title, "production native store must address the simulated thread")
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "inherited Codex root must remain untouched")
}
