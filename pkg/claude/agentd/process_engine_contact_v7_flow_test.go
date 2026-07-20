package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func v7Contact(t *testing.T, fs *store.FS, runID string) (pathv1.ContactRecordV7, string) {
	t.Helper()
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	require.NoError(t, err)
	require.Len(t, aggregate.Contacts, 1, "run must carry exactly one contact")
	for id, record := range aggregate.Contacts {
		marker, ok := aggregate.SideEffects[id]
		require.True(t, ok)
		return record, marker.State
	}
	panic("unreachable")
}

// TestProcessEngineSchema7ExplicitContactMigratesNudgesEscalatesAndSurvivesRestart
// is the schema-7 counterpart of the legacy nudge-budget flow test: a pristine
// v6 run with an EXPLICIT contact schedule migrates (the relaxed eligibility
// gate), nudges on cadence, escalates exactly once, persists used budget
// across a host restart, and resets on observed recovery.
func TestProcessEngineSchema7ExplicitContactMigratesNudgesEscalatesAndSurvivesRestart(t *testing.T) {
	t.Skip("automatic v6-to-v7 migration was removed by the schema-8 S2 release")
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "nudge-v7-run", programTemplate("nudge-v7", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 1, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:nudges-v7", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	require.NoError(t, host.EnableExclusiveV7())
	host.Now = func() time.Time { return now }

	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	schema, err := fs.RunStateSchemaVersion(t.Context(), "nudge-v7-run")
	require.NoError(t, err)
	require.Equal(t, pathv1.CheckpointStateSchemaVersion, schema,
		"explicit-contact template must migrate now that contact parity exists")
	record, markerState := v7Contact(t, fs, "nudge-v7-run")
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Equal(t, pathv1.ContactProvenanceDispatch, record.Provenance)
	assert.Equal(t, "1s", record.Cadence)
	assert.Equal(t, uint64(1), record.Budget)
	assert.Equal(t, "human:oncall", record.EscalationTarget)
	assert.Equal(t, "agent:agt_fake", record.Assignee)

	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 0, adapter.escalations)

	// Restart: a fresh host over the same store must see the used budget and
	// escalate exactly once from durable state.
	restarted := processengine.New(fs, "agentd:nudges-v7-restart", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	require.NoError(t, restarted.EnableExclusiveV7())
	restarted.Now = func() time.Time { return now }
	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 1, adapter.escalations)
	record, markerState = v7Contact(t, fs, "nudge-v7-run")
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Equal(t, uint64(1), record.Used)
	assert.NotEmpty(t, record.EscalatedAt)
	assert.Empty(t, record.NextContactAt, "escalation clears the nudge schedule")

	// Repeat ticks stay silent after escalation.
	now = now.Add(time.Minute)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 1, adapter.escalations)

	// Observed recovery resets budget and escalation from the shared core.
	adapter.activity = processexec.Activity{Recovered: true, At: now.Add(time.Second)}
	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	record, _ = v7Contact(t, fs, "nudge-v7-run")
	assert.Zero(t, record.Used)
	assert.Empty(t, record.EscalatedAt)
	assert.NotEmpty(t, record.LastRecoveredAt)
}

// TestProcessEngineSchema7NilContactGetsEngineDefaults closes the original
// TCL-529 gap: a migrated run whose performer declares NO contact still
// receives the engine default schedule instead of silently losing nudges.
func TestProcessEngineSchema7NilContactGetsEngineDefaults(t *testing.T) {
	t.Skip("automatic v6-to-v7 migration was removed by the schema-8 S2 release")
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "default-contact-v7-run", programTemplate("default-contact-v7", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
	}), false)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:default-contact-v7", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	require.NoError(t, host.EnableExclusiveV7())
	host.Now = func() time.Time { return now }

	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	schema, err := fs.RunStateSchemaVersion(t.Context(), "default-contact-v7-run")
	require.NoError(t, err)
	require.Equal(t, pathv1.CheckpointStateSchemaVersion, schema)
	record, markerState := v7Contact(t, fs, "default-contact-v7-run")
	assert.Equal(t, pathv1.ContactStateScheduled, markerState)
	assert.Equal(t, processexec.DefaultAgentContactCadence.String(), record.Cadence)
	assert.Equal(t, uint64(processexec.DefaultAgentContactBudget), record.Budget)
	assert.Equal(t, "human:operator", record.EscalationTarget)

	now = now.Add(processexec.DefaultAgentContactCadence + time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges, "default schedule must nudge a stalled v7 performer")
}

