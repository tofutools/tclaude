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

// tcl-kostyh: the operator renames a page or a board from its group's
// Resources row, and moves a page to another group its agent is in.

func TestPageRenameAndMoveFromTheGroup(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{
		Agent:    &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage}},
		Features: &config.FeaturesConfig{Processes: true},
	}))
	shower := e.agent(t, "mover", true)
	e.f.HaveGroup("crew")
	e.f.HaveGroup("ops")
	e.f.HaveGroup("elsewhere")
	e.f.HaveMember("crew", shower.conv)
	target := ":" + strconv.Itoa(devServer(t, "moving"))
	p, rec := e.show(t, shower, target, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, "crew", p.Group)
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks()).Code)

	// Rename, open: the listing and the saved row both say so.
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/title", map[string]any{"title": "Checkout preview"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, e.ls(t, shower), 1)
	assert.Equal(t, "Checkout preview", e.ls(t, shower)[0].Title)
	assert.Equal(t, http.StatusBadRequest, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/title", map[string]any{"title": "  "}).Code)

	// The agent's show without a title keeps the operator's; one with a
	// title replaces it, and the audit says whose it replaced.
	_, rec = e.show(t, shower, target, "app", "")
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, "Checkout preview", e.ls(t, shower)[0].Title)
	_, rec = e.show(t, shower, target, "app", "Agent's own")
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, "Agent's own", e.ls(t, shower)[0].Title)
	shows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "page.show"})
	require.NoError(t, err)
	require.NotEmpty(t, shows)
	assert.Contains(t, shows[0].Detail, `replaced the operator's title "Checkout preview"`)
	_, rec = e.show(t, shower, target, "app", "")
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Equal(t, "app", e.ls(t, shower)[0].Title, "the agent's title holds only while it passes one")
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/title", map[string]any{"title": "Checkout preview"}).Code)

	// Move, open: only to a group its agent is in; the marks stay behind.
	e.f.HaveMember("ops", shower.conv)
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/group", map[string]any{"group": "elsewhere"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not_member")
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/group", map[string]any{"group": "ops"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "ops", e.ls(t, shower)[0].Group)
	require.Len(t, e.groupPages(t, "ops"), 1)
	assert.Empty(t, e.groupPages(t, "crew"))
	assert.Empty(t, e.marks(t, shower, "app", "").Batches, "the marks went to crew's audience")

	// Closed: rename and move back, and it reopens there under its new title.
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/title", map[string]any{"title": "Old checkout"}).Code)
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/group", map[string]any{"group": "crew"}).Code)
	row, err := db.GetGroupPage(p.ID)
	require.NoError(t, err)
	assert.Equal(t, "Old checkout", row.Title)
	assert.True(t, row.TitleByOperator)
	require.Len(t, e.groupPages(t, "crew"), 1)

	// A show of the saved name reopens it under the operator's title.
	rec = e.call(t, shower, http.MethodPost, "/v1/pages", map[string]any{"target": target, "name": "app", "group": "crew"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var again pageOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &again))
	require.Equal(t, p.ID, again.ID)
	assert.Equal(t, "Old checkout", e.ls(t, shower)[0].Title)
	assert.Equal(t, http.StatusNotFound, e.dashJSON(t, http.MethodPost, "/api/pages/pg_nope/group", map[string]any{"group": "crew"}).Code)
}

func TestBoardRenameFromTheGroup(t *testing.T) {
	e := newPageEnv(t)
	rec := e.dashJSON(t, http.MethodPost, "/api/boards", map[string]any{"id": "plan", "title": "Plan"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/boards/plan/title", map[string]any{"title": "Release plan"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	b, err := db.GetCollabBoard("plan")
	require.NoError(t, err)
	assert.Equal(t, "Release plan", b.Title)
	assert.Equal(t, http.StatusBadRequest, e.dashJSON(t, http.MethodPost, "/api/boards/plan/title", map[string]any{"title": ""}).Code)
	assert.Equal(t, http.StatusNotFound, e.dashJSON(t, http.MethodPost, "/api/boards/nope/title", map[string]any{"title": "x"}).Code)
}
