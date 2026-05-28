package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowInstance_InsertGetListRoundTrip(t *testing.T) {
	setupTestDB(t)

	id, err := InsertWorkflowInstance(&WorkflowInstance{
		TemplateRef:  "example:ci",
		TemplateName: "ci",
		Title:        "nightly ci",
		Mermaid:      "flowchart TD\n  a --> b",
		Params:       `{"branch":"main"}`,
		GroupID:      7,
	})
	require.NoError(t, err, "InsertWorkflowInstance")
	require.Greater(t, id, int64(0), "expected positive id")

	got, err := GetWorkflowInstance(id)
	require.NoError(t, err, "GetWorkflowInstance")
	require.NotNil(t, got, "got nil row")
	assert.Equal(t, "example:ci", got.TemplateRef)
	assert.Equal(t, "ci", got.TemplateName)
	assert.Equal(t, "nightly ci", got.Title)
	assert.Equal(t, WorkflowStatusRunning, got.Status, "status defaults to running")
	assert.Equal(t, "flowchart TD\n  a --> b", got.Mermaid)
	assert.Equal(t, `{"branch":"main"}`, got.Params)
	assert.Equal(t, "{}", got.Vars, "blank vars defaults to {}")
	assert.Equal(t, int64(7), got.GroupID)
	assert.False(t, got.CreatedAt.IsZero(), "created_at stamped on insert")
	assert.False(t, got.UpdatedAt.IsZero(), "updated_at stamped on insert")
	assert.True(t, got.CompletedAt.IsZero(), "completed_at zero before terminal")

	all, err := ListWorkflowInstances()
	require.NoError(t, err, "ListWorkflowInstances")
	require.Len(t, all, 1, "expected 1 instance")
	assert.Equal(t, id, all[0].ID)
}

func TestWorkflowInstance_GetMissingReturnsNil(t *testing.T) {
	setupTestDB(t)
	got, err := GetWorkflowInstance(9999)
	require.NoError(t, err, "missing id is not an error")
	assert.Nil(t, got, "expected nil for missing id")
}

func TestWorkflowInstance_UpdateStatusStampsCompletedAt(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})

	// Terminal status stamps completed_at.
	n, err := UpdateWorkflowInstanceStatus(id, WorkflowStatusCompleted)
	require.NoError(t, err, "UpdateWorkflowInstanceStatus")
	require.Equal(t, 1, n, "rows affected")
	got, _ := GetWorkflowInstance(id)
	assert.Equal(t, WorkflowStatusCompleted, got.Status)
	assert.False(t, got.CompletedAt.IsZero(), "terminal status stamps completed_at")

	// Going back to running clears completed_at.
	_, err = UpdateWorkflowInstanceStatus(id, WorkflowStatusRunning)
	require.NoError(t, err)
	got, _ = GetWorkflowInstance(id)
	assert.Equal(t, WorkflowStatusRunning, got.Status)
	assert.True(t, got.CompletedAt.IsZero(), "non-terminal status clears completed_at")

	// Missing id affects 0 rows.
	n, err = UpdateWorkflowInstanceStatus(9999, WorkflowStatusFailed)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "missing id affects 0 rows")
}

func TestWorkflowInstance_UpdateVars(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})

	n, err := UpdateWorkflowInstanceVars(id, `{"count":3}`)
	require.NoError(t, err, "UpdateWorkflowInstanceVars")
	require.Equal(t, 1, n, "rows affected")
	got, _ := GetWorkflowInstance(id)
	assert.Equal(t, `{"count":3}`, got.Vars)

	// Blank normalizes back to {}.
	_, err = UpdateWorkflowInstanceVars(id, "")
	require.NoError(t, err)
	got, _ = GetWorkflowInstance(id)
	assert.Equal(t, "{}", got.Vars, "blank vars stored as {}")
}

func TestWorkflowInstance_DeleteCascadesNodesAndEvents(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})

	// Two nodes + two events hanging off the instance.
	_, err := InsertWorkflowNode(&WorkflowNode{InstanceID: id, NodeID: "a", Label: "build"})
	require.NoError(t, err, "insert node a")
	_, err = InsertWorkflowNode(&WorkflowNode{InstanceID: id, NodeID: "b", Label: "test"})
	require.NoError(t, err, "insert node b")
	_, err = AppendWorkflowEvent(&WorkflowEvent{InstanceID: id, Kind: WorkflowEventInstanceCreated})
	require.NoError(t, err, "append event 1")
	_, err = AppendWorkflowEvent(&WorkflowEvent{InstanceID: id, NodeID: "a", Kind: WorkflowEventNodeStarted})
	require.NoError(t, err, "append event 2")

	nodes, _ := ListWorkflowNodes(id)
	require.Len(t, nodes, 2, "two nodes before delete")
	events, _ := ListWorkflowEvents(id)
	require.Len(t, events, 2, "two events before delete")

	// Delete the instance — CASCADE must take nodes + events with it.
	require.NoError(t, DeleteWorkflowInstance(id), "DeleteWorkflowInstance")

	gone, _ := GetWorkflowInstance(id)
	assert.Nil(t, gone, "instance gone")
	nodesAfter, _ := ListWorkflowNodes(id)
	assert.Empty(t, nodesAfter, "nodes cascade-deleted with instance")
	eventsAfter, _ := ListWorkflowEvents(id)
	assert.Empty(t, eventsAfter, "events cascade-deleted with instance")

	// Idempotent re-delete.
	assert.NoError(t, DeleteWorkflowInstance(id), "re-delete is a no-op")
}

