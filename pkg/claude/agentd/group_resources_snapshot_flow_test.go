package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type snapshotResource struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	GroupID int64  `json:"group_id"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Owner   string `json:"owner"`
	Detail  string `json:"detail"`
}

func (e *pageEnv) snapshotResources(t *testing.T) []snapshotResource {
	t.Helper()
	rec := e.dash(t, http.MethodGet, "/api/snapshot")
	require.Equal(t, http.StatusOK, rec.Code)
	var snap struct {
		GroupResources []snapshotResource `json:"group_resources"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	return snap.GroupResources
}

// tcl-1lth8e: the snapshot lists every group's resources in one shape, with
// what the server knows of each: a page's stage live (running) or not
// (closed), a drawing board that exists (closed: nothing of it runs).
func TestSnapshotGroupResourcesOneModel(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}}}))
	shower := e.agent(t, "res-shower", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)

	p, rec := e.show(t, shower, ":"+strconv.Itoa(devServer(t, "res")), "app", "App")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/drawing-boards", map[string]any{"id": "plan", "title": "Plan"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/drawing-boards/plan/group", map[string]any{"group": "crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := e.snapshotResources(t)
	require.Len(t, got, 2, "%+v", got)
	assert.Equal(t, snapshotResource{Kind: "drawing_board", ID: "plan", GroupID: got[0].GroupID, Title: "Plan", State: "closed"}, got[0])
	assert.Equal(t, snapshotResource{Kind: "page", ID: p.ID, GroupID: got[0].GroupID, Title: "App", State: "running", Owner: shower.id}, got[1])

	// Closed, the page stays, with why.
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	got = e.snapshotResources(t)
	require.Len(t, got, 2)
	assert.Equal(t, "closed", got[1].State)
	assert.Equal(t, "closed by the operator", got[1].Detail)
}

// A group's shell lists only while it runs. It has no saved state, so a
// closed row would stay forever with nothing to remove it by; once the shell
// exits (or before it ever started) the group lists no shell, and the ＋ menu
// starts one.
func TestSnapshotGroupShellListsOnlyWhileRunning(t *testing.T) {
	e := newPageEnv(t)
	for _, name := range []string{"live", "idle"} {
		e.f.HaveGroup(name)
		_, err := db.SetAgentGroupDefaultCwd(name, e.f.TestCwd(name))
		require.NoError(t, err)
	}
	e.f.World.Tmux.Register(docGroupShellSession("live"), e.f.TestCwd("live"), docShellPane{})

	got := e.snapshotResources(t)
	require.Len(t, got, 1, "only the running shell lists, the idle group's is not kept as closed: %+v", got)
	assert.Equal(t, snapshotResource{Kind: "shell", ID: "live", GroupID: got[0].GroupID, Title: "Group shell", State: "running", Detail: e.f.TestCwd("live")}, got[0])
}

// Every running shell lists as a group resource, not just a group's own: a
// shell in an agent's directory lists under the group it was opened from,
// or else the agent's own group, and only while it runs.
func TestSnapshotListsAgentShellsUnderTheirGroup(t *testing.T) {
	e := newPageEnv(t)
	worker := e.agent(t, "shell-worker", false)
	e.f.HaveGroup("crew")
	e.f.HaveGroup("side")
	e.f.HaveMember("crew", worker.conv)
	e.f.HaveMember("side", worker.conv)
	loner := e.agent(t, "shell-loner", false)
	e.f.World.Tmux.Register(agentd.TermSessionNameForTest(worker.conv, "start"), worker.cwd, docShellPane{})
	e.f.World.Tmux.Register(agentd.TermSessionNameForGroupForTest(worker.conv, "current", "side"), worker.cwd, docShellPane{})
	e.f.World.Tmux.Register(agentd.TermSessionNameForTest(loner.conv, "start"), loner.cwd, docShellPane{})

	groupID := map[string]int64{}
	for _, name := range []string{"crew", "side"} {
		g, err := db.GetAgentGroupByName(name)
		require.NoError(t, err)
		groupID[name] = g.ID
	}
	var shells []snapshotResource
	for _, r := range e.snapshotResources(t) {
		if r.Kind == "shell" {
			shells = append(shells, r)
		}
	}
	require.Len(t, shells, 2, "an agent in no group has no group to list its shell in: %+v", shells)
	byID := map[string]snapshotResource{}
	for _, r := range shells {
		byID[r.ID] = r
	}
	start, ok := byID["term:"+worker.id+":start:"]
	require.True(t, ok, "%+v", shells)
	assert.Equal(t, groupID["crew"], start.GroupID, "a shell opened outside a group lists under the agent's first group")
	assert.Equal(t, "running", start.State)
	assert.Equal(t, worker.cwd, start.Detail, "its detail is the directory it opened in")
	side, ok := byID["term:"+worker.id+":current:side"]
	require.True(t, ok, "%+v", shells)
	assert.Equal(t, groupID["side"], side.GroupID, "a shell opened from a group lists under that group")
}

