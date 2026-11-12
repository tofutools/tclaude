package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A board's setup travels with its group (docs/taskboard.md "Carrying a
// board's setup"): an export, a clone and a group template carry its
// options and custom columns, never its tasks, unless left out.

// haveBoardSetup gives group's board a custom column (cordoned), a hidden
// Done column, and options that set the idle check and the column's limit.
func haveBoardSetup(t *testing.T, f *testharness.Flow, group string) *db.AgentGroup {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	_, err = db.AddGroupTaskColumn(g.ID, "review", "Review", "", "test")
	require.NoError(t, err)
	require.NoError(t, db.SetGroupTaskColumnState(g.ID, "review", db.GroupTaskColumnCordoned))
	require.NoError(t, db.SetGroupTaskColumnState(g.ID, db.GroupTaskColumnDone, db.GroupTaskColumnHidden))
	rec := humanReq(t, f, http.MethodPatch, "/v1/groups/"+group+"/taskboard/options", map[string]any{"set": map[string]string{
		"idle.enabled": "on", "limits.columns.review.max": "2",
	}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// A task, which no carry takes along.
	_, err = db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: "stays home", Actor: "test"})
	require.NoError(t, err)
	return g
}

// requireBoardSetup asserts group's board has the setup haveBoardSetup gave,
// and no tasks.
func requireBoardSetup(t *testing.T, group string) {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	cols, err := db.ListGroupTaskColumns(g.ID)
	require.NoError(t, err)
	require.Len(t, cols, 1, "the custom column came along")
	assert.Equal(t, "review", cols[0].Slug)
	assert.Equal(t, "Review", cols[0].Label)
	states, err := db.ListGroupTaskColumnStates(g.ID)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskColumnCordoned, states["review"], "the column's state came along")
	assert.Equal(t, db.GroupTaskColumnHidden, states[db.GroupTaskColumnDone], "the hidden Done came along")
	opts, err := db.GetGroupTaskboardOptions(g.ID)
	require.NoError(t, err)
	o, err := boardopts.Parse(opts.Options)
	require.NoError(t, err)
	assert.Equal(t, []string{"idle.enabled", "limits.columns.review.max"}, o.SetKeys([]string{"review"}), "the options came along")
	tasks, err := db.ListGroupTasks(g.ID, db.GroupTaskListOpts{})
	require.NoError(t, err)
	assert.Empty(t, tasks, "no task travels with the setup")
}

// requireBareBoard asserts group's board is as a new board starts.
func requireBareBoard(t *testing.T, group string) {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	tb, err := db.GroupTaskboardSetup(g.ID)
	require.NoError(t, err)
	assert.Nil(t, tb, "the board setup was left out")
}

func haveBoardGroup(t *testing.T, f *testharness.Flow, group, conv string) *db.AgentGroup {
	t.Helper()
	f.HaveConvWithTitle(conv, group+"-worker")
	f.HaveAliveSession(conv, "lbl-"+group, "tmux-"+group, f.TestCwd(group))
	f.HaveGroup(group)
	f.HaveMember(group, conv)
	return haveBoardSetup(t, f, group)
}

func exportGroupQuery(t *testing.T, f *testharness.Flow, group, query string) []byte {
	t.Helper()
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet, "/v1/groups/"+group+"/export?"+query, nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.Bytes()
}

func TestBoardSetupGroupExportImport(t *testing.T) {
	f := newFlow(t)
	haveBoardGroup(t, f, "team", "aaaaaaaa-1111-2222-3333-444444444444")
	into := f.TestCwd("landing")

	archive := exportGroupQuery(t, f, "team", "format=paf")

	// The preview shows the setup.
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost,
		"/v1/groups/import/inspect?as=team-copy", bytes.NewReader(archive))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		Taskboard *struct {
			Columns []struct {
				Slug, Label, State string
			} `json:"columns"`
			BuiltinStates map[string]string `json:"builtin_states"`
			Set           []string          `json:"set"`
		} `json:"taskboard"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	require.NotNil(t, insp.Taskboard, "the preview shows the board setup: %s", rec.Body.String())
	require.Len(t, insp.Taskboard.Columns, 1)
	assert.Equal(t, "review", insp.Taskboard.Columns[0].Slug)
	assert.Equal(t, db.GroupTaskColumnCordoned, insp.Taskboard.Columns[0].State)
	assert.Equal(t, map[string]string{"done": "hidden"}, insp.Taskboard.BuiltinStates)
	assert.Equal(t, []string{"idle.enabled", "limits.columns.review.max"}, insp.Taskboard.Set)

	// The import lands it by default.
	importArchiveOK(t, f, archive, into, "team-copy")
	requireBoardSetup(t, "team-copy")

	// Unticked, the import leaves it out.
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost,
		"/v1/groups/import?board_options=false&as=team-bare&into="+into, bytes.NewReader(archive))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireBareBoard(t, "team-bare")
}

func TestBoardSetupGroupExportLeftOut(t *testing.T) {
	f := newFlow(t)
	haveBoardGroup(t, f, "team", "aaaaaaaa-1111-2222-3333-444444444444")
	archive := exportGroupQuery(t, f, "team", "format=paf&board_options=false")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost,
		"/v1/groups/import/inspect?as=team-copy", bytes.NewReader(archive))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"taskboard"`, "an export without board options carries none")
	importArchiveOK(t, f, archive, f.TestCwd("landing"), "team-copy")
	requireBareBoard(t, "team-copy")
}

