package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The dashboard's "+ New task" (tcl-kp5ngc): Write it myself is `add` with
// resolve_ref; Ask an agent is POST …/taskboard/ask.

// resolve_ref links a bare awb or Linear id through its tracker, as an import
// does, and keeps the title given; without it a bare id stays unlinked.
func TestTaskboardCreateResolvesRef(t *testing.T) {
	const member = "tb-new-member-1111"
	h := newTaskboardImportFlow(t, member)
	add := func(body map[string]any) tbTask {
		return h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", body), http.StatusCreated)
	}

	plain := add(map[string]any{"title": "my own title", "ref": "tcl-a1b2c3"})
	require.NotNil(t, plain.Ref)
	assert.Empty(t, plain.Ref.URL, "a single add without resolve_ref stores the id unlinked")

	resolved := add(map[string]any{"title": "my own title", "ref": "tcl-d4e5f6", "resolve_ref": true, "stage": "next",
		"body": "the brief", "after": []string{plain.ID}})
	assert.Equal(t, "my own title", resolved.Title, "the title is the caller's, not the tracker's")
	assert.Equal(t, "next", resolved.Stage)
	assert.Equal(t, []string{plain.ID}, resolved.DependsOn)
	require.NotNil(t, resolved.Ref)
	assert.Equal(t, "awb", resolved.Ref.Kind)
	assert.Equal(t, "https://awb.example/#/issues/tcl-d4e5f6", resolved.Ref.URL)

	lin := add(map[string]any{"title": "linear one", "ref": "LIN-7", "resolve_ref": true})
	require.NotNil(t, lin.Ref)
	assert.NotEmpty(t, lin.Ref.URL, "a Linear id takes the URL Linear returns")

	// A URL is kept as given.
	url := add(map[string]any{"title": "a link", "ref": "https://example.com/x", "resolve_ref": true})
	assert.Equal(t, "https://example.com/x", url.Ref.URL)

	// resolve_ref is for one task, not an import.
	rec := h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"refs": []string{"tcl-a1b2c3"}, "resolve_ref": true})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Ask mails a member a request to turn prose into tasks: inbox mail with the
// add command and the request, refused for a non-member, an empty request,
// and a member the writer lock keeps off the board.
func TestTaskboardAskMailsAMember(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	ask := func(conv string, body map[string]any) (int, string) {
		rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/ask", body)
		return rec.Code, rec.Body.String()
	}

	code, body := ask("", map[string]any{"to": h.worker, "prompt": "  the three follow-ups from the review  "})
	require.Equal(t, http.StatusOK, code, body)
	var out struct {
		To        string `json:"to"`
		MessageID int64  `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, tbAgentID(t, h.worker), out.To)
	assert.NotZero(t, out.MessageID)

	msgs, err := db.ListInboxForActor(h.worker, tbAgentID(t, h.worker), 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	m := msgs[0]
	assert.Equal(t, "[taskboard] task request from the operator: crew", m.Subject)
	assert.Contains(t, m.Body, "tclaude agent taskboard add crew")
	assert.Contains(t, m.Body, "do not start the work")
	assert.True(t, strings.HasSuffix(m.Body, "Request:\nthe three follow-ups from the review\n"), "body=%q", m.Body)

	// A member may ask another member: it may change the board itself.
	code, body = ask(h.peer, map[string]any{"to": h.worker, "prompt": "one more"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), "task request from "+h.peer)

	// The owner is not a member here, but it may change the board: it can be
	// asked (the dashboard picks it by default).
	code, body = ask("", map[string]any{"to": h.lead, "prompt": "plan the sprint"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.lead), "\n"), "task request from the operator")
	h.agent(t, "tb-outsider-9999")
	code, _ = ask("", map[string]any{"to": "tb-outsider-9999", "prompt": "not in the group"})
	assert.Equal(t, http.StatusBadRequest, code, "neither member nor owner")
	code, _ = ask("", map[string]any{"to": h.worker, "prompt": "   "})
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = ask("", map[string]any{"prompt": "who?"})
	assert.Equal(t, http.StatusBadRequest, code)

	// Lock the board to the worker: the peer may no longer be asked, and the
	// context says who may.
	rec := h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/writers", map[string]any{"agents": []string{h.worker}})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	code, body = ask("", map[string]any{"to": h.peer, "prompt": "locked out"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "taskboard_locked")
	code, body = ask("", map[string]any{"to": h.worker, "prompt": "still fine"})
	assert.Equal(t, http.StatusOK, code, body)

	rec = h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var ctx struct {
		Groups []struct {
			Group   string   `json:"group"`
			Locked  bool     `json:"locked"`
			Writers []string `json:"writers"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(t, ctx.Groups, 1)
	assert.True(t, ctx.Groups[0].Locked)
	assert.ElementsMatch(t, []string{tbAgentID(t, h.lead), tbAgentID(t, h.worker)}, ctx.Groups[0].Writers, "the owner and the listed writer")

	// A member the lock keeps off the board may not ask either.
	code, _ = ask(h.peer, map[string]any{"to": h.worker, "prompt": "from outside the lock"})
	assert.Equal(t, http.StatusForbidden, code)
}
