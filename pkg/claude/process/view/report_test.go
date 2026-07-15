package view_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
)

func TestBuildViewerEnvelopeDoesNotLeakPersistedContent(t *testing.T) {
	t.Parallel()
	secret := "DO_NOT_LEAK_7d3d"
	validAgentID := "agt_0123456789abcdef0123456789abcdef"
	ref := "viewer@sha256:" + strings.Repeat("a", 64)
	tmpl := &model.Template{ID: "viewer", Name: secret, Start: "work", Params: map[string]model.Param{"token": {Default: secret}}, Nodes: map[string]model.Node{
		"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: secret, Run: "/private/" + secret, Args: []string{secret}}, Next: model.Next{"pass": "done"}},
		"done": {Type: model.NodeTypeEnd},
	}}
	st := state.New("run-view", ref, ref, []state.NodeInit{{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusRunning}, {ID: "done", Type: model.NodeTypeEnd}})
	node := st.Nodes["work"]
	node.ActiveAttempt = &state.AttemptState{Attempt: 1}
	st.Nodes["work"] = node
	st.OutstandingCommands = map[string]state.OutstandingCommand{
		"invalid-old": {ID: "invalid-old", NodeID: "work", Attempt: 1, Kind: state.CommandKindStartAttempt, Status: state.CommandStatusIssued, ExternalRef: "agent:agt_" + strings.Repeat(secret, 20), CreatedAt: time.Unix(0, 0)},
		"start":       {ID: "start", NodeID: "work", Attempt: 1, Kind: state.CommandKindStartAttempt, Status: state.CommandStatusIssued, ExternalRef: "agent:" + validAgentID, Payload: json.RawMessage(`{"secret":"` + secret + `"}`), Feedback: secret, CreatedAt: time.Unix(1, 0)},
	}
	st.Obligations = map[string]state.ObligationRecord{
		"tampered": {ID: "tampered", NodeID: "work", Kind: state.WaitKind(secret), Status: state.WaitStatus(secret)},
	}
	logs := []evidence.NodeLog{{NodeID: "work", Entries: []evidence.LogEntry{
		entry(1, "work", &state.Event{Type: state.EventNodeAttemptStarted, NodeID: "work", Attempt: 1, Actor: state.ActorRef("program:/private/" + secret + "@exit0"), Reason: secret, EvidenceRef: "/private/" + secret}),
		entry(2, "work", &state.Event{Type: state.EventCommandObserved, NodeID: "work", Actor: state.ActorRef("agent:agt_" + strings.Repeat(secret, 20))}),
	}}}
	verification := processverify.Report{RunID: "run-view", EffectiveStatus: state.RunStatusDirty, Dirty: true, Diagnostics: []processverify.Diagnostic{{Layer: processverify.LayerLoad, Severity: model.SeverityError, Code: "unsafe_" + secret, Path: secret, Message: secret}}}

	envelope := processview.Build(store.Snapshot{Run: store.RunRecord{ID: "run-view", TemplateRef: ref, Params: map[string]string{"token": secret}}, State: &st, NodeLogs: logs}, tmpl, verification)
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	assert.NotContains(t, string(data), secret)
	assert.NotContains(t, string(data), "/private/")
	assert.NotContains(t, string(data), "payload")
	assert.NotContains(t, string(data), "feedback")
	assert.Equal(t, "program", envelope.Report.Nodes["work"].Timeline[0].Actor.Kind)
	assert.Equal(t, validAgentID, envelope.Report.Nodes["work"].Conversation.AgentID)
	assert.Nil(t, envelope.Report.Nodes["work"].Timeline[1].Actor)
	assert.Nil(t, envelope.Report.Nodes["work"].Obligation)
	assert.Equal(t, "verification_error", envelope.Verification.Diagnostics[0].Code)
	assert.Empty(t, envelope.Verification.Diagnostics[0].Path)

	invalidStatus := st.Obligations["tampered"]
	invalidStatus.Kind = state.WaitKindAgent
	st.Obligations["tampered"] = invalidStatus
	envelope = processview.Build(store.Snapshot{Run: store.RunRecord{ID: "run-view", TemplateRef: ref}, State: &st}, tmpl, verification)
	assert.Nil(t, envelope.Report.Nodes["work"].Obligation, "invalid status must independently fail closed")
}

