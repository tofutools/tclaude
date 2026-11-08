package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV327PhaseTwoQuestions: a phase 2 question task's question
// moves into a question row and an entry dated when the task was created,
// its "answered" change entry becomes the answer entry tied to it, and its
// operator_request keeps only the marker. A second run changes nothing.
func TestMigrateV327PhaseTwoQuestions(t *testing.T) {
	d := freshMigratedDB(t)
	_, err := d.Exec(`
		INSERT INTO agent_groups (id, name, created_at) VALUES (1, 'crew', 1);
		INSERT INTO group_tasks (id, group_id, title, stage, status, rank, created_by, created_at, updated_at, closed_at, assignee_kind, operator_request)
		VALUES
			('tk_answered', 1, 'Lane colour?', 'next', 'done', 'a', 'agent:agt_w', 100, 300, 300, 'operator',
				'{"kind":"question","options":[{"key":"amber","label":"Amber"},{"key":"teal","label":"Teal"}],"recommend":"amber","asked_by":"agent:agt_w","answer":{"picked":["teal"],"text":"dimmer","answered_by":"human:operator","answered_at":"1970-01-01T00:00:00.0000003Z"}}'),
			('tk_open', 1, 'Ship it?', 'next', 'open', 'b', 'agent:agt_w', 400, 400, NULL, 'operator',
				'{"kind":"question","asked_by":"agent:agt_w","asked_via":"peer:node1/agent:agt_r"}'),
			('tk_plain', 1, 'Plain', 'next', 'open', 'c', 'human:operator', 500, 500, NULL, '', '');
		INSERT INTO group_task_events (id, task_id, at, actor, kind, detail) VALUES
			(1, 'tk_answered', 100, 'agent:agt_w', 'created', 'in next'),
			(2, 'tk_answered', 300, 'human:operator', 'answered', 'Answer: Teal; dimmer'),
			(3, 'tk_answered', 300, 'human:operator', 'done', 'Answer: Teal; dimmer'),
			(4, 'tk_open', 400, 'agent:agt_w', 'created', 'in next');
	`)
	require.NoError(t, err)

	run := func() {
		tx, err := d.Begin()
		require.NoError(t, err)
		require.NoError(t, migrateV327PhaseTwoQuestions(tx))
		require.NoError(t, tx.Commit())
	}
	run()
	run() // idempotent

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM group_task_questions`).Scan(&n))
	assert.Equal(t, 2, n)

	q, err := getGroupTaskQuestionTx(d, "tk_answered", 1)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskQuestionAnswered, q.State)
	assert.Equal(t, "Lane colour?", q.Title)
	assert.Equal(t, "amber", q.Recommend)
	require.Len(t, q.Options, 2)
	require.NotNil(t, q.Answer)
	assert.Equal(t, []string{"teal"}, q.Answer.Picked)
	assert.Equal(t, int64(2), q.AnswerEventID, "the answered change entry is the answer entry")

	// The stream, in order: created, the question (dated at create), the
	// answer, done.
	events, err := queryRows(d, scanGroupTaskEvent, `SELECT `+groupTaskEventCols+` FROM group_task_events WHERE task_id = ? ORDER BY at, id`, "tk_answered")
	require.NoError(t, err)
	var got []string
	for _, e := range events {
		got = append(got, e.Kind+"/"+e.CommentKind)
	}
	assert.Equal(t, []string{"created/", "comment/question", "comment/answer", "done/"}, got)
	assert.Equal(t, q.EventID, events[2].CommentID)
	assert.Equal(t, "Answer: Teal; dimmer", events[2].Body)

	open, err := getGroupTaskQuestionTx(d, "tk_open", 1)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskQuestionOpen, open.State)
	assert.Equal(t, "peer:node1/agent:agt_r", open.AskedVia)

	var req string
	require.NoError(t, d.QueryRow(`SELECT operator_request FROM group_tasks WHERE id = 'tk_open'`).Scan(&req))
	assert.Equal(t, groupTaskQuestionMarker, req)
	require.NoError(t, d.QueryRow(`SELECT operator_request FROM group_tasks WHERE id = 'tk_plain'`).Scan(&req))
	assert.Empty(t, req)
}