func TestWorkflowNode_InsertGetListAndBulk(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})

	// Single insert with full field set round-trips.
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	nid, err := InsertWorkflowNode(&WorkflowNode{
		InstanceID:   inst,
		NodeID:       "build",
		Label:        "Build",
		ExecutorKind: "agent",
		Status:       WorkflowNodeStatusRunning,
		Outcome:      "",
		Detail:       `{"prompt":"go build"}`,
		Output:       "ok",
		Assignee:     "conv-123",
		Visits:       2,
		StartedAt:    started,
	})
	require.NoError(t, err, "InsertWorkflowNode")
	require.Greater(t, nid, int64(0))

	got, err := GetWorkflowNode(inst, "build")
	require.NoError(t, err, "GetWorkflowNode")
	require.NotNil(t, got)
	assert.Equal(t, "Build", got.Label)
	assert.Equal(t, "agent", got.ExecutorKind)
	assert.Equal(t, WorkflowNodeStatusRunning, got.Status)
	assert.Equal(t, `{"prompt":"go build"}`, got.Detail)
	assert.Equal(t, "conv-123", got.Assignee)
	assert.Equal(t, int64(2), got.Visits)
	assert.True(t, got.StartedAt.Equal(started), "started_at round-trip: got %v want %v", got.StartedAt, started)
	assert.True(t, got.FinishedAt.IsZero(), "unset finished_at is zero")

	// Default status when blank.
	dn, _ := InsertWorkflowNode(&WorkflowNode{InstanceID: inst, NodeID: "deploy"})
	require.Greater(t, dn, int64(0))
	deploy, _ := GetWorkflowNode(inst, "deploy")
	assert.Equal(t, WorkflowNodeStatusPending, deploy.Status, "blank status defaults to pending")
	assert.Equal(t, "{}", deploy.Detail, "blank detail defaults to {}")

	list, err := ListWorkflowNodes(inst)
	require.NoError(t, err, "ListWorkflowNodes")
	require.Len(t, list, 2, "two nodes")
	assert.Equal(t, "build", list[0].NodeID, "ordered by id asc")
	assert.Equal(t, "deploy", list[1].NodeID)

	// Missing node → nil.
	none, err := GetWorkflowNode(inst, "nope")
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestWorkflowNode_BulkInsertTransactional(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})

	// InstanceID on the structs is left zero — InsertWorkflowNodes must
	// override it with the passed instance id.
	err := InsertWorkflowNodes(inst, []*WorkflowNode{
		{NodeID: "n1", Label: "one"},
		{NodeID: "n2", Label: "two"},
		{NodeID: "n3", Label: "three"},
	})
	require.NoError(t, err, "InsertWorkflowNodes")
	list, _ := ListWorkflowNodes(inst)
	require.Len(t, list, 3, "all three inserted")
	assert.Equal(t, inst, list[0].InstanceID, "instance id overridden onto nodes")

	// Empty slice is a no-op, not an error.
	require.NoError(t, InsertWorkflowNodes(inst, nil), "empty slice no-op")

	// A duplicate node_id mid-batch rolls the WHOLE batch back.
	err = InsertWorkflowNodes(inst, []*WorkflowNode{
		{NodeID: "n4", Label: "four"},
		{NodeID: "n1", Label: "dup"}, // collides with the existing n1
	})
	require.Error(t, err, "duplicate node_id should error")
	after, _ := ListWorkflowNodes(inst)
	assert.Len(t, after, 3, "failed batch rolled back — n4 not persisted")
}

func TestWorkflowNode_UniqueConstraint(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})
	_, err := InsertWorkflowNode(&WorkflowNode{InstanceID: inst, NodeID: "x"})
	require.NoError(t, err)
	_, err = InsertWorkflowNode(&WorkflowNode{InstanceID: inst, NodeID: "x"})
	require.Error(t, err, "(instance_id, node_id) must be unique")
}

