package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkgraphInstance_InsertGetListRoundTrip(t *testing.T) {
	setupTestDB(t)

	id, err := InsertWorkgraphInstance(&WorkgraphInstance{
		TemplateRef:  "example:ci",
		TemplateName: "ci",
		Title:        "nightly ci",
		Mermaid:      "flowchart TD\n  a --> b",
		Params:       `{"branch":"main"}`,
		GroupID:      7,
	})
	require.NoError(t, err, "InsertWorkgraphInstance")
	require.Greater(t, id, int64(0), "expected positive id")

	got, err := GetWorkgraphInstance(id)
	require.NoError(t, err, "GetWorkgraphInstance")
	require.NotNil(t, got, "got nil row")
	assert.Equal(t, "example:ci", got.TemplateRef)
	assert.Equal(t, "ci", got.TemplateName)
	assert.Equal(t, "nightly ci", got.Title)
	assert.Equal(t, WorkgraphStatusRunning, got.Status, "status defaults to running")
	assert.Equal(t, "flowchart TD\n  a --> b", got.Mermaid)
	assert.Equal(t, `{"branch":"main"}`, got.Params)
	assert.Equal(t, "{}", got.Vars, "blank vars defaults to {}")
	assert.Equal(t, int64(7), got.GroupID)
	assert.False(t, got.CreatedAt.IsZero(), "created_at stamped on insert")
	assert.False(t, got.UpdatedAt.IsZero(), "updated_at stamped on insert")
	assert.True(t, got.CompletedAt.IsZero(), "completed_at zero before terminal")

	all, err := ListWorkgraphInstances()
	require.NoError(t, err, "ListWorkgraphInstances")
	require.Len(t, all, 1, "expected 1 instance")
	assert.Equal(t, id, all[0].ID)
}

func TestWorkgraphInstance_GetMissingReturnsNil(t *testing.T) {
	setupTestDB(t)
	got, err := GetWorkgraphInstance(9999)
	require.NoError(t, err, "missing id is not an error")
	assert.Nil(t, got, "expected nil for missing id")
}

func TestWorkgraphInstance_UpdateStatusStampsCompletedAt(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})

	// Terminal status stamps completed_at.
	n, err := UpdateWorkgraphInstanceStatus(id, WorkgraphStatusCompleted)
	require.NoError(t, err, "UpdateWorkgraphInstanceStatus")
	require.Equal(t, 1, n, "rows affected")
	got, _ := GetWorkgraphInstance(id)
	assert.Equal(t, WorkgraphStatusCompleted, got.Status)
	assert.False(t, got.CompletedAt.IsZero(), "terminal status stamps completed_at")

	// Going back to running clears completed_at.
	_, err = UpdateWorkgraphInstanceStatus(id, WorkgraphStatusRunning)
	require.NoError(t, err)
	got, _ = GetWorkgraphInstance(id)
	assert.Equal(t, WorkgraphStatusRunning, got.Status)
	assert.True(t, got.CompletedAt.IsZero(), "non-terminal status clears completed_at")

	// Missing id affects 0 rows.
	n, err = UpdateWorkgraphInstanceStatus(9999, WorkgraphStatusFailed)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "missing id affects 0 rows")
}

func TestWorkgraphInstance_UpdateVars(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})

	n, err := UpdateWorkgraphInstanceVars(id, `{"count":3}`)
	require.NoError(t, err, "UpdateWorkgraphInstanceVars")
	require.Equal(t, 1, n, "rows affected")
	got, _ := GetWorkgraphInstance(id)
	assert.Equal(t, `{"count":3}`, got.Vars)

	// Blank normalizes back to {}.
	_, err = UpdateWorkgraphInstanceVars(id, "")
	require.NoError(t, err)
	got, _ = GetWorkgraphInstance(id)
	assert.Equal(t, "{}", got.Vars, "blank vars stored as {}")
}

func TestWorkgraphInstance_DeleteCascadesNodesAndEvents(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})

	// Two nodes + two events hanging off the instance.
	_, err := InsertWorkgraphNode(&WorkgraphNode{InstanceID: id, NodeID: "a", Label: "build"})
	require.NoError(t, err, "insert node a")
	_, err = InsertWorkgraphNode(&WorkgraphNode{InstanceID: id, NodeID: "b", Label: "test"})
	require.NoError(t, err, "insert node b")
	_, err = AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: id, Kind: WorkgraphEventInstanceCreated})
	require.NoError(t, err, "append event 1")
	_, err = AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: id, NodeID: "a", Kind: WorkgraphEventNodeStarted})
	require.NoError(t, err, "append event 2")

	nodes, _ := ListWorkgraphNodes(id)
	require.Len(t, nodes, 2, "two nodes before delete")
	events, _ := ListWorkgraphEvents(id)
	require.Len(t, events, 2, "two events before delete")

	// Delete the instance — CASCADE must take nodes + events with it.
	require.NoError(t, DeleteWorkgraphInstance(id), "DeleteWorkgraphInstance")

	gone, _ := GetWorkgraphInstance(id)
	assert.Nil(t, gone, "instance gone")
	nodesAfter, _ := ListWorkgraphNodes(id)
	assert.Empty(t, nodesAfter, "nodes cascade-deleted with instance")
	eventsAfter, _ := ListWorkgraphEvents(id)
	assert.Empty(t, eventsAfter, "events cascade-deleted with instance")

	// Idempotent re-delete.
	assert.NoError(t, DeleteWorkgraphInstance(id), "re-delete is a no-op")
}

