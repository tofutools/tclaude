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
