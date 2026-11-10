package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A task's dashboard link (docs/taskboard.md "A task's link"): show and add
// carry it as url, and a [taskboard] mail ends with it, on the dashboard
// listener's base URL; with the listener off there is none.
func TestTaskboardTaskLink(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	urlOf := func(body []byte) string {
		var out struct {
			URL string `json:"url"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		return out.URL
	}

	rec := h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "no listener"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Empty(t, urlOf(rec.Body.Bytes()), "no dashboard listener, no link")

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:7777"))
	rec = h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "ship the lanes"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	added := h.task(rec, http.StatusCreated)
	want := "http://127.0.0.1:7777/groups?task=" + added.ID + "%3Acrew"
	assert.Equal(t, want, urlOf(rec.Body.Bytes()), "add carries the link")

	rec = h.call(h.worker, http.MethodGet, "/v1/taskboard/tasks/"+added.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, want, urlOf(rec.Body.Bytes()), "show carries it")

	rec = h.call(h.worker, http.MethodGet, "/v1/groups/crew/taskboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"url":"http`, "a listing does not")

	code, body := h.post(h.lead, added.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	msgs, err := db.ListInboxForActor(h.worker, tbAgentID(t, h.worker), 10)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[0].Body, "Open it in the dashboard: "+want, "the brief ends with the link")
}