func TestBoardSetupClone(t *testing.T) {
	f := newFlow(t)
	haveBoardGroup(t, f, "team", "aaaaaaaa-1111-2222-3333-444444444444")

	resp := groupCloneRequest(t, f, "team", map[string]any{"new_name": "team-two", "no_clone_members": true})
	assert.Equal(t, "team-two", resp.Group)
	requireBoardSetup(t, "team-two")

	groupCloneRequest(t, f, "team", map[string]any{"new_name": "team-three", "no_clone_members": true, "no_board_options": true})
	requireBareBoard(t, "team-three")
}

func TestBoardSetupTemplate(t *testing.T) {
	f := newFlow(t)
	haveBoardGroup(t, f, "team", "aaaaaaaa-1111-2222-3333-444444444444")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/from-group", map[string]any{"group": "team", "template_name": "team-tpl"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/from-group", map[string]any{"group": "team", "template_name": "bare-tpl", "no_board_options": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// The template carries the setup through its export and import.
	rec = humanReq(t, f, http.MethodGet, "/v1/templates/team-tpl/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var env map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Contains(t, env["template"], "taskboard", "the export carries the setup")
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/import?as=imported-tpl", env)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/import?as=imported-bare&board_options=false", env)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	for _, c := range []struct {
		tmpl, group string
		carried     bool
	}{{"team-tpl", "made", true}, {"imported-tpl", "made-imported", true}, {"bare-tpl", "made-bare", false}, {"imported-bare", "made-imported-bare", false}} {
		rec := humanReq(t, f, http.MethodGet, "/v1/templates/"+c.tmpl, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		if c.carried {
			assert.Contains(t, rec.Body.String(), `"taskboard"`, c.tmpl)
		} else {
			assert.NotContains(t, rec.Body.String(), `"taskboard"`, c.tmpl)
		}
		rec = humanReq(t, f, http.MethodPost, "/v1/templates/"+c.tmpl+"/instantiate", map[string]any{"group_name": c.group, "task": "go"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		agentd.WaitForBackgroundForTest()
		if c.carried {
			requireBoardSetup(t, c.group)
		} else {
			requireBareBoard(t, c.group)
		}
	}
}

// A setup from outside is checked: what this node's boards cannot take is
// left out with a warning, and the rest lands.
func TestBoardSetupTemplateImportChecked(t *testing.T) {
	f := newFlow(t)
	body := fullTemplateBody("outside")
	body["taskboard"] = map[string]any{
		"columns": []map[string]any{
			{"slug": "review", "label": "Review", "state": "cordoned"},
			{"slug": "Bad Slug", "label": "Bad"},
			{"slug": "qa", "label": "QA", "state": "sideways"},
		},
		"builtin_states": map[string]string{"next": "hidden"},
		"options":        map[string]any{"idle": map[string]any{"enabled": true}, "nonsense": 1},
	}
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", map[string]any{
		"format": "tclaude-task-force", "format_version": 3, "template": body,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	assert.NotEmpty(t, ir.Warnings, "what is left out is said")

	rec = humanReq(t, f, http.MethodGet, "/v1/templates/outside", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		Taskboard *struct {
			Columns []struct {
				Slug, Label, State string
			} `json:"columns"`
			BuiltinStates map[string]string `json:"builtin_states"`
		} `json:"taskboard"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.Taskboard, rec.Body.String())
	require.Len(t, got.Taskboard.Columns, 2, "the bad slug is left out")
	assert.Equal(t, "review", got.Taskboard.Columns[0].Slug)
	assert.Equal(t, "qa", got.Taskboard.Columns[1].Slug)
	assert.Empty(t, got.Taskboard.Columns[1].State, "an unknown state is left out")
	assert.Empty(t, got.Taskboard.BuiltinStates, "only ideas and done hide")
}

// A restricted peer's clone copies the board setup only when the peer reads
// the source's board (taskboard.read there).
func TestPeerBoardSetupClone(t *testing.T) {
	h := newPeerProcessHarness(t)
	haveBoardSetup(t, h.f, "crew")
	h.grant(t, agentd.PermGroupsClone, "group=crew")
	h.grant(t, agentd.PermNodeGroupsCreate, "")

	code, body := h.call("POST", "/api/groups/crew/clone", map[string]any{"new_name": "crew2", "no_clone_members": true})
	require.Equal(t, 200, code, body)
	requireBareBoard(t, "crew2")

	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	code, body = h.call("POST", "/api/groups/crew/clone", map[string]any{"new_name": "crew3", "no_clone_members": true})
	require.Equal(t, 200, code, body)
	requireBoardSetup(t, "crew3")
}