func TestWorkgraphNode_InsertGetListAndBulk(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})

	// Single insert with full field set round-trips.
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	nid, err := InsertWorkgraphNode(&WorkgraphNode{
		InstanceID:   inst,
		NodeID:       "build",
		Label:        "Build",
		ExecutorKind: "agent",
		Status:       WorkgraphNodeStatusRunning,
		Outcome:      "",
		Detail:       `{"prompt":"go build"}`,
		Output:       "ok",
		Assignee:     "conv-123",
		Visits:       2,
		StartedAt:    started,
	})
	require.NoError(t, err, "InsertWorkgraphNode")
	require.Greater(t, nid, int64(0))

	got, err := GetWorkgraphNode(inst, "build")
	require.NoError(t, err, "GetWorkgraphNode")
	require.NotNil(t, got)
	assert.Equal(t, "Build", got.Label)
	assert.Equal(t, "agent", got.ExecutorKind)
	assert.Equal(t, WorkgraphNodeStatusRunning, got.Status)
	assert.Equal(t, `{"prompt":"go build"}`, got.Detail)
	assert.Equal(t, "conv-123", got.Assignee)
	assert.Equal(t, int64(2), got.Visits)
	assert.True(t, got.StartedAt.Equal(started), "started_at round-trip: got %v want %v", got.StartedAt, started)
	assert.True(t, got.FinishedAt.IsZero(), "unset finished_at is zero")

	// Default status when blank.
	dn, _ := InsertWorkgraphNode(&WorkgraphNode{InstanceID: inst, NodeID: "deploy"})
	require.Greater(t, dn, int64(0))
	deploy, _ := GetWorkgraphNode(inst, "deploy")
	assert.Equal(t, WorkgraphNodeStatusPending, deploy.Status, "blank status defaults to pending")
	assert.Equal(t, "{}", deploy.Detail, "blank detail defaults to {}")

	list, err := ListWorkgraphNodes(inst)
	require.NoError(t, err, "ListWorkgraphNodes")
	require.Len(t, list, 2, "two nodes")
	assert.Equal(t, "build", list[0].NodeID, "ordered by id asc")
	assert.Equal(t, "deploy", list[1].NodeID)

	// Missing node → nil.
	none, err := GetWorkgraphNode(inst, "nope")
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestWorkgraphNode_BulkInsertTransactional(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})

	// InstanceID on the structs is left zero — InsertWorkgraphNodes must
	// override it with the passed instance id.
	err := InsertWorkgraphNodes(inst, []*WorkgraphNode{
		{NodeID: "n1", Label: "one"},
		{NodeID: "n2", Label: "two"},
		{NodeID: "n3", Label: "three"},
	})
	require.NoError(t, err, "InsertWorkgraphNodes")
	list, _ := ListWorkgraphNodes(inst)
	require.Len(t, list, 3, "all three inserted")
	assert.Equal(t, inst, list[0].InstanceID, "instance id overridden onto nodes")

	// Empty slice is a no-op, not an error.
	require.NoError(t, InsertWorkgraphNodes(inst, nil), "empty slice no-op")

	// A duplicate node_id mid-batch rolls the WHOLE batch back.
	err = InsertWorkgraphNodes(inst, []*WorkgraphNode{
		{NodeID: "n4", Label: "four"},
		{NodeID: "n1", Label: "dup"}, // collides with the existing n1
	})
	require.Error(t, err, "duplicate node_id should error")
	after, _ := ListWorkgraphNodes(inst)
	assert.Len(t, after, 3, "failed batch rolled back — n4 not persisted")
}

func TestWorkgraphNode_UniqueConstraint(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})
	_, err := InsertWorkgraphNode(&WorkgraphNode{InstanceID: inst, NodeID: "x"})
	require.NoError(t, err)
	_, err = InsertWorkgraphNode(&WorkgraphNode{InstanceID: inst, NodeID: "x"})
	require.Error(t, err, "(instance_id, node_id) must be unique")
}

