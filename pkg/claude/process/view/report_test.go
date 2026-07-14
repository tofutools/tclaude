package view_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
)

func TestBuildProjectsPersistedRunDataWithoutRawCommandContent(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	tmpl := &model.Template{
		ID: "viewer", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Next: model.Next{"pass": "check"}},
			"check":  {Type: model.NodeTypeTask, Next: model.Next{"fail": "failed", "pass": "done"}},
			"done":   {Type: model.NodeTypeEnd},
			"failed": {Type: model.NodeTypeEnd},
		},
	}
	st := state.New("run-view", "viewer@sha256:pinned", "viewer@sha256:pinned", []state.NodeInit{
		{ID: "parent", Type: model.NodeTypeTask, Status: state.NodeStatusBlocked},
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusCompleted},
		{ID: "check", Type: model.NodeTypeTask, Status: state.NodeStatusBlocked},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	parent := st.Nodes["parent"]
	parent.Children = []string{"work", "check"}
	st.Nodes["parent"] = parent
	check := st.Nodes["check"]
	check.Attempt = 2
	check.BlockedReason = "tests failed"
	check.BlockedOwner = "human:operator"
	check.BlockedAttempt = 2
	check.BlockedAt = t0.Add(8 * time.Minute)
	st.Nodes["check"] = check

	st.OutstandingCommands = map[string]state.OutstandingCommand{
		"cmd_work": {
			ID: "cmd_work", NodeID: "work", Attempt: 1, Kind: state.CommandKindStartAttempt,
			Status: state.CommandStatusObserved, ExternalRef: "agt_live123", CreatedAt: t0,
		},
		"cmd_check": {
			ID: "cmd_check", NodeID: "check", Attempt: 2, Kind: state.CommandKindStartAttempt,
			Status: state.CommandStatusIssued, ExternalRef: "agent:agt_retry456", CreatedAt: t0.Add(6 * time.Minute),
		},
		"cmd_block": {
			ID: "cmd_block", NodeID: "check", Attempt: 2, Kind: state.CommandKindBlockNode,
			Status: state.CommandStatusObserved, CreatedAt: t0.Add(8 * time.Minute),
		},
	}
	st.Obligations = map[string]state.ObligationRecord{
		"old": {
			ID: "old", NodeID: "work", Attempt: 1, CommandID: "cmd_old", Kind: state.WaitKindAgent,
			Assignee: "agent:agt_old", Status: state.WaitStatusSatisfied, CreatedAt: t0.Add(-time.Hour),
		},
		"active": {
			ID: "active", NodeID: "work", Attempt: 1, CommandID: "cmd_work", Kind: state.WaitKindAgent,
			Assignee: "agent:agt_live123", Status: state.WaitStatusPending, CreatedAt: t0,
			DueAt: t0.Add(5 * time.Minute), Summary: "Agent work for work",
			AvailableActions: []string{"pass", "fail"},
		},
	}
	st.Contacts = map[string]state.ContactState{
		"cmd_work": {
			CommandID: "cmd_work", Kind: state.WaitKindAgent, Assignee: "agent:agt_live123",
			Cadence: "5m0s", Budget: 3, Used: 1, LastContactedAt: t0.Add(2 * time.Minute),
			NextContactAt: t0.Add(7 * time.Minute), EscalationTarget: "human:operator",
		},
		"cmd_block": {
			CommandID: "cmd_block", Kind: state.WaitKindHuman, Assignee: "human:operator",
			Cadence: "30m0s", Budget: 5, Used: 2, LastContactedAt: t0.Add(9 * time.Minute),
			NextContactAt: t0.Add(39 * time.Minute), EscalationTarget: "human:operator",
			Paused: true, PauseReason: "operator is responding",
		},
	}

	secret := "SECRET RAW COMMAND PROMPT"
	logs := []evidence.NodeLog{
		{NodeID: "check", Entries: []evidence.LogEntry{
			entry(8, t0.Add(8*time.Minute), "check", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptSettled, NodeID: "check", Attempt: 2, Outcome: "fail", EvidenceRef: "artifact:failure"}),
			entry(6, t0.Add(6*time.Minute), "check", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptStarted, NodeID: "check", Attempt: 2, Actor: "agent:agt_retry456"}),
			entry(4, t0.Add(4*time.Minute), "check", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptStarted, NodeID: "check", Attempt: 1, Actor: "agent:agt_first"}),
			entry(5, t0.Add(5*time.Minute), "check", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptSettled, NodeID: "check", Attempt: 1, Outcome: "retry"}),
		}},
		{NodeID: "work", Entries: []evidence.LogEntry{
			entry(3, t0.Add(3*time.Minute), "work", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptSettled, NodeID: "work", Attempt: 1, Outcome: "pass", EvidenceRef: "artifact:work"}),
			entry(2, t0.Add(2*time.Minute), "work", evidence.EntryKindGate, &state.Event{
				Type: state.EventCommandIssued, NodeID: "work", Attempt: 1,
				Command: &state.OutstandingCommand{Payload: json.RawMessage(`{"prompt":"` + secret + `"}`), Feedback: secret},
				Reason:  secret, Feedback: secret,
			}),
			entry(1, t0.Add(time.Minute), "work", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptStarted, NodeID: "work", Attempt: 1, Actor: "agent:agt_live123"}),
			entry(9, t0.Add(9*time.Minute), "other", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptSettled, NodeID: "other", Outcome: "pass"}),
		}},
	}

	report := processview.Build(store.Snapshot{State: &st, NodeLogs: logs}, tmpl)
	assert.Equal(t, processview.SchemaVersion, report.SchemaVersion)
	require.Len(t, report.Nodes["work"].Timeline, 3)
	assert.Equal(t, []int64{1, 2, 3}, timelineSeqs(report.Nodes["work"].Timeline))
	assert.Equal(t, "agt_live123", report.Nodes["work"].Conversation.AgentID)
	require.NotNil(t, report.Nodes["work"].Obligation)
	assert.Equal(t, "active", report.Nodes["work"].Obligation.ID)
	assert.Equal(t, 1, report.Nodes["work"].Obligation.Contact.BudgetUsed)

	assert.Equal(t, processview.NodeSummary{
		AttemptCount: 3, RetryCount: 1, FailureCount: 1, CompletedStages: 1, TotalStages: 2,
	}, report.Nodes["parent"].Summary)
	require.NotNil(t, report.Nodes["check"].Blocked)
	assert.Equal(t, "operator is responding", report.Nodes["check"].Blocked.Contact.PauseReason)
	assert.Equal(t, "agt_retry456", report.Nodes["check"].Conversation.AgentID)

	require.Equal(t, []processview.TraversedEdge{
		{From: "check", Outcome: "fail", To: "failed", Count: 1, LastAt: t0.Add(8 * time.Minute)},
		{From: "work", Outcome: "pass", To: "check", Count: 1, LastAt: t0.Add(3 * time.Minute)},
	}, report.TraversedEdges)
	timelineJSON, err := json.Marshal(report.Nodes["work"].Timeline)
	require.NoError(t, err)
	assert.NotContains(t, string(timelineJSON), secret)
	assert.NotContains(t, string(timelineJSON), "payload")
	assert.NotContains(t, string(timelineJSON), "feedback")
}

