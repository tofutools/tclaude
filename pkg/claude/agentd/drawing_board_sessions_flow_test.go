package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type drawingBoardSessionRow struct {
	Kind         string `json:"kind"`
	Key          string `json:"key"`
	DrawingBoard string `json:"drawingBoard"`
	Label        string `json:"label"`
	Killable     bool   `json:"killable"`
	LastActivity int64  `json:"last_activity"`
	Agents       []struct {
		AgentID string `json:"agent_id"`
		Title   string `json:"title"`
	} `json:"agents"`
}

type drawingBoardSessionFixture struct {
	t    *testing.T
	mux  http.Handler
	dash http.Handler
}

// drawingBoardSessionFlow is a collab-enabled daemon with one drawing board, "plan".
func drawingBoardSessionFlow(t *testing.T) (*testharness.Flow, drawingBoardSessionFixture) {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	fx := drawingBoardSessionFixture{t: t, mux: f.Mux, dash: agentd.BuildDashboardHandlerForTest()}
	require.Equal(t, http.StatusCreated, fx.operator(http.MethodPost, "/api/drawing-boards", map[string]any{"id": "plan", "title": "Release plan"}).Code)
	return f, fx
}

func (fx drawingBoardSessionFixture) operator(method, path string, body any) *httptest.ResponseRecorder {
	fx.t.Helper()
	return testharness.Serve(fx.dash, drawingBoardDashReq(fx.t, method, path, body))
}

func (fx drawingBoardSessionFixture) agent(conv, method, path string, body any) *httptest.ResponseRecorder {
	fx.t.Helper()
	return testharness.Serve(fx.mux, agentd.AsAgentPeer(testharness.JSONRequest(fx.t, method, path, body), conv))
}

// drawingBoardAgent adds an agent holding drawing-boards.write and returns its id.
func drawingBoardAgent(t *testing.T, f *testharness.Flow, conv, title string) string {
	t.Helper()
	f.HaveConvWithTitle(conv, title)
	id, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermDrawingBoardsWrite, "test"))
	return id
}

