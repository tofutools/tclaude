package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type boardSessionRow struct {
	Kind         string `json:"kind"`
	Key          string `json:"key"`
	Board        string `json:"board"`
	Label        string `json:"label"`
	Killable     bool   `json:"killable"`
	LastActivity int64  `json:"last_activity"`
	Agents       []struct {
		AgentID string `json:"agent_id"`
		Title   string `json:"title"`
	} `json:"agents"`
}

type boardSessionFixture struct {
	t    *testing.T
	mux  http.Handler
	dash http.Handler
}

// boardSessionFlow is a collab-enabled daemon with one board, "plan".
func boardSessionFlow(t *testing.T) (*testharness.Flow, boardSessionFixture) {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	enableCollab(t)
	fx := boardSessionFixture{t: t, mux: f.Mux, dash: agentd.BuildDashboardHandlerForTest()}
	require.Equal(t, http.StatusCreated, fx.operator(http.MethodPost, "/api/boards", map[string]any{"id": "plan", "title": "Release plan"}).Code)
	return f, fx
}

func (fx boardSessionFixture) operator(method, path string, body any) *httptest.ResponseRecorder {
	fx.t.Helper()
	return testharness.Serve(fx.dash, boardDashReq(fx.t, method, path, body))
}

func (fx boardSessionFixture) agent(conv, method, path string, body any) *httptest.ResponseRecorder {
	fx.t.Helper()
	return testharness.Serve(fx.mux, agentd.AsAgentPeer(testharness.JSONRequest(fx.t, method, path, body), conv))
}

// boardAgent adds an agent holding boards.write and returns its id.
func boardAgent(t *testing.T, f *testharness.Flow, conv, title string) string {
	t.Helper()
	f.HaveConvWithTitle(conv, title)
	id, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsWrite, "test"))
	return id
}

