package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type tbEpicTask struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Status           string `json:"status"`
	Kind             string `json:"kind"`
	ParentID         string `json:"parent_id"`
	CloseSuggestedAt string `json:"close_suggested_at"`
	Progress         *struct {
		Total      int `json:"total"`
		Done       int `json:"done"`
		InProgress int `json:"in_progress"`
		Ideas      int `json:"ideas"`
	} `json:"progress"`
	Children []tbEpicTask `json:"children"`
}

// An agent breaks work into an epic: the board carries kind, parent and
// progress; closing the last task nudges the epic's creator once; done on
// an epic with open tasks needs --force.
func TestTaskboardEpicTracksItsTasksAndNudgesItsCreator(t *testing.T) {
	h := newTaskboardFlow(t)
	const lead = "tb-epic-lead-1111"
	h.agent(t, lead)
	h.f.HaveGroup("crew")
	h.f.HaveMember("crew", lead)
	h.groupWrites("crew")
	call := func(method, path string, body any, want int) tbEpicTask {
		t.Helper()
		rec := h.call(lead, method, path, body)
		require.Equal(t, want, rec.Code, rec.Body.String())
		var out tbEpicTask
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	list := func(query string) []tbEpicTask {
		t.Helper()
		rec := h.call(lead, http.MethodGet, "/v1/groups/crew/taskboard"+query, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out struct {
			Tasks []tbEpicTask `json:"tasks"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out.Tasks
	}

	epic := call(http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Epics", "kind": "epic"}, http.StatusCreated)
	require.Equal(t, "epic", epic.Kind)
	require.NotNil(t, epic.Progress)
	a := call(http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "schema", "parent": epic.ID}, http.StatusCreated)
	b := call(http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "api"}, http.StatusCreated)
	require.Equal(t, epic.ID, a.ParentID)
	call(http.MethodPatch, "/v1/taskboard/tasks/"+b.ID, map[string]any{"parent": epic.ID}, http.StatusOK)

	// An epic is not worked on itself.
	rec := h.call(lead, http.MethodPost, "/v1/taskboard/tasks/"+epic.ID+"/assign", map[string]any{"to": lead})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	epics := list("?kind=epic")
	require.Len(t, epics, 1)
	require.Equal(t, 2, epics[0].Progress.Total)
	require.Len(t, list("?parent="+epic.ID), 2)
	shown := call(http.MethodGet, "/v1/taskboard/tasks/"+epic.ID, nil, http.StatusOK)
	require.Len(t, shown.Children, 2)

	rec = h.call(lead, http.MethodPost, "/v1/taskboard/tasks/"+epic.ID+"/status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "epic_open_children")

	// The last task closing: the board asks, and the creator is mailed once.
	call(http.MethodPost, "/v1/taskboard/tasks/"+a.ID+"/status", map[string]any{"status": "done"}, http.StatusOK)
	call(http.MethodPost, "/v1/taskboard/tasks/"+b.ID+"/status", map[string]any{"status": "done"}, http.StatusOK)
	shown = call(http.MethodGet, "/v1/taskboard/tasks/"+epic.ID, nil, http.StatusOK)
	require.NotEmpty(t, shown.CloseSuggestedAt)
	require.Equal(t, 2, shown.Progress.Done)
	nudges := 0
	for _, s := range tbInboxSubjects(t, lead) {
		if strings.Contains(s, "close epic?") {
			nudges++
		}
	}
	require.Equal(t, 1, nudges)
	require.Equal(t, "open", shown.Status, "never closed by itself")

	call(http.MethodPost, "/v1/taskboard/tasks/"+epic.ID+"/status", map[string]any{"status": "done"}, http.StatusOK)

	// Reopening the epic flags it again on the board, but mails nobody.
	reopened := call(http.MethodPost, "/v1/taskboard/tasks/"+epic.ID+"/reopen", map[string]any{}, http.StatusOK)
	require.NotEmpty(t, reopened.CloseSuggestedAt)
	nudges = 0
	for _, s := range tbInboxSubjects(t, lead) {
		if strings.Contains(s, "close epic?") {
			nudges++
		}
	}
	require.Equal(t, 1, nudges)
	// An epic does not move to another node.
	rec = h.call(lead, http.MethodPost, "/v1/taskboard/tasks/"+epic.ID+"/moved-away", map[string]any{"check": true})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// A linked node works epics through the routes and grants it already has.
func TestPeerTaskboardEpicsUnderTaskboardGrants(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, tbEpicTask, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, method, path, body))
		var out tbEpicTask
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	code, epic, body := call("POST", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Peer epic", "kind": "epic"})
	require.Equal(t, http.StatusCreated, code, body)
	code, child, body := call("POST", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "child", "parent": epic.ID})
	require.Equal(t, http.StatusCreated, code, body)
	require.Equal(t, epic.ID, child.ParentID)
	code, _, body = call("GET", "/v1/groups/crew/taskboard?kind=epic", nil)
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"total":1`)
	require.Contains(t, body, `"kind":"epic"`)
	code, _, body = call("GET", "/v1/groups/crew/taskboard?parent="+epic.ID, nil)
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, "child")
	code, _, body = call("POST", "/v1/taskboard/tasks/"+epic.ID+"/status", map[string]any{"status": "done", "force": true})
	require.Equal(t, http.StatusOK, code, body)
}
