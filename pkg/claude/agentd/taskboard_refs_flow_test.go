package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Several refs on a task (tk_01ad0884): the first is the task's ref as
// every reader of one ref knows it; add_refs and rm_refs change the list,
// and a pull request ref carries the status the agent rows show for it.

type tbRefsPR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Checks *struct {
		Total  int    `json:"total"`
		Passed int    `json:"passed"`
		State  string `json:"state"`
	} `json:"checks"`
	Reviews *struct {
		Decision string `json:"decision"`
	} `json:"reviews"`
}

type tbRefsRef struct {
	Kind  string    `json:"kind"`
	URL   string    `json:"url"`
	Label string    `json:"label"`
	PR    *tbRefsPR `json:"pr"`
}

type tbRefsTask struct {
	ID   string      `json:"id"`
	Ref  *tbRefsRef  `json:"ref"`
	Refs []tbRefsRef `json:"refs"`
}

func (h *taskboardHarness) refsTask(method, path string, body any, want int) tbRefsTask {
	h.t.Helper()
	rec := h.call("", method, path, body)
	require.Equal(h.t, want, rec.Code, "body=%s", rec.Body.String())
	var out tbRefsTask
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func refLabels(refs []tbRefsRef) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.Label)
	}
	return out
}

func TestTaskboardTaskHasSeveralRefs(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	const pr = "https://github.com/tofutools/tclaude/pull/4235"

	created := h.refsTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "answer dialog", "ref": pr, "add_refs": []string{"tcl-a1b2c3", pr}}, http.StatusCreated)
	assert.Equal(t, []string{"#4235", "tcl-a1b2c3"}, refLabels(created.Refs), "the same PR again is not a second ref")
	require.NotNil(t, created.Ref)
	assert.Equal(t, "#4235", created.Ref.Label, "ref is the first")
	path := "/v1/taskboard/tasks/" + created.ID

	// Without ref, add_refs alone makes the first.
	alone := h.refsTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "linked", "add_refs": []string{"LIN-12", "https://example.com/spec"}}, http.StatusCreated)
	require.NotNil(t, alone.Ref)
	assert.Equal(t, "LIN-12", alone.Ref.Label)
	assert.Equal(t, []string{"LIN-12", "example.com"}, refLabels(alone.Refs))

	edited := h.refsTask(http.MethodPatch, path, map[string]any{"add_refs": []string{"owner/repo#7"}}, http.StatusOK)
	assert.Equal(t, []string{"#4235", "tcl-a1b2c3", "owner/repo#7"}, refLabels(edited.Refs))

	// ref replaces the first and keeps the rest.
	edited = h.refsTask(http.MethodPatch, path, map[string]any{"ref": "https://github.com/tofutools/tclaude/pull/4240"}, http.StatusOK)
	assert.Equal(t, []string{"#4240", "tcl-a1b2c3", "owner/repo#7"}, refLabels(edited.Refs))

	// rm_refs names a ref by #position, label or link; the next one moves up.
	edited = h.refsTask(http.MethodPatch, path, map[string]any{"rm_refs": []string{"#1", "owner/repo#7"}}, http.StatusOK)
	assert.Equal(t, []string{"tcl-a1b2c3"}, refLabels(edited.Refs))
	require.NotNil(t, edited.Ref)
	assert.Equal(t, "awb", edited.Ref.Kind, "removing the first makes the next one first")

	rec := h.call("", http.MethodPatch, path, map[string]any{"rm_refs": []string{"LIN-999"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a ref the task does not have: %s", rec.Body.String())
	many := []string{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		many = append(many, "https://example.com/"+id)
	}
	rec = h.call("", http.MethodPatch, path, map[string]any{"add_refs": many})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "past 10 refs: %s", rec.Body.String())

	// The activity records the change.
	act := h.call("", http.MethodGet, path+"/activity", nil)
	require.Equal(t, http.StatusOK, act.Code)
	assert.Contains(t, act.Body.String(), "refs")

	// clear_ref takes them all off.
	edited = h.refsTask(http.MethodPatch, path, map[string]any{"clear_ref": true}, http.StatusOK)
	assert.Nil(t, edited.Ref)
	assert.Empty(t, edited.Refs)
}

// A pull request ref shows what the agent rows show for it, read from the
// same caches, on the board and on a single task.
func TestTaskboardPRRefCarriesItsStatus(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	const pr = "https://github.com/tofutools/tclaude/pull/4233"
	agentd.SeedPRCachesForTest(pr, "open", "drawing board: snap", 23, 25, "approved")

	created := h.refsTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "snap", "ref": "tcl-a1b2c3", "add_refs": []string{pr}}, http.StatusCreated)
	require.Len(t, created.Refs, 2)
	assert.Nil(t, created.Refs[0].PR, "an awb ref is no pull request")
	got := created.Refs[1].PR
	require.NotNil(t, got)
	assert.Equal(t, 4233, got.Number)
	assert.Equal(t, "open", got.State)
	assert.Equal(t, "drawing board: snap", got.Title)
	require.NotNil(t, got.Checks)
	assert.Equal(t, 25, got.Checks.Total)
	assert.Equal(t, 23, got.Checks.Passed)
	assert.Equal(t, "failing", got.Checks.State)
	require.NotNil(t, got.Reviews)
	assert.Equal(t, "approved", got.Reviews.Decision)

	rec := h.call("", http.MethodGet, "/v1/groups/crew/taskboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var board struct {
		Tasks []tbRefsTask `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &board))
	require.Len(t, board.Tasks, 1)
	require.Len(t, board.Tasks[0].Refs, 2)
	require.NotNil(t, board.Tasks[0].Refs[1].PR)
	assert.Equal(t, 23, board.Tasks[0].Refs[1].PR.Checks.Passed)

	// A PR nothing has read yet is still a PR: its number, no status.
	unread := h.refsTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "other", "ref": "https://github.com/tofutools/tclaude/pull/1"}, http.StatusCreated)
	require.NotNil(t, unread.Refs[0].PR)
	assert.Equal(t, 1, unread.Refs[0].PR.Number)
	assert.Empty(t, unread.Refs[0].PR.State)
	assert.Nil(t, unread.Refs[0].PR.Checks)
}

// A task cloned or moved in keeps all of its refs.
func TestTaskboardTransferKeepsRefs(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	h.f.HaveGroup("other")
	created := h.refsTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "two refs", "ref": "LIN-5", "add_refs": []string{"https://example.com/x"}}, http.StatusCreated)

	rec := h.call("", http.MethodPost, "/v1/taskboard/tasks/"+created.ID+"/move-to-group", map[string]any{"group": "other", "clone": true})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var cloned struct {
		Task tbRefsTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cloned))
	assert.Equal(t, []string{"LIN-5", "example.com"}, refLabels(cloned.Task.Refs))

	in := h.refsTask(http.MethodPost, "/v1/groups/other/taskboard/transfer-in", map[string]any{
		"title": "from afar", "ref": "LIN-6", "add_refs": []string{"tcl-a1b2c3"},
		"origin": map[string]any{"node": "far", "group": "crew", "task": "tk_00000001"}}, http.StatusCreated)
	assert.Equal(t, []string{"LIN-6", "tcl-a1b2c3"}, refLabels(in.Refs))
}
