package agentd

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	triggerlogic "github.com/tofutools/tclaude/pkg/claude/triggers"
)

func TestObserveTriggerAgentFactsAreExplicitAndCapabilityGated(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	agent := &db.Agent{AgentID: "agt_fact", CurrentConvID: "conv"}
	alive := map[string]struct{}{"tmux-live": {}}
	row := &db.SessionRow{TmuxSession: "tmux-live", ConvID: "conv", Harness: harness.DefaultName,
		Status: session.StatusIdle, LastHook: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-10 * time.Minute)}
	idle := observeTriggerAgentFact(db.TriggerSourceAgentIdle, agent, []*db.SessionRow{row}, alive, nil, nil, now)
	assert.Equal(t, triggerlogic.FactTrue, idle.result)
	assert.Equal(t, row.LastHook, idle.since)
	assert.Contains(t, idle.detail, "group messages")
	assert.Contains(t, idle.detail, "pane keystrokes")

	row.Status = session.StatusAwaitingPermission
	waiting := observeTriggerAgentFact(db.TriggerSourceAgentAwaitingInput, agent, []*db.SessionRow{row}, alive, nil, nil, now)
	assert.Equal(t, triggerlogic.FactFalse, waiting.result)
	assert.Contains(t, waiting.detail, "awaiting_permission is explicitly excluded")

	row.Harness = harness.CopilotName
	row.Status = session.StatusIdle
	unsupported := observeTriggerAgentFact(db.TriggerSourceAgentAwaitingInput, agent, []*db.SessionRow{row}, alive, nil, nil, now)
	assert.Equal(t, triggerlogic.FactUnknown, unsupported.result)
	assert.Contains(t, unsupported.detail, "does not expose")

	unavailable := observeTriggerAgentFact(db.TriggerSourceAgentIdle, agent, []*db.SessionRow{row}, alive,
		errors.New("sessions unavailable"), nil, now)
	assert.Equal(t, triggerlogic.FactUnknown, unavailable.result)
}

func TestObserveTriggerAgentAwaitingInputRequiresReadyCodexAppServer(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	agent := &db.Agent{AgentID: "agt_codex", CurrentConvID: "codex-conv"}
	alive := map[string]struct{}{"tmux-live": {}}
	row := &db.SessionRow{TmuxSession: "tmux-live", ConvID: "codex-conv", Harness: harness.CodexName,
		Status: session.StatusAwaitingInput, UpdatedAt: now}

	unsupported := observeTriggerAgentFact(db.TriggerSourceAgentAwaitingInput, agent, []*db.SessionRow{row}, alive, nil, nil, now)
	assert.Equal(t, triggerlogic.FactUnknown, unsupported.result)
	assert.Contains(t, unsupported.detail, "ready managed app-server")

	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{Generation: "gen", LaunchID: "launch",
		AgentID: agent.AgentID, ConvID: row.ConvID, SocketPath: "/tmp/codex-trigger.sock", State: db.CodexAppServerReady}))
	supported := observeTriggerAgentFact(db.TriggerSourceAgentAwaitingInput, agent, []*db.SessionRow{row}, alive, nil, nil, now)
	assert.Equal(t, triggerlogic.FactTrue, supported.result)
}

func TestTriggerDwellFiresOnceAcrossTicksAndRearmsOnlyAfterFalse(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}))
	agentID, _, err := db.EnsureAgentForConv("dwell-live-conv", "test")
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	row := &db.SessionRow{ID: "dwell-live-session", TmuxSession: "dwell-live-tmux",
		ConvID: "dwell-live-conv", Harness: harness.DefaultName, Status: session.StatusIdle,
		LastHook: now.Add(-2 * time.Minute)}
	require.NoError(t, db.SaveSession(row))
	old := liveTmuxCache
	liveTmuxCache = newTmuxSessionCache(0, time.Now, func() (map[string]struct{}, error) {
		return map[string]struct{}{"dwell-live-tmux": {}}, nil
	})
	t.Cleanup(func() { liveTmuxCache = old })
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "idle-once", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGlobal, Source: db.TriggerSourceAgentIdle, DraftFilter: db.TriggerDraftInclude,
		ForSeconds: 60, Actions: []db.TriggerAction{{Type: db.TriggerActionMessage,
			Message: &db.TriggerMessageAction{BodyTemplate: "idle {{agent.id}} {{event.fact_result}}"}}}})
	require.NoError(t, err)

	runTriggerTick(now)
	firings, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, firings, 1)
	assert.Equal(t, agentID, firings[0].AgentID)
	runTriggerTick(now.Add(time.Hour))
	firings, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Len(t, firings, 1, "a durable mature episode cannot refire after another tick/restart")

	row.Status = session.StatusWorking
	row.LastHook = now.Add(time.Hour + time.Second)
	require.NoError(t, db.SaveSession(row))
	runTriggerTick(now.Add(time.Hour + time.Second))
	row.Status = session.StatusIdle
	row.LastHook = now.Add(time.Hour + 2*time.Second)
	require.NoError(t, db.SaveSession(row))
	runTriggerTick(now.Add(time.Hour + 2*time.Minute + 2*time.Second))
	firings, err = db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Len(t, firings, 2, "an observed false condition re-arms the next true episode")
}