// TestProcessEngineSchema7OverBoundContactFieldsStayOnV6 is the P1
// regression: templates whose eventual durable contact fields exceed the
// schema-7 bounds must not migrate — no v7 claim, no v7 dispatch, no wedge —
// and legacy v6 servicing must continue to own them.
func TestProcessEngineSchema7OverBoundContactFieldsStayOnV6(t *testing.T) {
	longTarget := "human:" + strings.Repeat("x", 300)
	longAssignee := strings.Repeat("y", 300)
	cases := []struct {
		name      string
		performer model.Performer
	}{
		{name: "explicit escalation target over bound", performer: model.Performer{
			Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
			Contact: &model.ContactSchedule{Cadence: "1s", Budget: 1, EscalationTarget: longTarget},
		}},
		{name: "derived human assignee over bound", performer: model.Performer{
			Kind: model.PerformerHuman, Assignee: longAssignee, Ask: "review",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, root := processEngineFlow(t)
			adapter := &deferredContactAdapter{}
			runID := "overbound-" + strings.ReplaceAll(tc.name, " ", "-")
			fs := createEngineRun(t, root, runID, programTemplate("overbound", tc.performer), false)
			now := time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC)
			host := processengine.New(fs, "agentd:overbound", map[model.PerformerKind]processexec.Adapter{
				model.PerformerAgent: adapter, model.PerformerHuman: adapter,
			})
			require.NoError(t, host.EnableExclusiveV7())
			host.Now = func() time.Time { return now }
			results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Empty(t, results[0].Error, "over-bound contact fields must not wedge the run")
			schema, err := fs.RunStateSchemaVersion(t.Context(), runID)
			require.NoError(t, err)
			assert.Equal(t, state.StateSchemaVersion, schema, "run must remain on legacy schema 6")
			snapshot, err := fs.LoadRun(t.Context(), runID)
			require.NoError(t, err)
			require.Len(t, snapshot.State.Contacts, 1, "legacy servicing must keep contact authority")
		})
	}
}

// TestProcessEngineSchema7ContactSurfacesWorklistAndViewer proves the read
// surfaces: the daemon worklist carries the nudge schedule for a schema-7
// item, and the viewer projects contact state through the provenance funnel
// (no raw escalation target strings).
func TestProcessEngineSchema7ContactSurfacesWorklistAndViewer(t *testing.T) {
	t.Skip("automatic v6-to-v7 migration was removed by the schema-8 S2 release")
	f, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "surface-contact-v7-run", programTemplate("surface-contact-v7", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:surface-contact-v7", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	require.NoError(t, host.EnableExclusiveV7())
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	record, _ := v7Contact(t, fs, "surface-contact-v7-run")

	list := processEngineGet(t, f, "/v1/process/worklist")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, list, &listing)
	require.Len(t, listing.Items, 1)
	item := listing.Items[0]
	assert.Equal(t, state.WaitStatusPending, item.Status)
	require.NotNil(t, item.Nudge, "schema-7 worklist items must carry the nudge schedule")
	assert.Equal(t, 5, item.Nudge.BudgetMax)
	assert.Equal(t, 0, item.Nudge.BudgetUsed)
	assert.Equal(t, "human:oncall", item.Nudge.EscalationTarget)
	assert.False(t, item.Nudge.NextContactAt.IsZero())
	assert.Equal(t, item.Nudge.NextContactAt, item.DueAt, "due mirrors the next nudge instant")

	view := processEngineGet(t, f, "/v1/process/runs/surface-contact-v7-run/view")
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	body := view.Body.String()
	assert.Contains(t, body, `"contacts"`)
	assert.Contains(t, body, `"budgetMax":5`)
	assert.Contains(t, body, `"state":"scheduled"`)
	// The viewer funnels escalation/assignee provenance: kinds only for
	// humans, stable agent ids for agents — never the raw target string.
	assert.NotContains(t, body, "oncall")
	assert.Contains(t, body, `"escalationTarget":{"kind":"human"}`)
	_ = record
}
