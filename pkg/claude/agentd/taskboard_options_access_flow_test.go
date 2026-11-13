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
)

// Board options have slugs of their own (docs/taskboard.md "Who may read and
// change them"): a member reads its own group's, an owner changes them, and
// anyone else needs taskboard.options.read / .write, scoped by group. The
// task slugs reach neither.

// optionsCode answers conv's options request on group's board.
func (h *tbAssignHarness) optionsCode(conv, group, method string, body any) (int, string) {
	h.t.Helper()
	rec := h.call(conv, method, "/v1/groups/"+group+"/taskboard/options", body)
	return rec.Code, rec.Body.String()
}

func tbGrant(t *testing.T, conv, slug, group string) {
	t.Helper()
	scope := ""
	if group != "" {
		scope = `{"group":["` + group + `"]}`
	}
	require.NoError(t, db.GrantAgentPermissionWithScope(conv, slug, scope, "test"))
}

var tbOptionsOff = map[string]any{"set": map[string]string{"idle.enabled": "off"}}

func TestTaskboardOptionsGates(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	h.f.HaveGroup("web")
	const outsider, other = "tb-out-4444", "tb-other-5555"
	h.agent(t, outsider)
	h.agent(t, other)

	// A member reads its own group's options; it does not change them.
	code, body := h.optionsCode(h.worker, "crew", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, code, "a member reads its own board's options: %s", body)
	code, body = h.optionsCode(h.worker, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "a member does not change them by membership: %s", body)
	assert.Contains(t, body, agentd.PermTaskboardOptionsWrite)

	// Nor under taskboard.write, which is about tasks.
	tbGrant(t, h.worker, agentd.PermTaskboardWrite, "crew")
	code, body = h.optionsCode(h.worker, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "taskboard.write alone does not change options: %s", body)

	// An owner does; so does the operator.
	code, body = h.optionsCode(h.lead, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusOK, code, "an owner changes its board's options: %s", body)
	code, body = h.optionsCode("", "crew", http.MethodPatch, map[string]any{"reset": []string{"all"}})
	require.Equal(t, http.StatusOK, code, "the operator passes: %s", body)

	// taskboard.options.own.write: its own group's board, never another,
	// whatever the grant's scope.
	tbGrant(t, h.worker, agentd.PermTaskboardOptionsOwnWrite, "")
	code, body = h.optionsCode(h.worker, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusOK, code, "own.write changes its own board: %s", body)
	code, body = h.optionsCode(h.worker, "web", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "own.write never reaches a group it is not a member of: %s", body)
	code, body = h.optionsCode(h.worker, "web", http.MethodGet, nil)
	require.Equal(t, http.StatusForbidden, code, "nor does own.read: %s", body)
	tbGrant(t, outsider, agentd.PermTaskboardOptionsOwnWrite, "")
	code, body = h.optionsCode(outsider, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "a non-member's own.write reaches no board: %s", body)

	// taskboard.options.read / .write scoped to a group reach that group only.
	code, body = h.optionsCode(other, "crew", http.MethodGet, nil)
	require.Equal(t, http.StatusForbidden, code, body)
	tbGrant(t, other, agentd.PermTaskboardOptionsRead, "crew")
	code, body = h.optionsCode(other, "crew", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, code, "a scoped read reads that board: %s", body)
	code, body = h.optionsCode(other, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "read does not write: %s", body)
	code, body = h.optionsCode(other, "web", http.MethodGet, nil)
	require.Equal(t, http.StatusForbidden, code, "nor another group's: %s", body)
	tbGrant(t, other, agentd.PermTaskboardOptionsWrite, "crew")
	code, body = h.optionsCode(other, "crew", http.MethodPatch, map[string]any{"set": map[string]string{"limits.in_progress.max": "3"}})
	require.Equal(t, http.StatusOK, code, "a scoped write changes that board: %s", body)
	code, body = h.optionsCode(other, "web", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "and no other: %s", body)

	// taskboard.manage manages the board as a whole, its options included;
	// options.write implies options.read.
	const manager, writer = "tb-mgr-6666", "tb-wr-7777"
	h.agent(t, manager)
	h.agent(t, writer)
	tbGrant(t, manager, agentd.PermTaskboardManage, "web")
	code, body = h.optionsCode(manager, "web", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusOK, code, "taskboard.manage changes the options: %s", body)
	tbGrant(t, writer, agentd.PermTaskboardOptionsWrite, "web")
	code, body = h.optionsCode(writer, "web", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, code, "write implies read: %s", body)

	// An explicit deny of own.read stops a member's read.
	require.NoError(t, db.SetAgentPermissionOverride(h.peer, agentd.PermTaskboardOptionsOwnRead, "deny", "test"))
	code, body = h.optionsCode(h.peer, "crew", http.MethodGet, nil)
	require.Equal(t, http.StatusForbidden, code, "a denied own.read reads nothing: %s", body)
}

