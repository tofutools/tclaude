package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Work-in-progress limits across linked nodes (docs/taskboard.md "Work-in-
// progress limits", "Across linked nodes"): a linked node's changes meet a
// board's limits as a local caller's do, and only a linked node's operator
// that may change the board's options goes over them.

// peerLimitsHarness is crew on this node with two members, and bob, a linked
// node, calling its board through its peer view.
type peerLimitsHarness struct {
	*peerProcessHarness
	view          http.Handler
	first, second string // the members' conv ids
}

func newPeerLimitsHarness(t *testing.T) *peerLimitsHarness {
	t.Helper()
	fh := newFedHarness(t)
	saveTaskboardConfig(t, nil)
	fh.f.HaveGroup("crew")
	h := &peerLimitsHarness{peerProcessHarness: &peerProcessHarness{fedHarness: fh},
		view: agentd.PeerViewHandler(fh.peer.id.ID()), first: "tb-peer-lim-first-1111", second: "tb-peer-lim-second-2222"}
	for _, conv := range []string{h.first, h.second} {
		fh.f.HaveConvWithTitle(conv, conv)
		_, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
		fh.f.HaveMember("crew", conv)
	}
	return h
}

// bob calls this node's board as bob's operator.
func (h *peerLimitsHarness) bob(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	rec := testharness.Serve(h.view, testharness.JSONRequest(t, method, path, body))
	return rec.Code, rec.Body.String()
}

// options sets the board's options as this node's operator.
func (h *peerLimitsHarness) options(t *testing.T, set map[string]string) {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodPatch, tbOptionsPath, map[string]any{"set": set})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// add adds a task in next as this node's operator.
func (h *peerLimitsHarness) add(t *testing.T, title string) string {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": title, "stage": "next"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out tbTask
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.ID
}

// show reads a task as bob does.
func (h *peerLimitsHarness) show(t *testing.T, id string) tbLimitTask {
	t.Helper()
	code, body := h.bob(t, http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	return tbDecodeLimit(t, code, body, http.StatusOK)
}

func (h *peerLimitsHarness) assign(t *testing.T, id, to string, extra map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"to": to}
	for k, v := range extra {
		body[k] = v
	}
	return h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+id+"/assign", body)
}

// A linked node's assign, start and add with an assignee into a full In
// progress queue by default; the queued place and reason read on its side; a
// slot its own close frees fills the line, the assignee briefed.
func TestPeerTaskboardLimitQueue(t *testing.T) {
	h := newPeerLimitsHarness(t)
	for _, slug := range []string{agentd.PermTaskboardWrite, agentd.PermTaskboardAssign} {
		h.grant(t, slug, "group=crew")
	}
	h.options(t, map[string]string{"limits.in_progress.max": "1"})

	holds := h.add(t, "holds the slot")
	code, body := h.assign(t, holds, h.first, nil)
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	assert.Nil(t, got.Limit)
	assert.True(t, got.started())

	// An assign queues: held in Next, first in line, told on bob's side.
	waits := h.add(t, "waits")
	code, body = h.assign(t, waits, h.second, nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit, body)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.Equal(t, 1, got.Limit.Position)
	assert.False(t, got.started())
	v := h.show(t, waits)
	assert.True(t, v.Queued)
	assert.Equal(t, "in_progress", v.QueuedFor)
	assert.Equal(t, 1, v.QueuedPosition)
	assert.Equal(t, "In progress is full, 1st in line", v.QueuedReason)
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.second), "queued for you, 1st in line for In progress"))

	// A start by hand only keeps it in line.
	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+waits+"/start", map[string]any{})
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit, body)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.False(t, got.started())

	// An add with an assignee queues behind it.
	code, body = h.bob(t, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "added for second", "stage": "next", "to": h.second})
	got = tbDecodeLimit(t, code, body, http.StatusCreated)
	require.NotNil(t, got.Limit, body)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.Equal(t, 2, got.Limit.Position)
	added := got.ID

	// bob closes the task in the slot: the first in line starts for its
	// assignee, with its brief, and the next moves up.
	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+holds+"/status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	v = h.show(t, waits)
	assert.True(t, v.started(), "first in line starts")
	assert.False(t, v.Queued)
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.second), "started for you: a slot freed in In progress"))
	assert.Equal(t, 1, h.show(t, added).QueuedPosition)
}