// tcl-yc5ueb: a drawing board in no group (one made from the Collab panel's ＋ New),
// or in an archived one, lists with group_id 0, for the Groups tab's
// Ungrouped, until it is moved into a group.
func TestSnapshotListsDrawingBoardsInNoGroupUnderZero(t *testing.T) {
	e := newPageEnv(t)
	e.f.HaveGroup("crew")
	e.f.HaveGroup("old")
	for _, b := range []struct{ id, group string }{{"loose", ""}, {"kept", "crew"}, {"orphan", "old"}} {
		rec := e.dashJSON(t, http.MethodPost, "/api/drawing-boards", map[string]any{"id": b.id, "title": b.id})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if b.group != "" {
			rec = e.dashJSON(t, http.MethodPost, "/api/drawing-boards/"+b.id+"/group", map[string]any{"group": b.group})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
	}
	require.NoError(t, db.ArchiveAgentGroup("old"))

	groupOf := func() map[string]int64 {
		out := map[string]int64{}
		for _, r := range e.snapshotResources(t) {
			if r.Kind == "drawing_board" {
				out[r.ID] = r.GroupID
			}
		}
		return out
	}
	got := groupOf()
	require.Len(t, got, 3, "%+v", got)
	assert.Zero(t, got["loose"], "a drawing board in no group lists under 0")
	assert.Zero(t, got["orphan"], "a drawing board whose group is archived lists under 0")
	crew := got["kept"]
	assert.NotZero(t, crew)

	rec := e.dashJSON(t, http.MethodPost, "/api/drawing-boards/loose/group", map[string]any{"group": "crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, crew, groupOf()["loose"], "moved into a group, it lists there")
}

type snapshotResourceTimes struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	LastAt    string `json:"last_at"`
}

func (e *pageEnv) snapshotResourceTimes(t *testing.T) map[string]snapshotResourceTimes {
	t.Helper()
	rec := e.dash(t, http.MethodGet, "/api/snapshot")
	require.Equal(t, http.StatusOK, rec.Code)
	var snap struct {
		GroupResources []snapshotResourceTimes `json:"group_resources"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	out := map[string]snapshotResourceTimes{}
	for _, r := range snap.GroupResources {
		out[r.Kind+":"+r.ID] = r
	}
	return out
}

func parseSnapshotTime(t *testing.T, what, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err, "%s: %q is not RFC3339", what, value)
	return at
}

// The Resources table's Age and Last come with each resource: a drawing
// board's creation and last save, a page's first show and last show (a closed
// one's close), a pinned process's pin. A shell whose tmux renders no session
// times (the sim's zero) shows blank rather than the epoch.
func TestSnapshotGroupResourceTimes(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}}}))
	shower := e.agent(t, "times-shower", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	crew, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("crew", e.f.TestCwd("crew"))
	require.NoError(t, err)
	e.f.World.Tmux.Register(docGroupShellSession("crew"), e.f.TestCwd("crew"), docShellPane{})
	require.NoError(t, db.PinProcessToGroup(crew.ID, "tpl_times"))

	p, rec := e.show(t, shower, ":"+strconv.Itoa(devServer(t, "times")), "app", "App")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/drawing-boards", map[string]any{"id": "plan", "title": "Plan"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/drawing-boards/plan/group", map[string]any{"group": "crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := e.snapshotResourceTimes(t)
	board, err := db.GetCollabDrawingBoard("plan")
	require.NoError(t, err)
	b := got["drawing_board:plan"]
	assert.True(t, parseSnapshotTime(t, "board age", b.CreatedAt).Equal(board.CreatedAt), "board age is its creation: %+v", b)
	assert.True(t, parseSnapshotTime(t, "board last", b.LastAt).Equal(board.UpdatedAt), "board last is its last save: %+v", b)

	page := got["page:"+p.ID]
	pageAge := parseSnapshotTime(t, "page age", page.CreatedAt)
	assert.False(t, parseSnapshotTime(t, "page last", page.LastAt).Before(pageAge), "a live page's last show is not before its first: %+v", page)

	process := got["process:tpl_times"]
	assert.NotEmpty(t, process.CreatedAt, "a pinned process's age is its pin: %+v", process)
	parseSnapshotTime(t, "process age", process.CreatedAt)
	assert.Empty(t, process.LastAt, "a process with no active run has no last: %+v", process)

	shell := got["shell:crew"]
	assert.Equal(t, snapshotResourceTimes{Kind: "shell", ID: "crew"}, shell, "zero tmux times render blank")

	// Closed, the page's last is its close, and its age is still its show.
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	rows, err := db.ListGroupPages()
	require.NoError(t, err)
	var saved *db.GroupPage
	for _, row := range rows {
		if row.ID == p.ID {
			saved = row
		}
	}
	require.NotNil(t, saved, "a closed group page is saved")
	closed := e.snapshotResourceTimes(t)["page:"+p.ID]
	assert.True(t, parseSnapshotTime(t, "closed page age", closed.CreatedAt).Equal(saved.CreatedAt), "%+v", closed)
	want := saved.ClosedAt
	if want.IsZero() {
		want = saved.UpdatedAt
	}
	assert.True(t, parseSnapshotTime(t, "closed page last", closed.LastAt).Equal(want), "%+v", closed)
}
