package db

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGroupTaskBoardScale measures the board reads the Groups tab and the
// daemon make against a board grown to a size, in an isolated HOME. It is
// opt-in: TCLAUDE_TASKBOARD_SCALE=<closed tasks> (e.g. 20000, 200000).
//
//	TCLAUDE_TASKBOARD_SCALE=20000 go test ./pkg/claude/common/db -run TestGroupTaskBoardScale -v
func TestGroupTaskBoardScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("TCLAUDE_TASKBOARD_SCALE"))
	if n <= 0 {
		t.Skip("set TCLAUDE_TASKBOARD_SCALE=<closed tasks> to measure")
	}
	setupTestDB(t)
	groups, agents := seedTaskboardScale(t, n, 8, 100)
	gid := groups[0]
	var live []string
	tasks, err := ListGroupTasks(gid, GroupTaskListOpts{Closed: true})
	require.NoError(t, err)
	for _, task := range tasks {
		live = append(live, task.ID)
	}
	var archivedID string
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT id FROM group_tasks WHERE archived_at IS NOT NULL LIMIT 1`).Scan(&archivedID))

	type op struct {
		name string
		fn   func() error
	}
	ops := []op{
		{"board: ListGroupTasks (live)", func() error { _, err := ListGroupTasks(gid, GroupTaskListOpts{}); return err }},
		{"board: ListGroupTasks (+recently closed)", func() error {
			_, err := ListGroupTasks(gid, GroupTaskListOpts{Closed: true})
			return err
		}},
		{"board views: statuses+attachments+activity+questions", func() error {
			if _, err := GroupTaskStatuses(live); err != nil {
				return err
			}
			if _, err := ListGroupTaskAttachmentsBatch(live); err != nil {
				return err
			}
			if _, err := GroupTaskActivitySummaries(live); err != nil {
				return err
			}
			_, err := ListGroupTaskQuestions(live)
			return err
		}},
		{"fleet: blocked+operator+question counts", func() error {
			if _, err := CountBlockedGroupTasks(); err != nil {
				return err
			}
			if _, err := CountOperatorGroupTasks(); err != nil {
				return err
			}
			_, err := CountOpenGroupTaskQuestions()
			return err
		}},
		{"stall scan: ListOpenAssignedGroupTasks", func() error { _, err := ListOpenAssignedGroupTasks(); return err }},
		{"agent: ListGroupTasksAssignedTo", func() error { _, err := ListGroupTasksAssignedTo(agents[0]); return err }},
		{"sweep: SweepGroupTasks (nothing due)", func() error {
			_, _, err := SweepGroupTasks(time.Now().Add(-14*24*time.Hour), time.Now().Add(-90*24*time.Hour))
			return err
		}},
		{"create: CreateGroupTask", func() error {
			_, err := CreateGroupTask(NewGroupTask{GroupID: groups[1], Title: "bench", Actor: "bench"})
			return err
		}},
		{"lookup: GetGroupTask (archived id)", func() error { _, err := GetGroupTask(archivedID); return err }},
		{"history: ListGroupTaskEvents (archived id)", func() error { _, err := ListGroupTaskEvents(archivedID); return err }},
		{"archive view: ListGroupTasks (archived)", func() error {
			_, err := ListGroupTasks(gid, GroupTaskListOpts{Archived: true})
			return err
		}},
	}
	var report strings.Builder
	fmt.Fprintf(&report, "closed=%d groups=%d live=100\n", n, len(groups))
	for _, o := range ops {
		var times []time.Duration
		for range 15 {
			start := time.Now()
			require.NoError(t, o.fn(), o.name)
			times = append(times, time.Since(start))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		fmt.Fprintf(&report, "%-55s median %9s  p90 %9s\n", o.name, times[len(times)/2].Round(time.Microsecond), times[len(times)*9/10].Round(time.Microsecond))
	}
	t.Log("\n" + report.String())
}

// seedTaskboardScale writes closed tasks spread over groups (up to 3000
// closed in the last two weeks, the rest archived), live tasks, and the dependents a
// busy board accumulates: ~8 events with 2 comments per task, an attachment
// per 10, a question per 20, a dependency per 5. It returns the group ids
// and the agent ids.
func seedTaskboardScale(t *testing.T, closed, nGroups, live int) ([]int64, []string) {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	var groups []int64
	for i := range nGroups {
		gid, err := CreateAgentGroup(fmt.Sprintf("crew-%d", i), "")
		require.NoError(t, err)
		groups = append(groups, gid)
	}
	var agents []string
	for i := range 20 {
		id, _, err := EnsureAgentForConv(fmt.Sprintf("conv-scale-%d", i), "test")
		require.NoError(t, err)
		agents = append(agents, id)
	}
	tx, err := d.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	insTask, err := tx.Prepare(`INSERT INTO group_tasks (id, group_id, title, body, stage, status, rank, assignee_id, assigned_at,
		started_at, outcome, created_by, created_at, updated_at, closed_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'agent:x', ?, ?, ?, ?)`)
	require.NoError(t, err)
	insEvent, err := tx.Prepare(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind) VALUES (?, ?, 'agent:x', ?, '', ?, ?)`)
	require.NoError(t, err)
	insAtt, err := tx.Prepare(`INSERT INTO group_task_attachments (task_id, name, sha256, size_bytes, content_type, added_by, added_at)
		VALUES (?, 'shot.png', ?, 1000, 'image/png', 'agent:x', ?)`)
	require.NoError(t, err)
	insQ, err := tx.Prepare(`INSERT INTO group_task_questions (task_id, n, event_id, title, spec, asked_by, asked_at, state, closed_at)
		VALUES (?, 1, ?, 'q', '{}', 'agent:x', ?, 'answered', ?)`)
	require.NoError(t, err)
	insDep, err := tx.Prepare(`INSERT INTO group_task_deps (task_id, depends_on) VALUES (?, ?)`)
	require.NoError(t, err)
	now := time.Now()
	kinds := []string{"created", "moved", "assigned", "started", "comment", "comment", "done", "archived"}
	prev := map[int64]string{}
	// Two weeks of closures stay on the board's history whatever the archive
	// holds: ~3000 for a busy fleet, so a larger seed grows only the archive.
	recent, maxRecent := 0, 3000
	for i := range closed + live {
		gid := groups[i%nGroups]
		id := fmt.Sprintf("tk_%08x", i)
		isLive := i >= closed
		created := now.Add(-time.Duration(i%(90*24)) * time.Hour)
		status, stage := "done", "next"
		var closedAt, archivedAt any
		if isLive {
			status = []string{"open", "open", "open", "blocked"}[i%4]
			if i%3 == 0 {
				stage = "idea"
			}
			created = now.Add(-time.Duration(i%48) * time.Hour)
		} else {
			if i%7 == 0 {
				status = "dropped"
			}
			c := created.Add(2 * time.Hour)
			if i%20 < 3 && recent < maxRecent { // closed in the last two weeks, not archived
				recent++
				c = now.Add(-time.Duration(i%(13*24)) * time.Hour)
			} else {
				archivedAt = dbTime(c.Add(14 * 24 * time.Hour))
			}
			closedAt = dbTime(c)
		}
		var assignee, assignedAt, startedAt any
		if i%4 != 0 {
			assignee, assignedAt, startedAt = agents[i%len(agents)], dbTime(created), dbTime(created)
		}
		outcome := ""
		if !isLive && assignee != nil {
			outcome = "succeeded"
		}
		_, err := insTask.Exec(id, gid, "Task "+id, strings.Repeat("body text ", 20), stage, status, fmt.Sprintf("m%08d", i),
			assignee, assignedAt, startedAt, outcome, dbTime(created), dbTime(created), closedAt, archivedAt)
		require.NoError(t, err)
		for k, kind := range kinds {
			if isLive && k > 3 {
				break
			}
			body, ck := "", ""
			if kind == "comment" {
				body, ck = strings.Repeat("a comment ", 15), "note"
			}
			res, err := insEvent.Exec(id, dbTime(created.Add(time.Duration(k)*time.Minute)), kind, body, ck)
			require.NoError(t, err)
			if i%20 == 0 && k == 4 {
				ev, _ := res.LastInsertId()
				_, err = insQ.Exec(id, ev, dbTime(created), dbTime(created))
				require.NoError(t, err)
			}
		}
		if i%10 == 0 {
			_, err = insAtt.Exec(id, fmt.Sprintf("%064x", i), dbTime(created))
			require.NoError(t, err)
		}
		if p := prev[gid]; p != "" && i%5 == 0 {
			_, err = insDep.Exec(id, p)
			require.NoError(t, err)
		}
		prev[gid] = id
	}
	require.NoError(t, tx.Commit())
	_, err = d.Exec(`PRAGMA optimize`)
	require.NoError(t, err)
	return groups, agents
}