// A full column queues a linked node's move into it where the task is; its
// move out lets the first in line in.
func TestPeerTaskboardLimitColumn(t *testing.T) {
	h := newPeerLimitsHarness(t)
	for _, slug := range []string{agentd.PermTaskboardWrite, agentd.PermTaskboardAssign, agentd.PermTaskboardColumns} {
		h.grant(t, slug, "group=crew")
	}
	code, body := h.bob(t, http.MethodPost, "/v1/groups/crew/taskboard/columns", map[string]any{"label": "In review"})
	require.Equal(t, http.StatusCreated, code, body)
	h.options(t, map[string]string{"limits.columns.in-review.max": "1"})
	reviewed, waiting := h.add(t, "in review"), h.add(t, "waits for review")
	for _, id := range []string{reviewed, waiting} {
		code, body = h.assign(t, id, h.first, nil)
		require.Equal(t, http.StatusOK, code, body)
	}

	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+reviewed+"/stage", map[string]any{"stage": "in-review"})
	assert.Nil(t, tbDecodeLimit(t, code, body, http.StatusOK).Limit)
	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+waiting+"/stage", map[string]any{"stage": "in-review"})
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit, body)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.Empty(t, got.Column, "it stays in In progress")
	v := h.show(t, waiting)
	assert.Equal(t, "in-review", v.QueuedFor)
	assert.Equal(t, "In review is full, 1st in line", v.QueuedReason)

	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+reviewed+"/stage", map[string]any{"stage": "next"})
	require.Equal(t, http.StatusOK, code, body)
	v = h.show(t, waiting)
	assert.Equal(t, "in-review", v.Column, "the slot bob freed lets the line in")
	assert.Empty(t, v.QueuedFor)
}

// Under refuse a linked node's change is refused; under warn it goes in over
// the limit. Going over takes the linked node's operator and its say over
// the board's options (taskboard.options.write, or taskboard.manage); the
// refusal offers it (the dashboard's confirm) exactly then. An agent of the
// linked node follows the limit whatever its node holds.
func TestPeerTaskboardLimitRefuseWarnAndOver(t *testing.T) {
	h := newPeerLimitsHarness(t)
	for _, slug := range []string{agentd.PermTaskboardWrite, agentd.PermTaskboardAssign} {
		h.grant(t, slug, "group=crew")
	}
	h.options(t, map[string]string{"limits.in_progress.max": "1", "limits.at_limit": "refuse", "events.limit_reached.to": "owners"})
	// first, an owner, hears of the board's full lanes.
	crew, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(crew.ID, h.first, "test"))
	code, body := h.assign(t, h.add(t, "holds the slot"), h.first, nil)
	require.Equal(t, http.StatusOK, code, body)

	task := h.add(t, "refused")
	code, body = h.assign(t, task, h.second, nil)
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "limit_reached")
	assert.NotContains(t, body, "over_limit", "no confirm offered to a node that may not go over")
	code, body = h.assign(t, task, h.second, map[string]any{"over_limit": true})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "only an operator")
	assert.Nil(t, h.show(t, task).Assignee, "refused, nothing changed")

	// Its agent follows the limit, even once its node may go over.
	h.grant(t, agentd.PermTaskboardOptionsWrite, "group=crew")
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker"}
	reply := agentPeerViewExchange(t, h.peer, worker, http.MethodPost, "/v1/taskboard/tasks/"+task+"/assign",
		map[string]any{"to": h.second, "over_limit": true})
	assert.Equal(t, http.StatusForbidden, reply.Status, string(reply.Body))
	assert.Contains(t, string(reply.Body), "an agent follows it")
	reply = agentPeerViewExchange(t, h.peer, worker, http.MethodPost, "/v1/taskboard/tasks/"+task+"/assign",
		map[string]any{"to": h.second})
	assert.Equal(t, http.StatusConflict, reply.Status, string(reply.Body))
	assert.NotContains(t, string(reply.Body), "over_limit", "no confirm offered to an agent")

	// With taskboard.options.write, its operator is offered the confirm and
	// goes over.
	code, body = h.assign(t, task, h.second, nil)
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "(go over it with over_limit)")
	code, body = h.assign(t, task, h.second, map[string]any{"over_limit": true})
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit, body)
	assert.Equal(t, "over", got.Limit.Action)
	assert.True(t, got.started())
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.first), "let in over the limit by peer: the operator of"))

	// taskboard.manage covers it too.
	h.revoke(t, agentd.PermTaskboardOptionsWrite, "group=crew")
	h.grant(t, agentd.PermTaskboardManage, "group=crew")
	more := h.add(t, "managed over")
	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+more+"/assign", map[string]any{"to": h.first, "over_limit": true})
	assert.Equal(t, "over", tbDecodeLimit(t, code, body, http.StatusOK).Limit.Action)

	// warn: in, over the limit, and the answer says so.
	h.options(t, map[string]string{"limits.at_limit": "warn"})
	warned := h.add(t, "warned")
	code, body = h.assign(t, warned, h.first, nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit, body)
	assert.Equal(t, "warned", got.Limit.Action)
	assert.True(t, got.started())
}