// rows reads the board rows of the operator's Background terminals listing.
func (fx boardSessionFixture) rows() []boardSessionRow {
	fx.t.Helper()
	rec := fx.operator(http.MethodGet, "/api/background-terminals", nil)
	require.Equal(fx.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Terminals []boardSessionRow `json:"terminals"`
	}
	require.NoError(fx.t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	out := []boardSessionRow{}
	for _, row := range resp.Terminals {
		if row.Kind == "board" {
			out = append(out, row)
		}
	}
	return out
}

func (fx boardSessionFixture) requireAgents(agentIDs ...string) {
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

func (fx boardSessionFixture) draw(conv, id string, version, nonce int64) int {
	fx.t.Helper()
	rec := fx.agent(conv, http.MethodPatch, "/v1/boards/plan", map[string]any{"elements": []any{rect(id, version, nonce, 0)}})
	require.Equal(fx.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Accepted int `json:"accepted"`
	}
	require.NoError(fx.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Accepted
}

// Scenario: an agent's accepted edit starts a board session and later ones
// refresh it. The operator's edits start none, and neither does an edit that
// changed nothing.
func TestBoardSessionsStartOnAcceptedAgentEdit(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const conv = "board-session-edit"
	agentID := boardAgent(t, f, conv, "drawer")

	rec := fx.operator(http.MethodPatch, "/api/boards/plan", map[string]any{"elements": []any{rect("op", 1, 1, 0)}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fx.requireAgents()

	require.Zero(t, fx.draw(conv, "op", 1, 2), "a losing patch is not an edit")
	fx.requireAgents()

	require.Equal(t, 1, fx.draw(conv, "n1", 1, 1))
	fx.requireAgents(agentID)
	rows := fx.rows()
	assert.Equal(t, "board:plan", rows[0].Key, "the row carries the board's pane key")
	assert.Equal(t, "plan", rows[0].Board)
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
func TestBoardSessionsOpenCloseAndOwnership(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const a, b, reader = "board-session-a", "board-session-b", "board-session-reader"
	idA := boardAgent(t, f, a, "alpha")
	idB := boardAgent(t, f, b, "beta")
	f.HaveConvWithTitle(reader, "reader")
	_, _, err := db.EnsureAgentForConv(reader, "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(reader, agentd.PermBoardsRead, "test"))

	require.Equal(t, http.StatusForbidden, fx.agent(reader, http.MethodPost, "/v1/boards/plan/session", nil).Code, "open needs boards.write")
	require.Equal(t, http.StatusForbidden, fx.agent(reader, http.MethodDelete, "/v1/boards/plan/session", nil).Code, "close needs boards.write")
	require.Equal(t, http.StatusNotFound, fx.agent(a, http.MethodPost, "/v1/boards/nope/session", nil).Code)
	fx.requireAgents()

	rec := fx.agent(a, http.MethodPost, "/v1/boards/plan/session", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	b0 := decodeBoard(t, rec)
	assert.Equal(t, "Release plan", b0.Title)
	assert.Equal(t, []string{idA}, b0.Participants, "opening joins the board, so its activity reaches the agent")
	fx.requireAgents(idA)

	rec = fx.agent(b, http.MethodDelete, "/v1/boards/plan/session", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no_session")
	fx.requireAgents(idA)

	require.Equal(t, http.StatusOK, fx.agent(b, http.MethodPost, "/v1/boards/plan/session", nil).Code)
	fx.requireAgents(idA, idB)

	require.Equal(t, http.StatusNoContent, fx.agent(b, http.MethodDelete, "/v1/boards/plan/session", nil).Code)
	fx.requireAgents(idA)
	require.Equal(t, http.StatusNoContent, fx.agent(a, http.MethodDelete, "/v1/boards/plan/session", nil).Code)
	fx.requireAgents()
	assert.Equal(t, http.StatusNotFound, fx.agent(a, http.MethodDelete, "/v1/boards/plan/session", nil).Code, "already closed")

	participants := decodeBoard(t, fx.operator(http.MethodGet, "/api/boards/plan", nil)).Participants
	assert.Contains(t, participants, idA, "closing a session is not leaving the board")
}

// Scenario: the retire route ends the agent's sessions but not its peer's.
func TestBoardSessionsEndWhenTheAgentRetires(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const stays, leaves = "board-session-stays", "board-session-leaves"
	stayID := boardAgent(t, f, stays, "stays")
	leaveID := boardAgent(t, f, leaves, "leaves")
	fx.draw(stays, "s", 1, 1)
	fx.draw(leaves, "l", 1, 1)
	fx.requireAgents(stayID, leaveID)

	code, _ := postRetire(t, fx.dash, leaves, "")
	require.Equal(t, http.StatusOK, code)
	fx.requireAgents(stayID)
}

// Scenario: deleting the board takes its sessions with it, and the agent's
// session does not resurface on a new board under the same id.
func TestBoardSessionsEndWhenTheBoardIsDeleted(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const conv = "board-session-deleted"
	agentID := boardAgent(t, f, conv, "drawer")
	fx.draw(conv, "n1", 1, 1)
	fx.requireAgents(agentID)

	require.Equal(t, http.StatusNoContent, fx.operator(http.MethodDelete, "/api/boards/plan", nil).Code)
	fx.requireAgents()
	require.Equal(t, http.StatusCreated, fx.operator(http.MethodPost, "/api/boards", map[string]any{"id": "plan", "title": "Again"}).Code)
	fx.requireAgents()
}

// Scenario: a session is keyed by the agent, not a conversation or a process:
// it outlives a reincarnation and a daemon restart, and the listing names the
// current generation. Nothing about it depends on a dashboard being open.
func TestBoardSessionsSurviveReincarnationAndRestart(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const conv, next = "board-session-gen1", "board-session-gen2"
	agentID := boardAgent(t, f, conv, "first")
	fx.draw(conv, "n1", 1, 1)

	f.HaveConvWithTitle(next, "second")
	_, err := db.RotateAgentConv(conv, next, "reincarnate")
	require.NoError(t, err)
	fx.requireAgents(agentID)
	assert.Equal(t, "second", fx.rows()[0].Agents[0].Title)

	db.Close()
	fx.requireAgents(agentID)

	require.Equal(t, http.StatusNoContent, fx.agent(next, http.MethodDelete, "/v1/boards/plan/session", nil).Code,
		"the successor owns the session")
	fx.requireAgents()
}

// Scenario: the operator ends a row from the menu. Every agent's session on the
// board goes, the board and its content stay, and the agents stay on it.
func TestBoardSessionsOperatorEndKeepsTheBoard(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const a, b = "board-session-end-a", "board-session-end-b"
	idA := boardAgent(t, f, a, "alpha")
	idB := boardAgent(t, f, b, "beta")
	fx.draw(a, "n1", 1, 1)
	fx.draw(b, "n2", 1, 1)
	fx.requireAgents(idA, idB)

	rec := fx.operator(http.MethodDelete, "/api/boards/plan/sessions", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ended":2}`, rec.Body.String())
	fx.requireAgents()

	after := decodeBoard(t, fx.operator(http.MethodGet, "/api/boards/plan", nil))
	assert.Len(t, after.Elements, 2)
	assert.ElementsMatch(t, []string{idA, idB}, after.Participants)

	require.Equal(t, http.StatusNotFound, fx.operator(http.MethodDelete, "/api/boards/nope/sessions", nil).Code)

	require.Equal(t, 1, fx.draw(a, "n1", 2, 1))
	fx.requireAgents(idA)
}

// Scenario: with Collab off the rows and every session route are absent, while
// the sessions themselves wait for it to come back on.
func TestBoardSessionsAreHiddenWithCollabOff(t *testing.T) {
	f, fx := boardSessionFlow(t)
	const conv = "board-session-off"
	agentID := boardAgent(t, f, conv, "drawer")
	fx.draw(conv, "n1", 1, 1)
	fx.requireAgents(agentID)

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))

	fx.requireAgents()
	require.Equal(t, http.StatusNotFound, fx.agent(conv, http.MethodPost, "/v1/boards/plan/session", nil).Code)
	require.Equal(t, http.StatusNotFound, fx.agent(conv, http.MethodDelete, "/v1/boards/plan/session", nil).Code)
	require.Equal(t, http.StatusNotFound, fx.operator(http.MethodDelete, "/api/boards/plan/sessions", nil).Code)

	enableCollab(t)
	fx.requireAgents(agentID)
}
