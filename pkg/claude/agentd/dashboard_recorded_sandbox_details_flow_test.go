package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestDashboardSnapshotDynamicallyGatesRecordedSandboxDetails(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	handler := agentd.BuildDashboardHandlerForTest()

	snap := fetchSnapshotOnly(t, handler)
	assert.False(t, snap.RecordedSandboxDetails,
		"the recorded-details arrow stays hidden by default")

	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{
		RecordedSandboxDetails: true,
	}}))
	snap = fetchSnapshotOnly(t, handler)
	assert.True(t, snap.RecordedSandboxDetails,
		"an explicit opt-in reaches the live dashboard snapshot")
}