// Unrestricted trust acts as this node's operator: its operator goes over a
// limit; its agent still follows it.
func TestPeerTaskboardLimitUnrestricted(t *testing.T) {
	h := newPeerLimitsHarness(t)
	h.trustUnrestricted(t)
	h.options(t, map[string]string{"limits.in_progress.max": "1", "limits.at_limit": "refuse"})
	code, body := h.assign(t, h.add(t, "holds the slot"), h.first, nil)
	require.Equal(t, http.StatusOK, code, body)

	task := h.add(t, "over")
	code, body = h.assign(t, task, h.second, nil)
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "(go over it with over_limit)")
	code, body = h.assign(t, task, h.second, map[string]any{"over_limit": true})
	assert.Equal(t, "over", tbDecodeLimit(t, code, body, http.StatusOK).Limit.Action)

	// Its agent is served under the peer grants, not as the operator: with
	// the assign grant (and the say over the options), only the limit
	// refuses it.
	for _, slug := range []string{agentd.PermTaskboardAssign, agentd.PermTaskboardOptionsWrite} {
		h.grant(t, slug, "group=crew")
	}
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker"}
	reply := agentPeerViewExchange(t, h.peer, worker, http.MethodPost, "/v1/taskboard/tasks/"+h.add(t, "agent's")+"/assign",
		map[string]any{"to": h.first, "over_limit": true})
	assert.Equal(t, http.StatusForbidden, reply.Status, string(reply.Body))
	assert.Contains(t, string(reply.Body), "an agent follows it")
}

// The CLI's --over-limit with --peer, end to end through this node's sender
// path and bob's receiving side (as TestPeerTaskboardCLI): refused while
// this node may not change bob's board options, let in once it may.
func TestPeerTaskboardLimitCLIOverLimit(t *testing.T) {
	h := newPeerLimitsHarness(t)
	for _, slug := range []string{agentd.PermTaskboardWrite, agentd.PermTaskboardAssign} {
		h.grant(t, slug, "group=crew")
	}
	h.options(t, map[string]string{"limits.in_progress.max": "1", "limits.at_limit": "refuse"})
	code, body := h.assign(t, h.add(t, "holds the slot"), h.first, nil)
	require.Equal(t, http.StatusOK, code, body)
	task := h.add(t, "over by CLI")
	bridgeAgentClientToMux(t, h.f.Mux)
	opened := 0
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		done := make(chan int, 1)
		go func() { done <- agent.RunTaskboardCLI(args, stdout, stderr) }()
		conn := acceptOutboundPeerView(t, h.peer, opened)
		opened++
		wire := readPeerViewWire(t, conn)
		req := httptest.NewRequest(wire.Method, wire.URI, bytes.NewReader(wire.Body))
		for k, v := range wire.Header {
			req.Header[k] = v
		}
		rec := testharness.Serve(h.view, req)
		writePeerViewWire(t, conn, peerViewWire{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
		rc := <-done
		_ = conn.Close()
		return rc, stdout.String(), stderr.String()
	}

	rc, out, errOut := run("assign", task, "--to", h.second, "--peer", "bob")
	assert.NotEqual(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, errOut, "In progress is full")
	rc, out, errOut = run("assign", task, "--to", h.second, "--peer", "bob", "--over-limit")
	assert.NotEqual(t, 0, rc, "no say over bob's options: out=%s err=%s", out, errOut)
	assert.Contains(t, errOut, "only an operator")

	h.grant(t, agentd.PermTaskboardOptionsWrite, "group=crew")
	rc, out, errOut = run("assign", task, "--to", h.second, "--peer", "bob", "--over-limit")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "over the limit")
	assert.True(t, h.show(t, task).started())
}

// A linked node's move of a started task to another group of this node meets
// the target's In progress: a full one queues it there, and the slot the
// target frees lets it in.
func TestPeerTaskboardLimitMoveToGroup(t *testing.T) {
	h := newPeerLimitsHarness(t)
	h.f.HaveGroup("ops")
	h.f.HaveMember("ops", h.first)
	for _, grant := range [][2]string{{agentd.PermTaskboardAssign, "group=crew"}, {agentd.PermTaskboardManage, "group=crew"},
		{agentd.PermTaskboardTransfer, "group=ops"}, {agentd.PermTaskboardWrite, "group=ops"}, {agentd.PermTaskboardAssign, "group=ops"}} {
		h.grant(t, grant[0], grant[1])
	}
	rec := fedHuman(t, h.f, http.MethodPatch, "/v1/groups/ops/taskboard/options",
		map[string]any{"set": map[string]string{"limits.in_progress.max": "1"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = fedHuman(t, h.f, http.MethodPost, "/v1/groups/ops/taskboard/tasks", map[string]any{"title": "ops work", "to": h.first, "stage": "next"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var opsWork tbTask
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &opsWork))

	moving := h.add(t, "moves to ops")
	code, body := h.assign(t, moving, h.first, nil)
	require.True(t, tbDecodeLimit(t, code, body, http.StatusOK).started(), body)
	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+moving+"/move-to-group", map[string]any{"group": "ops"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"action":"queued"`)
	v := h.show(t, moving)
	assert.False(t, v.started())
	assert.Equal(t, "in_progress", v.QueuedFor)
	assert.Equal(t, 1, v.QueuedPosition)

	code, body = h.bob(t, http.MethodPost, "/v1/taskboard/tasks/"+opsWork.ID+"/status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, h.show(t, moving).started(), "the slot bob freed in ops lets it in")
}
