package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type tbTask struct {
	ID          string   `json:"id"`
	Group       string   `json:"group"`
	Title       string   `json:"title"`
	Stage       string   `json:"stage"`
	Status      string   `json:"status"`
	BlockReason string   `json:"block_reason"`
	DependsOn   []string `json:"depends_on"`
	Queued      bool     `json:"queued"`
	CreatedBy   string   `json:"created_by"`
	Ref         *struct {
		Kind  string `json:"kind"`
		URL   string `json:"url"`
		Label string `json:"label"`
	} `json:"ref"`
}

type taskboardHarness struct {
	t *testing.T
	f *testharness.Flow
}

func newTaskboardFlow(t *testing.T) *taskboardHarness {
	f := newFlow(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Taskboard: new(true)}}))
	return &taskboardHarness{t: t, f: f}
}

func (h *taskboardHarness) agent(t *testing.T, conv string) {
	h.f.HaveConvWithTitle(conv, conv)
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
}

// call sends a request as conv ("" for the operator).
func (h *taskboardHarness) call(conv, method, path string, body any) *httptest.ResponseRecorder {
	req := testharness.JSONRequest(h.t, method, path, body)
	if conv == "" {
		req = agentd.AsHumanPeer(req)
	} else {
		req = agentd.AsAgentPeer(req, conv)
	}
	return testharness.Serve(h.f.Mux, req)
}

func (h *taskboardHarness) task(rec *httptest.ResponseRecorder, want int) tbTask {
	h.t.Helper()
	require.Equal(h.t, want, rec.Code, "body=%s", rec.Body.String())
	var out tbTask
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (h *taskboardHarness) list(conv, path string) []string {
	h.t.Helper()
	rec := h.call(conv, http.MethodGet, path, nil)
	require.Equal(h.t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out struct {
		Tasks []tbTask `json:"tasks"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	titles := []string{}
	for _, t := range out.Tasks {
		titles = append(titles, t.Title)
	}
	return titles
}

// A member captures ideas, queues and orders them, blocks and closes them;
// the board lists the queue first, then the ideas, and history records it all.
func TestTaskboardMemberCapturesAndOrders(t *testing.T) {
	h := newTaskboardFlow(t)
	const member = "tb-member-1111"
	h.agent(t, member)
	h.f.HaveGroup("crew")
	h.f.HaveMember("crew", member)

	add := func(body map[string]any) tbTask {
		return h.task(h.call(member, http.MethodPost, "/v1/groups/crew/taskboard/tasks", body), http.StatusCreated)
	}
	idea := add(map[string]any{"title": "try the fused card", "ref": "tcl-7dux2g"})
	assert.Equal(t, "idea", idea.Stage, "capture lands in ideas")
	require.NotNil(t, idea.Ref)
	assert.Equal(t, "awb", idea.Ref.Kind)
	assert.Equal(t, "tcl-7dux2g", idea.Ref.Label)
	assert.Contains(t, idea.CreatedBy, "agent:")
	first := add(map[string]any{"title": "fix flake in foo_test.go", "stage": "next",
		"ref": "https://github.com/tofutools/tclaude/pull/3432"})
	assert.Equal(t, "github", first.Ref.Kind)
	assert.Equal(t, "#3432", first.Ref.Label)
	second := add(map[string]any{"title": "LIN follow-up", "stage": "next", "ref": "LIN-231", "after": []string{first.ID}})
	assert.Equal(t, "linear", second.Ref.Kind)
	assert.True(t, second.Queued, "an unmet dependency queues the task")

	assert.Equal(t, []string{"fix flake in foo_test.go", "LIN follow-up", "try the fused card"},
		h.list(member, "/v1/groups/crew/taskboard"))
	// The group can be left out by a member of exactly one group.
	assert.Equal(t, h.list(member, "/v1/groups/crew/taskboard"), h.list(member, "/v1/taskboard"))

	// Reorder; promote puts the idea at the end of the queue.
	h.task(h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+second.ID+"/move", map[string]any{"top": true}), http.StatusOK)
	h.task(h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/stage", map[string]any{"stage": "next"}), http.StatusOK)
	assert.Equal(t, []string{"LIN follow-up", "fix flake in foo_test.go", "try the fused card"},
		h.list(member, "/v1/groups/crew/taskboard?stage=next"))
	assert.Empty(t, h.list(member, "/v1/groups/crew/taskboard?stage=idea"))

	// Block needs a reason; done leaves the list and the dependency is met.
	rec := h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/status", map[string]any{"status": "blocked"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	blocked := h.task(h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/status",
		map[string]any{"status": "blocked", "reason": "waiting on mock review"}), http.StatusOK)
	assert.Equal(t, "waiting on mock review", blocked.BlockReason)
	h.task(h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+first.ID+"/status", map[string]any{"status": "done"}), http.StatusOK)
	assert.Equal(t, []string{"LIN follow-up", "try the fused card"}, h.list(member, "/v1/groups/crew/taskboard"))
	assert.Contains(t, h.list(member, "/v1/groups/crew/taskboard?all=1"), "fix flake in foo_test.go")
	shown := h.task(h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+second.ID, nil), http.StatusOK)
	assert.False(t, shown.Queued)
	assert.Equal(t, []string{first.ID}, shown.DependsOn)

	// A dependency cycle is refused.
	rec = h.call(member, http.MethodPatch, "/v1/taskboard/tasks/"+first.ID, map[string]any{"add_deps": []string{second.ID}})
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())

	// A title with a newline, a line separator or a bidi override is refused.
	for _, title := range []string{"a\nb", "a\u2028b", "evil\u202Egnp.exe"} {
		rec = h.call(member, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": title})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%q", title)
	}
	// A ref's kind comes from its host, not from a substring.
	spoof := add(map[string]any{"title": "spoof", "ref": "https://evil.example/?u=://github.com/x"})
	assert.Equal(t, "url", spoof.Ref.Kind)

	rec = h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+idea.ID+"/events", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var hist struct {
		Events []struct{ Kind string } `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hist))
	kinds := []string{}
	for _, e := range hist.Events {
		kinds = append(kinds, e.Kind)
	}
	assert.Equal(t, []string{"created", "promoted", "blocked"}, kinds)

	// The sweep archives a closed task out of --all.
	agentd.SweepTaskboardForTest(time.Now().Add(15 * 24 * time.Hour))
	assert.NotContains(t, h.list(member, "/v1/groups/crew/taskboard?all=1"), "fix flake in foo_test.go")
}

// On by default; features.taskboard false turns every route to 404.
func TestTaskboardOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	get := func() *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups/crew/taskboard", nil)))
	}
	rec := get()
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Taskboard: new(false)}}))
	rec = get()
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "feature_disabled")
}

