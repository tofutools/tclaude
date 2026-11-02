package agentd_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type dashCapacityQueue struct {
	CapacityQueue *struct {
		Capacity struct {
			MaxLiveAgents int    `json:"max_live_agents"`
			UsedAgents    *int   `json:"used_agents"`
			WhenFull      string `json:"when_full"`
			Queued        int    `json:"queued"`
		} `json:"capacity"`
		Entries []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Group    string `json:"group"`
			Position int    `json:"position"`
		} `json:"entries"`
	} `json:"capacity_queue"`
}

// Scenario: the dashboard's Spawn queue block. With capacity limits off the
// snapshot carries no capacity_queue; with a limit it carries the capacity
// line, and queued spawns in order without their request; the operator
// cancels one through the dashboard.
func TestDashboardSpawnQueueBlock(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	f := newFlow(t)
	f.HaveGroup("team")
	dash := agentd.BuildDashboardHandlerForTest()
	snapshot := func() (dashCapacityQueue, string) {
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var s dashCapacityQueue
		testharness.DecodeJSON(t, rec, &s)
		return s, rec.Body.String()
	}

	s, raw := snapshot()
	assert.Nil(t, s.CapacityQueue, "hidden while capacity limits are off")
	assert.NotContains(t, raw, `"capacity_queue"`)

	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	s, _ = snapshot()
	require.NotNil(t, s.CapacityQueue, "a limit shows the capacity line")
	assert.Equal(t, 1, s.CapacityQueue.Capacity.MaxLiveAgents)
	assert.Empty(t, s.CapacityQueue.Entries)

	f.AsHuman().Spawn("team", "first")
	queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{
		"name": "second", "environment": []map[string]string{{"name": "DASH_SECRET", "value": "hunter2"}},
	}))
	queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "third"}))

	s, raw = snapshot()
	require.NotNil(t, s.CapacityQueue)
	require.Len(t, s.CapacityQueue.Entries, 2)
	assert.Equal(t, "second", s.CapacityQueue.Entries[0].Name)
	assert.Equal(t, 1, s.CapacityQueue.Entries[0].Position)
	assert.Equal(t, 2, s.CapacityQueue.Capacity.Queued)
	require.NotNil(t, s.CapacityQueue.Capacity.UsedAgents)
	assert.Equal(t, 1, *s.CapacityQueue.Capacity.UsedAgents)
	assert.NotContains(t, raw, "hunter2", "the stored request never rides the snapshot")

	path := "/api/spawn-queue/" + strconv.FormatInt(s.CapacityQueue.Entries[0].ID, 10)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodDelete, path, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	s, _ = snapshot()
	require.Len(t, s.CapacityQueue.Entries, 1)
	assert.Equal(t, "third", s.CapacityQueue.Entries[0].Name)
	assert.Equal(t, 1, s.CapacityQueue.Entries[0].Position)
	assert.Equal(t, http.StatusConflict, testharness.Serve(dash, testharness.JSONRequest(t, http.MethodDelete, path, nil)).Code)
}
