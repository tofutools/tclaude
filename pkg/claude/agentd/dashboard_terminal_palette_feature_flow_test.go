package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestDashboardSnapshotDynamicallyGatesTerminalCommandPaletteShortcut(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	handler := agentd.BuildDashboardHandlerForTest()

	snap := fetchSnapshotOnly(t, handler)
	assert.False(t, snap.TerminalPaletteShortcut,
		"the harness clear-line chord stays terminal-owned by default")

	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{
		TerminalCommandPaletteShortcut: true,
	}}))
	snap = fetchSnapshotOnly(t, handler)
	assert.True(t, snap.TerminalPaletteShortcut,
		"an explicit opt-in reaches the live dashboard snapshot")
}