func TestBuildUsesPersistedAmbiguityStateOnly(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(100, 0)
	st := state.New("run", "ref", "ref", []state.NodeInit{{ID: "parent", Status: state.NodeStatusBlocked}, {ID: "older", Status: state.NodeStatusRunning}, {ID: "newer", Status: state.NodeStatusRunning}})
	parent := st.Nodes["parent"]
	parent.Children = []string{"older", "newer"}
	parent.BlockedAttempt = 1
	parent.BlockedOwner = "human:operator"
	st.Nodes["parent"] = parent
	older := st.Nodes["older"]
	older.ActiveAttempt = &state.AttemptState{Attempt: 9}
	st.Nodes["older"] = older
	newer := st.Nodes["newer"]
	newer.ActiveAttempt = &state.AttemptState{Attempt: 1}
	st.Nodes["newer"] = newer
	st.OutstandingCommands = map[string]state.OutstandingCommand{
		"old-child":     {ID: "old-child", NodeID: "older", Attempt: 9, Kind: state.CommandKindStartAttempt, Status: state.CommandStatusObserved, ExternalRef: "agent:agt_00000000000000000000000000000000", CreatedAt: t0},
		"new-child":     {ID: "new-child", NodeID: "newer", Attempt: 1, Kind: state.CommandKindStartAttempt, Status: state.CommandStatusIssued, ExternalRef: "agent:agt_11111111111111111111111111111111", CreatedAt: t0.Add(time.Minute)},
		"z-old-contact": {ID: "z-old-contact", NodeID: "parent", Attempt: 1, Kind: state.CommandKindBlockNode, Status: state.CommandStatusObserved, CreatedAt: t0},
		"a-new-contact": {ID: "a-new-contact", NodeID: "parent", Attempt: 1, Kind: state.CommandKindBlockNode, Status: state.CommandStatusIssued, CreatedAt: t0.Add(time.Minute)},
	}
	st.Contacts = map[string]state.ContactState{
		"z-old-contact": {CommandID: "z-old-contact", Budget: 9},
		"a-new-contact": {CommandID: "a-new-contact", Budget: 2},
	}
	tmpl := &model.Template{Nodes: map[string]model.Node{"parent": {Type: model.NodeTypeTask}, "older": {Type: model.NodeTypeTask}, "newer": {Type: model.NodeTypeTask}}}
	envelope := processview.Build(store.Snapshot{State: &st}, tmpl, processverify.Report{})
	assert.Equal(t, "agt_11111111111111111111111111111111", envelope.Report.Nodes["parent"].Conversation.AgentID)
	require.NotNil(t, envelope.Report.Nodes["parent"].Blocked.Contact)
	assert.Equal(t, 2, envelope.Report.Nodes["parent"].Blocked.Contact.BudgetMax)

	// The newest current command being unbound is authoritative; no historical fallback.
	cmd := st.OutstandingCommands["new-child"]
	cmd.ExternalRef = ""
	st.OutstandingCommands["new-child"] = cmd
	envelope = processview.Build(store.Snapshot{State: &st}, tmpl, processverify.Report{})
	assert.Nil(t, envelope.Report.Nodes["parent"].Conversation)

	// A resolved block is a tombstone, not a current blockage.
	parent.Status = state.NodeStatusReady
	st.Nodes["parent"] = parent
	envelope = processview.Build(store.Snapshot{State: &st}, tmpl, processverify.Report{})
	assert.Nil(t, envelope.Report.Nodes["parent"].Blocked)
}