func TestWorkgraphNode_UpdatePartialTouchesOnlySetFields(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})
	_, err := InsertWorkgraphNode(&WorkgraphNode{
		InstanceID:   inst,
		NodeID:       "build",
		Label:        "Build",
		ExecutorKind: "agent",
		Status:       WorkgraphNodeStatusPending,
		Detail:       `{"a":1}`,
		Assignee:     "conv-orig",
		Visits:       1,
	})
	require.NoError(t, err)

	before, _ := GetWorkgraphNode(inst, "build")
	// Force a measurable updated_at gap (RFC3339 is second-granularity).
	time.Sleep(1100 * time.Millisecond)

	// Patch only status + outcome + finished_at.
	newStatus := WorkgraphNodeStatusDone
	outcome := "success"
	finished := time.Now().UTC().Truncate(time.Second)
	n, err := UpdateWorkgraphNode(inst, "build", WorkgraphNodePatch{
		Status:     &newStatus,
		Outcome:    &outcome,
		FinishedAt: &finished,
	})
	require.NoError(t, err, "UpdateWorkgraphNode")
	require.Equal(t, 1, n, "rows affected")

	got, _ := GetWorkgraphNode(inst, "build")
	// Changed.
	assert.Equal(t, WorkgraphNodeStatusDone, got.Status)
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

func TestWorkgraphNode_UpdateClearsTimestampWithZeroTime(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})
	started := time.Now().UTC().Truncate(time.Second)
	_, _ = InsertWorkgraphNode(&WorkgraphNode{InstanceID: inst, NodeID: "x", StartedAt: started})

	// A zero-valued StartedAt pointer clears the stamp.
	var zero time.Time
	n, err := UpdateWorkgraphNode(inst, "x", WorkgraphNodePatch{StartedAt: &zero})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, _ := GetWorkgraphNode(inst, "x")
	assert.True(t, got.StartedAt.IsZero(), "zero-time pointer clears started_at")
}

func TestWorkgraphNode_UpdateEmptyPatchAndMissing(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})
	_, _ = InsertWorkgraphNode(&WorkgraphNode{InstanceID: inst, NodeID: "x"})

	// Empty patch is a 0-row no-op.
	n, err := UpdateWorkgraphNode(inst, "x", WorkgraphNodePatch{})
	require.NoError(t, err, "empty patch")
	assert.Equal(t, 0, n, "empty patch affects 0 rows")

	// Missing node affects 0 rows.
	label := "nope"
	n, err = UpdateWorkgraphNode(inst, "missing", WorkgraphNodePatch{Label: &label})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "missing node affects 0 rows")
}

func TestWorkgraphEvents_AppendListAndFilter(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})

	// Append a mix of instance-level and node-level events.
	id1, err := AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: inst, Kind: WorkgraphEventInstanceCreated})
	require.NoError(t, err, "append instance event")
	require.Greater(t, id1, int64(0))
	_, err = AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: inst, NodeID: "build", Kind: WorkgraphEventNodeStarted})
	require.NoError(t, err)
	_, err = AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: inst, NodeID: "build", Kind: WorkgraphEventNodeDone, Message: "exit 0"})
	require.NoError(t, err)
	_, err = AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: inst, NodeID: "test", Kind: WorkgraphEventNodeStarted})
	require.NoError(t, err)

	// Unfiltered: all four, oldest first, with at stamped.
	all, err := ListWorkgraphEvents(inst)
	require.NoError(t, err, "ListWorkgraphEvents")
	require.Len(t, all, 4, "all events")
	assert.Equal(t, WorkgraphEventInstanceCreated, all[0].Kind, "oldest first")
	assert.False(t, all[0].At.IsZero(), "at stamped server-side when zero")

	// Filtered by node id.
	buildEvents, err := ListWorkgraphEvents(inst, "build")
	require.NoError(t, err, "ListWorkgraphEvents filtered")
	require.Len(t, buildEvents, 2, "only build's events")
	assert.Equal(t, "exit 0", buildEvents[1].Message)
}

func TestWorkgraphEvents_AppendHonoursExplicitAt(t *testing.T) {
	setupTestDB(t)
	inst, _ := InsertWorkgraphInstance(&WorkgraphInstance{TemplateRef: "r", TemplateName: "n"})
	when := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	_, err := AppendWorkgraphEvent(&WorkgraphEvent{InstanceID: inst, Kind: "note", At: when})
	require.NoError(t, err)
	got, _ := ListWorkgraphEvents(inst)
	require.Len(t, got, 1)
	assert.True(t, got[0].At.Equal(when), "explicit at preserved: got %v want %v", got[0].At, when)
}
