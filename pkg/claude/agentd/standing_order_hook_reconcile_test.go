package agentd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/hookevents"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestStandingOrderGroupLifecycleReconcilesNativeHookDeclarations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	groupID, err := db.CreateAgentGroup("native-hook-lifecycle", "")
	require.NoError(t, err)
	id, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:         "native-config-reminder",
		TargetKind:   db.StandingTargetGroup,
		GroupID:      groupID,
		Summary:      "Re-read the project configuration.",
		TriggerEvent: db.StandingTriggerHookEvent,
		HookSelectors: []hookevents.Selector{{
			Harness: hookevents.HarnessClaude, Event: "ConfigChange",
		}},
		Timing:  db.StandingTimingNextTurn,
		Cadence: db.StandingCadenceAlways,
		Enabled: true,
	})
	require.NoError(t, err)
	order, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	require.Empty(t, reconcileStandingOrderHookDeclarations(nil, order))
	assert.True(t, claudeHookFileHasEvent(t, "ConfigChange"))

	harnesses := standingOrderHookHarnessesForGroupBestEffort(groupID)
	disabled, err := db.DisableGroupTargetStandingOrdersForRetire(groupID)
	require.NoError(t, err)
	require.Equal(t, 1, disabled)
	require.Empty(t, reconcileStandingOrderHookHarnesses(harnesses))
	assert.False(t, claudeHookFileHasEvent(t, "ConfigChange"),
		"automatic disable prunes the optional callback")

	harnesses = standingOrderHookHarnessesForGroupBestEffort(groupID)
	reenabled, err := db.ReenableGroupRetiredStandingOrders(groupID)
	require.NoError(t, err)
	require.Equal(t, 1, reenabled)
	require.Empty(t, reconcileStandingOrderHookHarnesses(harnesses))
	assert.True(t, claudeHookFileHasEvent(t, "ConfigChange"),
		"automatic re-enable restores the optional callback")

	harnesses = standingOrderHookHarnessesForGroupBestEffort(groupID)
	require.NoError(t, db.DeleteAgentGroup("native-hook-lifecycle"))
	require.Empty(t, reconcileStandingOrderHookHarnesses(harnesses))
	assert.False(t, claudeHookFileHasEvent(t, "ConfigChange"),
		"group deletion prunes callbacks for swept orders")
}

func claudeHookFileHasEvent(t *testing.T, event string) bool {
	t.Helper()
	data, err := os.ReadFile(session.ClaudeSettingsPath())
	require.NoError(t, err)
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &settings))
	_, ok := settings.Hooks[event]
	return ok
}
