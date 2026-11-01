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

type actionView struct {
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Detail      string `json:"detail"`
	Note        string `json:"note"`
	Unavailable string `json:"unavailable"`
}

type actionFlow struct {
	t      *testing.T
	f      *testharness.Flow
	dash   http.Handler
	sender string
	dir    string
}

func newActionFlow(t *testing.T, sender string) *actionFlow {
	f := newFlow(t)
	enableCollab(t)
	f.HaveGroup("design")
	f.HaveMember("design", sender)
	dir := f.TestCwd("work")
	f.HaveAliveSession(sender, "hact", "tclaude-hact", dir)
	require.NoError(t, db.GrantAgentPermission(sender, agentd.PermHumanNotify, "test"))
	return &actionFlow{t: t, f: f, dash: dashMessageMux(t), sender: sender, dir: dir}
}

func (a *actionFlow) notify(body map[string]any) *httptest.ResponseRecorder {
	r := agentd.AsAgentPeer(testharness.JSONRequest(a.t, http.MethodPost, "/v1/notify-human", body), a.sender)
	return testharness.Serve(a.f.Mux, r)
}

func (a *actionFlow) send(action string) int64 {
	rec := a.notify(map[string]any{"body": "look at this", "action": action})
	require.Equal(a.t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(a.t, json.Unmarshal(rec.Body.Bytes(), &created))
	return created.ID
}

func (a *actionFlow) view(id int64) *actionView {
	snap := testharness.Serve(a.dash, testharness.JSONRequest(a.t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(a.t, http.StatusOK, snap.Code)
	var s struct {
		Messages []struct {
			ID     int64       `json:"id"`
			Action *actionView `json:"action"`
		} `json:"messages"`
	}
	require.NoError(a.t, json.Unmarshal(snap.Body.Bytes(), &s))
	for _, m := range s.Messages {
		if m.ID == id {
			return m.Action
		}
	}
	require.Fail(a.t, "the message is not in the snapshot")
	return nil
}

func (a *actionFlow) click(body map[string]any) *httptest.ResponseRecorder {
	return testharness.Serve(a.dash, testharness.JSONRequest(a.t, http.MethodPost, "/api/human-messages/action", body))
}

// An agent can only name an action from the list, and only one that is its own
// to name. A path or a URL of its choosing never gets through.
func TestHumanAction_RefusesAnythingOffTheList(t *testing.T) {
	a := newActionFlow(t, "hact-aaaa-bbbb-cccc-000000000001")
	for _, bad := range []map[string]any{
		{"body": "x", "action": "shell"},
		{"body": "x", "action": "terminal:/etc"},
		{"body": "x", "action": "tab:evil"},
		{"body": "x", "action": "tab:https://example.com"},
		{"body": "x", "action": "board:none"},
		{"body": "x", "action": "pr:https://github.com/o/r/pull/1"},
		{"body": "x", "action": "url:https://example.com"},
		{"body": "x", "action": "tab:usage", "options": []string{"a", "b"}},
	} {
		rec := a.notify(bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v → %s", bad, rec.Body.String())
	}
}

// The terminal link: the daemon names the directory and the sandbox on the
// button, and a click gets the sender's own agent, whatever the request says.
func TestHumanAction_TerminalIsTheSendersOwnDirectory(t *testing.T) {
	a := newActionFlow(t, "hact-aaaa-bbbb-cccc-000000000002")
	id := a.send("terminal")
	view := a.view(id)
	require.NotNil(t, view)
	assert.Equal(t, "terminal", view.Kind)
	assert.Equal(t, a.dir, view.Detail)
	assert.Contains(t, view.Note, "outside the agent's sandbox")
	assert.Empty(t, view.Unavailable)

	row, err := db.GetHumanMessage(id)
	require.NoError(t, err)
	assert.False(t, row.IsRead(), "nothing happens until the operator clicks")

	rec := a.click(map[string]any{"id": id, "agent": "someone-else", "path": "/etc", "kind": "tab"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var target struct {
		Kind  string `json:"kind"`
		Agent string `json:"agent"`
		Group string `json:"group"`
		Path  string `json:"path"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &target))
	assert.Equal(t, "terminal", target.Kind)
	assert.Equal(t, row.FromAgent, target.Agent)
	assert.Equal(t, "design", target.Group)
	assert.Empty(t, target.Path)

	row, err = db.GetHumanMessage(id)
	require.NoError(t, err)
	assert.True(t, row.IsRead(), "the click reads the message")
}

func TestHumanAction_BoardPRAndTabResolve(t *testing.T) {
	a := newActionFlow(t, "hact-aaaa-bbbb-cccc-000000000003")
	board, err := db.CreateCollabBoard("brd_actionflow", "Launch plan", "test", 10)
	require.NoError(t, err)
	agentID, err := db.AgentIDForConv(a.sender)
	require.NoError(t, err)
	const prURL = "https://github.com/o/r/pull/42"
	_, err = db.UpsertAgentPR(agentID, prURL, "", "open")
	require.NoError(t, err)

	for _, tc := range []struct {
		action, label, field, want string
	}{
		{"board:" + board.ID, "Open board Launch plan", "board", board.ID},
		{"pr:" + prURL, "Open PR #42", "url", prURL},
		{"tab:processes", "Go to Processes", "path", "/automations/processes"},
	} {
		id := a.send(tc.action)
		view := a.view(id)
		require.NotNil(t, view, tc.action)
		assert.Equal(t, tc.label, view.Label)
		rec := a.click(map[string]any{"id": id})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var target map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &target))
		assert.Equal(t, tc.want, target[tc.field], tc.action)
	}
}

// A target that went away turns the button off and refuses the click.
func TestHumanAction_GoneTargetIsUnavailable(t *testing.T) {
	a := newActionFlow(t, "hact-aaaa-bbbb-cccc-000000000004")
	board, err := db.CreateCollabBoard("brd_gone", "Gone", "test", 10)
	require.NoError(t, err)
	id := a.send("board:" + board.ID)
	require.NoError(t, db.DeleteCollabBoard(board.ID))
	assert.NotEmpty(t, a.view(id).Unavailable)
	rec := a.click(map[string]any{"id": id})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	row, err := db.GetHumanMessage(id)
	require.NoError(t, err)
	assert.False(t, row.IsRead())

	plain, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: a.sender, Body: "no action"})
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, a.click(map[string]any{"id": plain}).Code)
	require.Equal(t, http.StatusNotFound, a.click(map[string]any{"id": plain + 1000}).Code)
}

func TestHumanAction_HiddenWithCollabOff(t *testing.T) {
	a := newActionFlow(t, "hact-aaaa-bbbb-cccc-000000000005")
	id := a.send("tab:usage")
	require.NotNil(t, a.view(id))

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))

	assert.Nil(t, a.view(id))
	rec := a.click(map[string]any{"id": id})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "collab_disabled")
	refused := a.notify(map[string]any{"body": "x", "action": "tab:usage"})
	require.Equal(t, http.StatusNotFound, refused.Code, refused.Body.String())
}

// The Messages tab gets the button from the mailbox route; a peer reading that
// mailbox does not.
func TestHumanAction_InTheMailboxButNotForAPeer(t *testing.T) {
	a := newActionFlow(t, "hact-aaaa-bbbb-cccc-000000000006")
	id := a.send("terminal")
	type page struct {
		Messages []struct {
			ID     int64       `json:"id"`
			Action *actionView `json:"action"`
		} `json:"messages"`
	}
	rec := testharness.Serve(a.dash, testharness.JSONRequest(t, http.MethodGet, "/api/mailbox?id=human", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var local page
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &local))
	require.Len(t, local.Messages, 1)
	require.Equal(t, id, local.Messages[0].ID)
	require.NotNil(t, local.Messages[0].Action)
	assert.Equal(t, a.dir, local.Messages[0].Action.Detail)

	var peer page
	rec = agentd.ServePeerMailboxForTest(testharness.JSONRequest(t, http.MethodGet, "/api/mailbox?id=human", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &peer))
	require.Len(t, peer.Messages, 1)
	assert.Nil(t, peer.Messages[0].Action)
}