// rows reads the drawing board rows of the operator's Background terminals listing.
func (fx drawingBoardSessionFixture) rows() []drawingBoardSessionRow {
	fx.t.Helper()
	rec := fx.operator(http.MethodGet, "/api/background-terminals", nil)
	require.Equal(fx.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Terminals []drawingBoardSessionRow `json:"terminals"`
	}
	require.NoError(fx.t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	out := []drawingBoardSessionRow{}
	for _, row := range resp.Terminals {
		if row.Kind == "drawing_board" {
			out = append(out, row)
		}
	}
	return out
}

func (fx drawingBoardSessionFixture) requireAgents(agentIDs ...string) {
	fx.t.Helper()
	rows := fx.rows()
	if len(agentIDs) == 0 {
		require.Empty(fx.t, rows)
		return
	}
	require.Len(fx.t, rows, 1)
	got := []string{}
	for _, a := range rows[0].Agents {
		got = append(got, a.AgentID)
	}
	require.ElementsMatch(fx.t, agentIDs, got)
}

func (fx drawingBoardSessionFixture) draw(conv, id string, version, nonce int64) int {
	fx.t.Helper()
	rec := fx.agent(conv, http.MethodPatch, "/v1/drawing-boards/plan", map[string]any{"elements": []any{rect(id, version, nonce, 0)}})
	require.Equal(fx.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Accepted int `json:"accepted"`
	}
	require.NoError(fx.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Accepted
}

// Scenario: an agent's accepted edit starts a drawing board session and later ones
// refresh it. The operator's edits start none, and neither does an edit that
// changed nothing.
func TestDrawingBoardSessionsStartOnAcceptedAgentEdit(t *testing.T) {
	f, fx := drawingBoardSessionFlow(t)
	const conv = "drawing-board-session-edit"
	agentID := drawingBoardAgent(t, f, conv, "drawer")

	rec := fx.operator(http.MethodPatch, "/api/drawing-boards/plan", map[string]any{"elements": []any{rect("op", 1, 1, 0)}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fx.requireAgents()

	require.Zero(t, fx.draw(conv, "op", 1, 2), "a losing patch is not an edit")
	fx.requireAgents()

	require.Equal(t, 1, fx.draw(conv, "n1", 1, 1))
	fx.requireAgents(agentID)
	rows := fx.rows()
	assert.Equal(t, "drawing_board:plan", rows[0].Key, "the row carries the drawing board's pane key")
	assert.Equal(t, "plan", rows[0].DrawingBoard)
	assert.Equal(t, "Release plan", rows[0].Label)
	assert.Equal(t, "drawer", rows[0].Agents[0].Title)
	assert.True(t, rows[0].Killable, "the operator can end a row")
	assert.NotZero(t, rows[0].LastActivity)

	require.Equal(t, 1, fx.draw(conv, "n1", 2, 1))
	fx.requireAgents(agentID)
}

// Scenario: an agent coming back opens a session explicitly, closes it when
// done, and can only ever reach its own. The caller is the only session a
// route names, so another agent's close finds nothing and changes nothing.
func TestDrawingBoardSessionsOpenCloseAndOwnership(t *testing.T) {
	f, fx := drawingBoardSessionFlow(t)
	const a, b, reader = "drawing-board-session-a", "drawing-board-session-b", "drawing-board-session-reader"
	idA := drawingBoardAgent(t, f, a, "alpha")
	idB := drawingBoardAgent(t, f, b, "beta")
	f.HaveConvWithTitle(reader, "reader")
	_, _, err := db.EnsureAgentForConv(reader, "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(reader, agentd.PermDrawingBoardsRead, "test"))

	require.Equal(t, http.StatusForbidden, fx.agent(reader, http.MethodPost, "/v1/drawing-boards/plan/session", nil).Code, "open needs drawing-boards.write")
	require.Equal(t, http.StatusForbidden, fx.agent(reader, http.MethodDelete, "/v1/drawing-boards/plan/session", nil).Code, "close needs drawing-boards.write")
	require.Equal(t, http.StatusNotFound, fx.agent(a, http.MethodPost, "/v1/drawing-boards/nope/session", nil).Code)
	fx.requireAgents()

	rec := fx.agent(a, http.MethodPost, "/v1/drawing-boards/plan/session", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	b0 := decodeDrawingBoard(t, rec)
	assert.Equal(t, "Release plan", b0.Title)
	assert.Equal(t, []string{idA}, b0.Participants, "opening joins the drawing board, so its activity reaches the agent")
	fx.requireAgents(idA)

	rec = fx.agent(b, http.MethodDelete, "/v1/drawing-boards/plan/session", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no_session")
	fx.requireAgents(idA)

	require.Equal(t, http.StatusOK, fx.agent(b, http.MethodPost, "/v1/drawing-boards/plan/session", nil).Code)
	fx.requireAgents(idA, idB)

	require.Equal(t, http.StatusNoContent, fx.agent(b, http.MethodDelete, "/v1/drawing-boards/plan/session", nil).Code)
	fx.requireAgents(idA)
	require.Equal(t, http.StatusNoContent, fx.agent(a, http.MethodDelete, "/v1/drawing-boards/plan/session", nil).Code)
	fx.requireAgents()
	assert.Equal(t, http.StatusNotFound, fx.agent(a, http.MethodDelete, "/v1/drawing-boards/plan/session", nil).Code, "already closed")

	participants := decodeDrawingBoard(t, fx.operator(http.MethodGet, "/api/drawing-boards/plan", nil)).Participants
	assert.Contains(t, participants, idA, "closing a session is not leaving the drawing board")
}

// Scenario: the retire route ends the agent's sessions but not its peer's.
func TestDrawingBoardSessionsEndWhenTheAgentRetires(t *testing.T) {
	f, fx := drawingBoardSessionFlow(t)
	const stays, leaves = "drawing-board-session-stays", "drawing-board-session-leaves"
	stayID := drawingBoardAgent(t, f, stays, "stays")
	leaveID := drawingBoardAgent(t, f, leaves, "leaves")
	fx.draw(stays, "s", 1, 1)
	fx.draw(leaves, "l", 1, 1)
	fx.requireAgents(stayID, leaveID)

	code, _ := postRetire(t, fx.dash, leaves, "")
	require.Equal(t, http.StatusOK, code)
	fx.requireAgents(stayID)
}

// Scenario: deleting the drawing board takes its sessions with it, and the agent's
// session does not resurface on a new drawing board under the same id.
func TestDrawingBoardSessionsEndWhenTheDrawingBoardIsDeleted(t *testing.T) {
	f, fx := drawingBoardSessionFlow(t)
	const conv = "drawing-board-session-deleted"
	agentID := drawingBoardAgent(t, f, conv, "drawer")
	fx.draw(conv, "n1", 1, 1)
	fx.requireAgents(agentID)

	require.Equal(t, http.StatusNoContent, fx.operator(http.MethodDelete, "/api/drawing-boards/plan", nil).Code)
	fx.requireAgents()
	require.Equal(t, http.StatusCreated, fx.operator(http.MethodPost, "/api/drawing-boards", map[string]any{"id": "plan", "title": "Again"}).Code)
	fx.requireAgents()
}

// Scenario: a session is keyed by the agent, not a conversation or a process:
// it outlives a reincarnation and a daemon restart, and the listing names the
// current generation. Nothing about it depends on a dashboard being open.
func TestDrawingBoardSessionsSurviveReincarnationAndRestart(t *testing.T) {
	f, fx := drawingBoardSessionFlow(t)
	const conv, next = "drawing-board-session-gen1", "drawing-board-session-gen2"
	agentID := drawingBoardAgent(t, f, conv, "first")
	fx.draw(conv, "n1", 1, 1)

	f.HaveConvWithTitle(next, "second")
	_, err := db.RotateAgentConv(conv, next, "reincarnate")
	require.NoError(t, err)
	fx.requireAgents(agentID)
	assert.Equal(t, "second", fx.rows()[0].Agents[0].Title)

	db.Close()
	fx.requireAgents(agentID)

	require.Equal(t, http.StatusNoContent, fx.agent(next, http.MethodDelete, "/v1/drawing-boards/plan/session", nil).Code,
		"the successor owns the session")
	fx.requireAgents()
}

// Scenario: the operator ends a row from the menu. Every agent's session on the
// drawing board goes, the drawing board and its content stay, and the agents stay on it.
func TestDrawingBoardSessionsOperatorEndKeepsTheDrawingBoard(t *testing.T) {
	f, fx := drawingBoardSessionFlow(t)
	const a, b = "drawing-board-session-end-a", "drawing-board-session-end-b"
	idA := drawingBoardAgent(t, f, a, "alpha")
	idB := drawingBoardAgent(t, f, b, "beta")
	fx.draw(a, "n1", 1, 1)
	fx.draw(b, "n2", 1, 1)
	fx.requireAgents(idA, idB)

	rec := fx.operator(http.MethodDelete, "/api/drawing-boards/plan/sessions", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ended":2}`, rec.Body.String())
	fx.requireAgents()

	after := decodeDrawingBoard(t, fx.operator(http.MethodGet, "/api/drawing-boards/plan", nil))
	assert.Len(t, after.Elements, 2)
	assert.ElementsMatch(t, []string{idA, idB}, after.Participants)

	require.Equal(t, http.StatusNotFound, fx.operator(http.MethodDelete, "/api/drawing-boards/nope/sessions", nil).Code)

	require.Equal(t, 1, fx.draw(a, "n1", 2, 1))
	fx.requireAgents(idA)
}
