package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

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
// (closed), a board that exists (closed: nothing of it runs).
func TestSnapshotGroupResourcesOneModel(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}}}))
	shower := e.agent(t, "res-shower", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)

	p, rec := e.show(t, shower, ":"+strconv.Itoa(devServer(t, "res")), "app", "App")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/boards", map[string]any{"id": "plan", "title": "Plan"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/boards/plan/group", map[string]any{"group": "crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := e.snapshotResources(t)
	require.Len(t, got, 2, "%+v", got)
	assert.Equal(t, snapshotResource{Kind: "board", ID: "plan", GroupID: got[0].GroupID, Title: "Plan", State: "closed"}, got[0])
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

// tcl-yc5ueb: a board in no group (one made from the Collab panel's ＋ New),
// or in an archived one, lists with group_id 0, for the Groups tab's
// Ungrouped, until it is moved into a group.
func TestSnapshotListsBoardsInNoGroupUnderZero(t *testing.T) {
	e := newPageEnv(t)
	e.f.HaveGroup("crew")
	e.f.HaveGroup("old")
	for _, b := range []struct{ id, group string }{{"loose", ""}, {"kept", "crew"}, {"orphan", "old"}} {
		rec := e.dashJSON(t, http.MethodPost, "/api/boards", map[string]any{"id": b.id, "title": b.id})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if b.group != "" {
			rec = e.dashJSON(t, http.MethodPost, "/api/boards/"+b.id+"/group", map[string]any{"group": b.group})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
	}
	require.NoError(t, db.ArchiveAgentGroup("old"))

	groupOf := func() map[string]int64 {
		out := map[string]int64{}
		for _, r := range e.snapshotResources(t) {
			if r.Kind == "board" {
				out[r.ID] = r.GroupID
			}
		}
		return out
	}
	got := groupOf()
	require.Len(t, got, 3, "%+v", got)
	assert.Zero(t, got["loose"], "a board in no group lists under 0")
	assert.Zero(t, got["orphan"], "a board whose group is archived lists under 0")
	crew := got["kept"]
	assert.NotZero(t, crew)

	rec := e.dashJSON(t, http.MethodPost, "/api/boards/loose/group", map[string]any{"group": "crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, crew, groupOf()["loose"], "moved into a group, it lists there")
}