func TestWorkflowNode_UpdatePartialTouchesOnlySetFields(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})
	_, err := InsertWorkflowNode(&WorkflowNode{
		InstanceID:   inst,
		NodeID:       "build",
		Label:        "Build",
		ExecutorKind: "agent",
		Status:       WorkflowNodeStatusPending,
		Detail:       `{"a":1}`,
		Assignee:     "conv-orig",
		Visits:       1,
	})
	require.NoError(t, err)

	before, _ := GetWorkflowNode(inst, "build")
	// Force a measurable updated_at gap (RFC3339 is second-granularity).
	time.Sleep(1100 * time.Millisecond)

	// Patch only status + outcome + finished_at.
	newStatus := WorkflowNodeStatusDone
	outcome := "success"
	finished := time.Now().UTC().Truncate(time.Second)
	n, err := UpdateWorkflowNode(inst, "build", WorkflowNodePatch{
		Status:     &newStatus,
		Outcome:    &outcome,
		FinishedAt: &finished,
	})
	require.NoError(t, err, "UpdateWorkflowNode")
	require.Equal(t, 1, n, "rows affected")

	got, _ := GetWorkflowNode(inst, "build")
	// Changed.
	assert.Equal(t, WorkflowNodeStatusDone, got.Status)
	assert.Equal(t, "success", got.Outcome)
	assert.True(t, got.FinishedAt.Equal(finished), "finished_at set")
	assert.True(t, got.UpdatedAt.After(before.UpdatedAt), "updated_at bumped")
	// Untouched.
	assert.Equal(t, "Build", got.Label, "label untouched")
	assert.Equal(t, "agent", got.ExecutorKind, "executor_kind untouched")
	assert.Equal(t, `{"a":1}`, got.Detail, "detail untouched")
	assert.Equal(t, "conv-orig", got.Assignee, "assignee untouched")
	assert.Equal(t, int64(1), got.Visits, "visits untouched")
	assert.True(t, got.StartedAt.IsZero(), "started_at untouched (still unset)")
}

func TestWorkflowNode_UpdateClearsTimestampWithZeroTime(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})
	started := time.Now().UTC().Truncate(time.Second)
	_, _ = InsertWorkflowNode(&WorkflowNode{InstanceID: inst, NodeID: "x", StartedAt: started})

	// A zero-valued StartedAt pointer clears the stamp.
	var zero time.Time
	n, err := UpdateWorkflowNode(inst, "x", WorkflowNodePatch{StartedAt: &zero})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, _ := GetWorkflowNode(inst, "x")
	assert.True(t, got.StartedAt.IsZero(), "zero-time pointer clears started_at")
}

func TestWorkflowNode_UpdateEmptyPatchAndMissing(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})
	_, _ = InsertWorkflowNode(&WorkflowNode{InstanceID: inst, NodeID: "x"})

	// Empty patch is a 0-row no-op.
	n, err := UpdateWorkflowNode(inst, "x", WorkflowNodePatch{})
	require.NoError(t, err, "empty patch")
	assert.Equal(t, 0, n, "empty patch affects 0 rows")

	// Missing node affects 0 rows.
	label := "nope"
	n, err = UpdateWorkflowNode(inst, "missing", WorkflowNodePatch{Label: &label})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "missing node affects 0 rows")
}

func TestWorkflowEvents_AppendListAndFilter(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})

	// Append a mix of instance-level and node-level events.
	id1, err := AppendWorkflowEvent(&WorkflowEvent{InstanceID: inst, Kind: WorkflowEventInstanceCreated})
	require.NoError(t, err, "append instance event")
	require.Greater(t, id1, int64(0))
	_, err = AppendWorkflowEvent(&WorkflowEvent{InstanceID: inst, NodeID: "build", Kind: WorkflowEventNodeStarted})
	require.NoError(t, err)
	_, err = AppendWorkflowEvent(&WorkflowEvent{InstanceID: inst, NodeID: "build", Kind: WorkflowEventNodeDone, Message: "exit 0"})
	require.NoError(t, err)
	_, err = AppendWorkflowEvent(&WorkflowEvent{InstanceID: inst, NodeID: "test", Kind: WorkflowEventNodeStarted})
	require.NoError(t, err)

	// Unfiltered: all four, oldest first, with at stamped.
	all, err := ListWorkflowEvents(inst)
	require.NoError(t, err, "ListWorkflowEvents")
	require.Len(t, all, 4, "all events")
	assert.Equal(t, WorkflowEventInstanceCreated, all[0].Kind, "oldest first")
	assert.False(t, all[0].At.IsZero(), "at stamped server-side when zero")

	// Filtered by node id.
	buildEvents, err := ListWorkflowEvents(inst, "build")
	require.NoError(t, err, "ListWorkflowEvents filtered")
	require.Len(t, buildEvents, 2, "only build's events")
	assert.Equal(t, "exit 0", buildEvents[1].Message)
}

func TestWorkflowEvents_AppendHonoursExplicitAt(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkflowInstance(&WorkflowInstance{TemplateRef: "r", TemplateName: "n"})
	when := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	_, err := AppendWorkflowEvent(&WorkflowEvent{InstanceID: inst, Kind: "note", At: when})
	require.NoError(t, err)
	got, _ := ListWorkflowEvents(inst)
	require.Len(t, got, 1)
	assert.True(t, got[0].At.Equal(when), "explicit at preserved: got %v want %v", got[0].At, when)
}