func TestBuildPreservesLegacyBlockedTimestampAbsenceAndOmitsInferredEdges(t *testing.T) {
	t.Parallel()
	st := state.New("legacy", "legacy@sha256:pinned", "legacy@sha256:pinned", []state.NodeInit{{ID: "work", Status: state.NodeStatusBlocked}})
	node := st.Nodes["work"]
	node.BlockedReason = "legacy block"
	node.BlockedOwner = "human:operator"
	node.BlockedAtUnavailable = true
	st.Nodes["work"] = node
	tmpl := &model.Template{Start: "work", Nodes: map[string]model.Node{
		"work": {Type: model.NodeTypeTask, Next: model.Next{"next": "done"}},
		"done": {Type: model.NodeTypeEnd},
	}}
	logs := []evidence.NodeLog{{NodeID: "work", Entries: []evidence.LogEntry{
		entry(1, time.Time{}, "work", evidence.EntryKindAttempt, &state.Event{Type: state.EventNodeAttemptSettled, NodeID: "work", Outcome: "pass"}),
	}}}
	report := processview.Build(store.Snapshot{State: &st, NodeLogs: logs}, tmpl)
	require.NotNil(t, report.Nodes["work"].Blocked)
	assert.True(t, report.Nodes["work"].Blocked.BlockedAtUnavailable)
	assert.True(t, report.Nodes["work"].Blocked.BlockedAt.IsZero())
	assert.Empty(t, report.TraversedEdges, "pass must not be guessed to mean an authored next edge")
}

func TestNewReportHasStableEmptyCollections(t *testing.T) {
	t.Parallel()
	report := processview.Build(store.Snapshot{}, nil)
	data, err := json.Marshal(report)
	require.NoError(t, err)
	assert.JSONEq(t, `{"schemaVersion":1,"nodes":{},"traversedEdges":[]}`, string(data))
}

func entry(seq int64, at time.Time, scopeID string, kind evidence.EntryKind, event *state.Event) evidence.LogEntry {
	return evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion, Seq: seq, At: at,
		Scope: evidence.Scope{Kind: evidence.ScopeNode, ID: scopeID}, Kind: kind, Event: event,
	}
}

func timelineSeqs(entries []processview.TimelineEntry) []int64 {
	seqs := make([]int64, len(entries))
	for i := range entries {
		seqs[i] = entries[i].Seq
	}
	return seqs
}
