package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A linked node (bob) reaches a group's task board on this node through its
// peer view, under the taskboard.read / taskboard.write grants this node
// gave it, scoped per group; nothing without them.
func TestPeerTaskboardReadAndWriteUnderGroupGrants(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: true}
	require.NoError(t, config.Save(cfg))
	fh.f.HaveGroup("crew")
	fh.f.HaveGroup("other")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	local := func(path string, body any) tbTask {
		t.Helper()
		rec := fedHuman(t, fh.f, http.MethodPost, path, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var out tbTask
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	crewTask := local("/v1/groups/crew/taskboard/tasks", map[string]any{"title": "crew plan", "stage": "next"})
	otherTask := local("/v1/groups/other/taskboard/tasks", map[string]any{"title": "other secret"})

	// No grant: nothing is reachable, and a task id answers as missing.
	code, body := call("GET", "/v1/groups/crew/taskboard", nil)
	assert.NotEqual(t, http.StatusOK, code, body)
	code, body = call("GET", "/v1/taskboard/tasks/"+crewTask.ID, nil)
	assert.Equal(t, http.StatusNotFound, code, body)
	assert.NotContains(t, body, "crew plan")

	// Read on crew: the list, one task, its history and the context, crew
	// only; no write.
	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	code, body = call("GET", "/v1/groups/crew/taskboard", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "crew plan")
	code, body = call("GET", "/v1/taskboard/tasks/"+crewTask.ID, nil)
	require.Equal(t, http.StatusOK, code, body)
	code, _ = call("GET", "/v1/taskboard/tasks/"+crewTask.ID+"/events", nil)
	require.Equal(t, http.StatusOK, code)
	code, body = call("GET", "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "crew plan")
	assert.NotContains(t, body, "other secret", "the context lists only the boards the peer reads")
	code, body = call("GET", "/v1/taskboard/tasks/"+otherTask.ID, nil)
	assert.Equal(t, http.StatusNotFound, code, "a task outside the peer's groups is not found: %s", body)
	code, body = call("POST", "/v1/taskboard/tasks/"+crewTask.ID+"/status", map[string]any{"status": "done"})
	assert.Equal(t, http.StatusForbidden, code, "read does not write: %s", body)
	code, body = call("POST", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "from bob"})
	assert.Equal(t, http.StatusForbidden, code, body)

	// The node summary carries the counts of the boards the peer reads.
	code, body = call("GET", "/api/node-summary", nil)
	require.Equal(t, http.StatusOK, code, body)
	var summary struct {
		Taskboard *struct {
			Live int `json:"live"`
			Next int `json:"next"`
		} `json:"taskboard"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	require.NotNil(t, summary.Taskboard, body)
	assert.Equal(t, 1, summary.Taskboard.Live, "crew's one task, not other's")

	// Write on crew: capture, change and close, recorded as the peer's.
	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	code, body = call("POST", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "from bob"})
	require.Equal(t, http.StatusCreated, code, body)
	var added tbTask
	require.NoError(t, json.Unmarshal([]byte(body), &added))
	assert.Equal(t, "idea", added.Stage)
	assert.Contains(t, added.CreatedBy, "peer: the operator of")
	code, body = call("POST", "/v1/taskboard/tasks/"+added.ID+"/stage", map[string]any{"stage": "next"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = call("POST", "/v1/taskboard/tasks/"+crewTask.ID+"/status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	// An import works too; the title lookups run as the peer, which holds no
	// proxy grant here, so each id is its title.
	code, body = call("POST", "/v1/groups/crew/taskboard/tasks", map[string]any{"refs": []string{"LIN-5"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"created":1`)
	// Still nothing on other.
	code, _ = call("POST", "/v1/taskboard/tasks/"+otherTask.ID+"/status", map[string]any{"status": "dropped"})
	assert.Equal(t, http.StatusNotFound, code)

	// Write alone implies read.
	h.grant(t, agentd.PermTaskboardWrite, "group=other")
	code, body = call("GET", "/v1/groups/other/taskboard", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "other secret")
	code, _ = call("GET", "/v1/taskboard/tasks/"+otherTask.ID, nil)
	assert.Equal(t, http.StatusOK, code)

	// The peer's writes are audited on this node as the peer's.
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.peer_view"})
	require.NoError(t, err)
	var labels []string
	for _, row := range rows {
		labels = append(labels, row.Detail)
	}
	assert.Contains(t, labels, "POST POST /v1/taskboard/tasks/{id}/status task="+crewTask.ID)
}

// Unrestricted trust reaches every group's board as this node's operator does.
func TestPeerTaskboardUnrestricted(t *testing.T) {
	h := newPeerProcessHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: true}
	require.NoError(t, config.Save(cfg))
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "crew plan"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var task tbTask
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &task))

	code, _ := h.call("GET", "/v1/taskboard/tasks/"+task.ID, nil)
	assert.Equal(t, http.StatusNotFound, code)
	h.trustUnrestricted(t)
	code, body := h.call("GET", "/v1/taskboard/tasks/"+task.ID, nil)
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.call("POST", "/v1/taskboard/tasks/"+task.ID+"/status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
}