// The writer lock treats a change of the options as any change of the
// board: a grant whose scope the group satisfies (here a group grant of
// own.write) passes it, as do the owners and the listed writers.
func TestTaskboardOptionsWriterLock(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	rec := h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/writers", map[string]any{"agents": []string{h.peer}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	code, body := h.optionsCode(h.worker, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusForbidden, code, "no options grant: %s", body)
	setGroupGrant(t, func(method, path string, body any) *httptest.ResponseRecorder { return h.call("", method, path, body) },
		"crew", agentd.PermTaskboardOptionsOwnWrite, map[string][]string{"group": {"crew"}})
	code, body = h.optionsCode(h.worker, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusOK, code, "a group grant scoped to the group passes the lock: %s", body)
	code, body = h.optionsCode(h.peer, "crew", http.MethodPatch, map[string]any{"reset": []string{"all"}})
	require.Equal(t, http.StatusOK, code, "a listed writer passes: %s", body)
	code, body = h.optionsCode(h.lead, "crew", http.MethodPatch, tbOptionsOff)
	require.Equal(t, http.StatusOK, code, "an owner passes: %s", body)
}

// The board context carries the options summary only for a caller that
// reads the options; a task's own queue facts read with the board.
func TestTaskboardContextOptionsSummaryGated(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	const outsider = "tb-out-4444"
	h.agent(t, outsider)
	tbGrant(t, outsider, agentd.PermTaskboardRead, "crew")
	h.setOptions("", map[string]string{"limits.in_progress.max": "1"})
	first := h.add("first", "next")
	second := h.add("second", "next")
	for _, task := range []tbTask{first, second} {
		code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
		require.Equal(t, http.StatusOK, code, body)
	}

	read := func(conv string) (bool, map[string]any) {
		t.Helper()
		rec := h.call(conv, http.MethodGet, "/v1/taskboard/context", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var c struct {
			Groups []struct {
				Group   string           `json:"group"`
				Options *json.RawMessage `json:"options"`
				Tasks   []map[string]any `json:"tasks"`
			} `json:"groups"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &c))
		for _, g := range c.Groups {
			if g.Group != "crew" {
				continue
			}
			for _, task := range g.Tasks {
				if task["id"] == second.ID {
					return g.Options != nil, task
				}
			}
			t.Fatalf("no second task in the context: %s", rec.Body.String())
		}
		t.Fatalf("no crew board in the context: %s", rec.Body.String())
		return false, nil
	}
	summary, task := read(h.worker)
	assert.True(t, summary, "a member reads its own board's options")
	assert.Equal(t, true, task["queued"], "the second task waits for the lane")
	summary, task = read(outsider)
	assert.False(t, summary, "a reader of tasks only gets no options summary")
	assert.Equal(t, true, task["queued"], "but still sees that a task waits for a slot")
	assert.NotEmpty(t, task["queued_reason"])
}