// Another group's board: an outsider gets 404 for its tasks and 403 for its
// list; a scoped grant opens exactly the named group; a deny beats membership;
// an owner who is not a member reads and writes.
func TestTaskboardCrossGroupAccess(t *testing.T) {
	h := newTaskboardFlow(t)
	const (
		member   = "tb-member-2222"
		outsider = "tb-outsider-3333"
		owner    = "tb-owner-4444"
	)
	for _, c := range []string{member, outsider, owner} {
		h.agent(t, c)
	}
	crew := h.f.HaveGroup("crew")
	h.f.HaveGroup("website")
	h.f.HaveMember("crew", member)
	h.f.HaveMember("website", outsider)
	require.NoError(t, db.AddAgentGroupOwner(crew.ID, owner, "test"))

	task := h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "secret plan"}), http.StatusCreated)
	assert.Equal(t, "human:operator", task.CreatedBy)
	other := h.task(h.call("", http.MethodPost, "/v1/groups/website/taskboard/tasks", map[string]any{"title": "site"}), http.StatusCreated)

	// The outsider: the task is not found, the list is forbidden.
	for _, path := range []string{"/v1/taskboard/tasks/" + task.ID, "/v1/taskboard/tasks/" + task.ID + "/events"} {
		rec := h.call(outsider, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
		assert.NotContains(t, rec.Body.String(), "secret plan")
	}
	rec := h.call(outsider, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/status", map[string]any{"status": "dropped"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// An unidentified caller cannot tell an existing id from a missing one.
	rec = testharness.Serve(h.f.Mux, agentd.AsUnconfirmedPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret plan")
	rec = h.call(outsider, http.MethodGet, "/v1/groups/crew/taskboard", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermTaskboardRead)
	rec = h.call(outsider, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "x"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermTaskboardWrite)

	// A read grant scoped to crew: reads, cannot write.
	require.NoError(t, db.GrantAgentPermissionWithScope(outsider, agentd.PermTaskboardRead, `{"group":["crew"]}`, "test"))
	assert.Equal(t, []string{"secret plan"}, h.list(outsider, "/v1/groups/crew/taskboard"))
	h.task(h.call(outsider, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil), http.StatusOK)
	rec = h.call(outsider, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/status", map[string]any{"status": "dropped"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "read does not write")

	// A write grant scoped to crew writes there (and implies read), and a crew
	// member's grant-free access never reaches website.
	require.NoError(t, db.GrantAgentPermissionWithScope(outsider, agentd.PermTaskboardWrite, `{"group":["crew"]}`, "test"))
	h.task(h.call(outsider, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/stage", map[string]any{"stage": "next"}), http.StatusOK)
	assert.Equal(t, http.StatusNotFound, h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+other.ID, nil).Code)

	// A scope naming another group does not reach crew.
	const third = "tb-third-5555"
	h.agent(t, third)
	require.NoError(t, db.GrantAgentPermissionWithScope(third, agentd.PermTaskboardRead, `{"group":["website"]}`, "test"))
	assert.Equal(t, http.StatusNotFound, h.call(third, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil).Code)
	h.task(h.call(third, http.MethodGet, "/v1/taskboard/tasks/"+other.ID, nil), http.StatusOK)

	// An explicit deny of read wins over membership.
	require.NoError(t, db.SetAgentPermissionOverride(member, agentd.PermTaskboardRead, db.PermEffectDeny, "test"))
	assert.Equal(t, http.StatusNotFound, h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil).Code)
	_, err := db.RevokeAgentPermission(member, agentd.PermTaskboardRead)
	require.NoError(t, err)
	h.task(h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil), http.StatusOK)

	// The owner, not a member, reads and writes through ownership.
	assert.Equal(t, []string{"secret plan"}, h.list(owner, "/v1/groups/crew/taskboard"))
	h.task(h.call(owner, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/status", map[string]any{"status": "done"}), http.StatusOK)
}
