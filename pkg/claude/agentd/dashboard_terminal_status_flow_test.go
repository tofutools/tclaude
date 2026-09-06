package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardTerminalStatus_ReturnsCompactLiveAgentProjection(t *testing.T) {
	f := newFlow(t)

	const convID = "term-aaaa-bbbb-cccc-dddddddddddd"
	f.HaveConvWithTitle(convID, "solo-worker")
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	f.HaveAliveSession(convID, "spwn-term", "tmux-term", f.TestCwd("term"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-term", TmuxSession: "tmux-term", ConvID: convID,
		Cwd: f.TestCwd("term"), Status: "awaiting_input",
		StatusDetail: "needs a decision", LastHook: time.Now(),
	}))

	req := testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+agentID+"/status", nil)
	req.Header.Set("Origin", "http://127.0.0.1")
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var got struct {
		AgentID string `json:"agent_id"`
		ConvID  string `json:"conv_id"`
		Online  bool   `json:"online"`
		State   struct {
			Status       string `json:"status"`
			StatusDetail string `json:"status_detail"`
		} `json:"state"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, agentID, got.AgentID)
	assert.Equal(t, convID, got.ConvID)
	assert.True(t, got.Online)
	assert.Equal(t, "awaiting_input", got.State.Status)
	assert.Equal(t, "needs a decision", got.State.StatusDetail)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fields))
	assert.ElementsMatch(t, []string{"agent_id", "conv_id", "online", "state"}, mapKeys(fields),
		"standalone status must not inherit full snapshot fields")
}

func TestDashboardAndTerminalStatusShareReadOnlyBackgroundObservation(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBgShellReconcileCacheForTest)
	stubLiveBgShellCommands(t, "npm run dev", "gh pr checks --watch")

	f := newFlow(t)
	const (
		convID = "term-bg-1111-2222-333333333333"
		label  = "spwn-term-bg"
	)
	f.HaveGroup("squad")
	f.HaveConvWithTitle(convID, "background-terminal")
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	f.HaveAliveSession(convID, label, "tmux-term-bg", f.TestCwd("term-bg"))
	f.HaveMember("squad", convID)

	row, err := db.LoadSession(label)
	require.NoError(t, err)
	now := time.Now()
	row.Status = "main_agent_idle"
	row.StatusDetail = "stale background detail"
	row.BgShellsJSON = db.BgShellSet(nil).
		Add("shell-live", "npm run dev", now).
		Add("shell-dead", "finished shell", now).
		Encode()
	row.MonitorsJSON = db.MonitorSet(nil).
		Add("monitor-live", "gh pr checks --watch", "checks", false, now, time.Time{}).
		Encode()
	require.NoError(t, db.SaveSession(row))
	before, err := db.LoadSession(label)
	require.NoError(t, err)

	snapshot := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	member := findDashMember(snapshot, "squad", convID)
	require.NotNil(t, member)
	assert.Equal(t, 1, member.State.BgShellCount)
	assert.Equal(t, 1, member.State.MonitorCount)

	req := testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+agentID+"/status", nil)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var terminal struct {
		State struct {
			BgShellCount int `json:"bg_shell_count"`
			MonitorCount int `json:"monitor_count"`
		} `json:"state"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &terminal))
	assert.Equal(t, member.State.BgShellCount, terminal.State.BgShellCount)
	assert.Equal(t, member.State.MonitorCount, terminal.State.MonitorCount)

	after, err := db.LoadSession(label)
	require.NoError(t, err)
	assert.Equal(t, before.BgShellsJSON, after.BgShellsJSON,
		"snapshot and terminal GETs must not repair the shell ledger")
	assert.Equal(t, before.MonitorsJSON, after.MonitorsJSON,
		"snapshot and terminal GETs must not repair the monitor ledger")
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.StatusDetail, after.StatusDetail)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