func TestBuildTraversedEdgesMatchPlannerSemantics(t *testing.T) {
	t.Parallel()
	tmpl := &model.Template{Nodes: map[string]model.Node{
		"retry":     {Next: model.Next{"pass": "done", "failure": "failed"}},
		"ok":        {Next: model.Next{"done": "done"}},
		"lone":      {Next: model.Next{"custom": "done"}},
		"decision":  {Type: model.NodeTypeDecision, Next: model.Next{"approve": "done"}},
		"ambiguous": {Next: model.Next{"one": "done", "two": "failed"}},
		"done":      {Type: model.NodeTypeEnd}, "failed": {Type: model.NodeTypeEnd},
	}}
	logs := []evidence.NodeLog{
		{NodeID: "retry", Entries: []evidence.LogEntry{
			entryStatus(1, "retry", "fail", state.NodeStatusReady),
			entryStatus(2, "retry", "error", state.NodeStatusFailed),
		}},
		{NodeID: "ok", Entries: []evidence.LogEntry{entryStatus(3, "ok", "OK", state.NodeStatusCompleted)}},
		{NodeID: "lone", Entries: []evidence.LogEntry{entryStatus(4, "lone", "passed", state.NodeStatusCompleted)}},
		{NodeID: "decision", Entries: []evidence.LogEntry{entry(5, "decision", &state.Event{Type: state.EventDecisionRecorded, NodeID: "decision", ChosenEdge: " APPROVE "})}},
		{NodeID: "ambiguous", Entries: []evidence.LogEntry{entryStatus(6, "ambiguous", "pass", state.NodeStatusCompleted)}},
	}
	envelope := processview.Build(store.Snapshot{State: &state.State{Nodes: map[string]state.NodeState{}}, NodeLogs: logs}, tmpl, processverify.Report{})
	assert.Equal(t, []processview.TraversedEdge{
		{From: "decision", Outcome: "approve", To: "done", Count: 1},
		{From: "lone", Outcome: "custom", To: "done", Count: 1},
		{From: "ok", Outcome: "done", To: "done", Count: 1},
		{From: "retry", Outcome: "failure", To: "failed", Count: 1},
	}, envelope.Report.TraversedEdges)
}

func TestNewEnvelopeHasStableEmptyCollections(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(processview.NewEnvelope("run", processverify.Report{}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"run":{"id":"run","effectiveStatus":"inconsistent"},"graph":null,"verification":{"effectiveStatus":"inconsistent","dirty":false,"diagnostics":[]},"report":{"schemaVersion":1,"nodes":{},"traversedEdges":[]},"viewerV2":{"protocol":"viewer_v2","stateSchemaVersion":0,"routingAvailable":false,"routingUnavailableReason":"unsupported_schema"}}`, string(data))
}

func TestBuildPreservesAuthoritativelyValidLongIdentifiers(t *testing.T) {
	t.Parallel()
	longID := "node-" + strings.Repeat("a", 180)
	templateID := "template-" + strings.Repeat("b", 180)
	ref := templateID + "@sha256:" + strings.Repeat("c", 64)
	tmpl := &model.Template{ID: templateID, Start: longID, Nodes: map[string]model.Node{longID: {Type: model.NodeTypeEnd}}}
	st := state.New("run-long", ref, ref, []state.NodeInit{{ID: longID, Type: model.NodeTypeEnd, Status: state.NodeStatusCompleted}})
	envelope := processview.Build(store.Snapshot{Run: store.RunRecord{ID: "run-long", TemplateRef: ref}, State: &st}, tmpl, processverify.Report{EffectiveStatus: state.RunStatusCompleted})
	require.NotNil(t, envelope.Graph)
	assert.Equal(t, ref, envelope.Run.TemplateRef)
	assert.Equal(t, longID, envelope.Graph.Nodes[0].ID)
}

func entry(seq int64, nodeID string, event *state.Event) evidence.LogEntry {
	return evidence.LogEntry{SchemaVersion: evidence.LogEntrySchemaVersion, Seq: seq, Scope: evidence.Scope{Kind: evidence.ScopeNode, ID: nodeID}, Kind: evidence.EntryKindAttempt, Event: event}
}

func entryStatus(seq int64, nodeID, outcome string, status state.NodeStatus) evidence.LogEntry {
	return entry(seq, nodeID, &state.Event{Type: state.EventNodeAttemptSettled, NodeID: nodeID, Outcome: outcome, NodeStatus: status})
}
