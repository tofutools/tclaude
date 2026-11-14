package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV337Backlog walks a database to v336, fills the board, and
// migrates it: the rows, their dependencies and history survive the
// rebuild, a parked stage is accepted, the triggers still fire, and a
// second run changes nothing.
func TestMigrateV337Backlog(t *testing.T) {
	path := t.TempDir() + "/v336.sqlite"
	d, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, createSchema(d))
	for _, step := range migrationSteps {
		if step.version > 336 {
			break
		}
		if step.version > 1 {
			require.NoError(t, step.apply(d), "v%d", step.version)
		}
	}
	_, err = d.Exec(`
		INSERT INTO agent_groups (id, name, created_at) VALUES (1, 'crew', 1);
		INSERT INTO group_task_columns (group_id, slug, label, position, created_at) VALUES (1, 'review', 'In review', 0, 1);
		INSERT INTO group_tasks (id, group_id, title, stage, status, rank, created_by, created_at, updated_at, column_slug, queued_for, queued_at)
		VALUES
			('tk_a', 1, 'A', 'next', 'open', 'a', 'human:operator', 1, 1, 'review', '', NULL),
			('tk_b', 1, 'B', 'idea', 'open', 'b', 'human:operator', 1, 1, '', '', NULL),
			('tk_c', 1, 'C', 'next', 'open', 'c', 'human:operator', 1, 1, '', 'review', 5);
		INSERT INTO group_task_deps (task_id, depends_on) VALUES ('tk_a', 'tk_b');
		INSERT INTO group_task_events (task_id, at, actor, kind) VALUES ('tk_a', 1, 'human:operator', 'created');
	`)
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_tasks SET stage = 'backlog' WHERE id = 'tk_b'`)
	require.Error(t, err, "v336 refuses the backlog stage")

	require.NoError(t, migrateV336toV337(d))
	require.NoError(t, migrateV336toV337(d), "a second run changes nothing")

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM group_tasks`).Scan(&n))
	assert.Equal(t, 3, n)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM group_task_deps`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM group_task_events`).Scan(&n))
	assert.Equal(t, 1, n)

	// Parked: out of its column and out of the queue.
	_, err = d.Exec(`UPDATE group_tasks SET stage = 'backlog' WHERE id IN ('tk_a', 'tk_c')`)
	require.NoError(t, err)
	var column, queued string
	require.NoError(t, d.QueryRow(`SELECT column_slug FROM group_tasks WHERE id = 'tk_a'`).Scan(&column))
	assert.Empty(t, column)
	require.NoError(t, d.QueryRow(`SELECT queued_for FROM group_tasks WHERE id = 'tk_c'`).Scan(&queued))
	assert.Empty(t, queued)
	// Never started.
	_, err = d.Exec(`UPDATE group_tasks SET started_at = 9 WHERE id = 'tk_a'`)
	require.NoError(t, err)
	var started sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT started_at FROM group_tasks WHERE id = 'tk_a'`).Scan(&started))
	assert.False(t, started.Valid)
	// A trigger of another table still reaches the rebuilt table.
	_, err = d.Exec(`UPDATE group_tasks SET stage = 'next', column_slug = 'review' WHERE id = 'tk_a'`)
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM group_task_columns WHERE slug = 'review'`)
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT column_slug FROM group_tasks WHERE id = 'tk_a'`).Scan(&column))
	assert.Empty(t, column)
}

func TestGroupTaskBacklogStage(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	worker, _, err := EnsureAgentForConv("conv-worker-1", "test")
	require.NoError(t, err)
	_, err = AddGroupTaskColumn(gid, "review", "In review", "", "t")
	require.NoError(t, err)

	idea, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "idea", Actor: "t"})
	require.NoError(t, err)
	parked, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "parked", Stage: GroupTaskStageBacklog, Actor: "t"})
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageBacklog, parked.Stage)
	queued, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "queued", Stage: GroupTaskStageNext, Actor: "t"})
	require.NoError(t, err)

	// The backlog lists last.
	tasks, err := ListGroupTasks(gid, GroupTaskListOpts{})
	require.NoError(t, err)
	require.Len(t, tasks, 3)
	assert.Equal(t, []string{queued.ID, idea.ID, parked.ID}, []string{tasks[0].ID, tasks[1].ID, tasks[2].ID})
	only, err := ListGroupTasks(gid, GroupTaskListOpts{Stage: GroupTaskStageBacklog})
	require.NoError(t, err)
	require.Len(t, only, 1)

	// Parking a started task in a column holds it, out of its column; the
	// history says so. Back to the ideas is a promotion.
	started, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "started", AssigneeID: worker, AssignedBy: "t", Column: "review", Actor: "t"})
	require.NoError(t, err)
	require.True(t, started.Started())
	got, err := SetGroupTaskStage(started.ID, GroupTaskStageBacklog, "t")
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageBacklog, got.Stage)
	assert.Equal(t, worker, got.AssigneeID, "a parked task keeps its assignee")
	assert.False(t, got.Started())
	assert.Empty(t, got.Column)
	events, err := ListGroupTaskEvents(started.ID)
	require.NoError(t, err)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	assert.Contains(t, kinds, GroupTaskEventDemoted)
	assert.Contains(t, kinds, GroupTaskEventHeld)
	got, err = SetGroupTaskStage(started.ID, GroupTaskStageIdea, "t")
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageIdea, got.Stage)
	events, err = ListGroupTaskEvents(started.ID)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskEventPromoted, events[len(events)-1].Kind)

	// A move beside a parked task parks it.
	moved, err := MoveGroupTask(queued.ID, GroupTaskPosition{Before: parked.ID}, "t")
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageBacklog, moved.Stage)

	// An assignee holds a task in the backlog; starting it takes it out.
	held, err := AssignGroupTaskHeld(idea.ID, worker, "t", false, GroupTaskStageBacklog, "t", AnyAssignee)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageBacklog, held.Stage)
	assert.False(t, held.Started())
	held, err = StartGroupTask(idea.ID, true, "", "t", AnyAssignee)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageNext, held.Stage)
	assert.True(t, held.Started())

	// A column stands in next, never the backlog.
	_, err = CreateGroupTask(NewGroupTask{GroupID: gid, Title: "x", Stage: GroupTaskStageBacklog, Column: "review", Actor: "t"})
	var conflict *GroupTaskConflict
	require.ErrorAs(t, err, &conflict)
	_, err = SetGroupTaskStage(parked.ID, "later", "t")
	require.ErrorAs(t, err, &conflict)

	// Getting in line for a full lane takes a task out of the backlog: a
	// queued task is in play.
	inLine, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "in line", Stage: GroupTaskStageBacklog, AssigneeID: worker, AssignedBy: "t", Hold: GroupTaskStageBacklog, Actor: "t"})
	require.NoError(t, err)
	inLine, err = QueueGroupTask(inLine.ID, QueueLaneInProgress, "full", "t", AnyAssignee)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageNext, inLine.Stage)
	assert.Equal(t, QueueLaneInProgress, inLine.QueuedFor)

	// A parked child counts in its epic as not started.
	epic, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "epic", Kind: GroupTaskKindEpic, Actor: "t"})
	require.NoError(t, err)
	_, err = EditGroupTask(parked.ID, GroupTaskEdit{ParentID: &epic.ID, Actor: "t"})
	require.NoError(t, err)
	// An epic is not parked, by any path.
	_, err = SetGroupTaskStage(epic.ID, GroupTaskStageBacklog, "t")
	require.ErrorAs(t, err, &conflict)
	_, err = MoveGroupTask(epic.ID, GroupTaskPosition{Before: parked.ID}, "t")
	require.ErrorAs(t, err, &conflict)
	_, err = CreateGroupTask(NewGroupTask{GroupID: gid, Title: "parked epic", Kind: GroupTaskKindEpic, Stage: GroupTaskStageBacklog, Actor: "t"})
	require.ErrorAs(t, err, &conflict)
	progress, err := GroupTaskEpicProgresses([]string{epic.ID})
	require.NoError(t, err)
	require.NotNil(t, progress[epic.ID])
	assert.Equal(t, 1, progress[epic.ID].Backlog)
	assert.Equal(t, 1, progress[epic.ID].Total)
}
