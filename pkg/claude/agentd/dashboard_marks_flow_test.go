package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const markAgent = "mark-aaaa-bbbb-cccc-000000000001"

type markFlow struct {
	*captureFlow
}

func newMarkFlow(t *testing.T) *markFlow {
	t.Helper()
	c := newCaptureFlow(t)
	c.f.HaveConvWithTitle(markAgent, "steward-k3x")
	c.f.HaveGroup("team")
	c.f.HaveMember("team", markAgent)
	c.f.HaveAliveSession(markAgent, "mark", "tclaude-mark", c.f.TestCwd("work"))
	require.NoError(t, db.GrantAgentPermission(markAgent, agentd.PermDashboardMark, "test"))
	return &markFlow{c}
}

func (m *markFlow) call(method, path string, body any) *httptest.ResponseRecorder {
	m.t.Helper()
	return testharness.Serve(m.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(m.t, method, path, body), markAgent))
}

func (m *markFlow) mark(body map[string]any) int64 {
	m.t.Helper()
	rec := m.call(http.MethodPost, "/v1/dashboard/marks", body)
	require.Equal(m.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		ID int64 `json:"id"`
	}
	require.NoError(m.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.ID
}

type listedMark struct {
	ID      int64  `json:"id"`
	State   string `json:"state"`
	EndedBy string `json:"ended_by"`
	Reply   string `json:"reply"`
	Tip     string `json:"tip"`
}

func (m *markFlow) list(query string) ([]listedMark, int64) {
	m.t.Helper()
	rec := m.call(http.MethodGet, "/v1/dashboard/marks"+query, nil)
	require.Equal(m.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Marks      []listedMark `json:"marks"`
		NextBefore int64        `json:"next_before"`
	}
	require.NoError(m.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Marks, resp.NextBefore
}

// tabMarks is what an open tab is told to draw.
func (m *markFlow) tabMarks() []int64 {
	m.t.Helper()
	rec := testharness.Serve(m.dash, testharness.JSONRequest(m.t, http.MethodPost, "/api/companion/presence",
		map[string]any{"client": "tab", "focused": true, "visible": true}))
	require.Equal(m.t, http.StatusOK, rec.Code)
	var resp struct {
		Marks []struct {
			ID int64 `json:"id"`
		} `json:"marks"`
	}
	require.NoError(m.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := []int64{}
	for _, mark := range resp.Marks {
		ids = append(ids, mark.ID)
	}
	return ids
}

func (m *markFlow) operator(path string, body any) int {
	m.t.Helper()
	return testharness.Serve(m.dash, testharness.JSONRequest(m.t, http.MethodPost, path, body)).Code
}

func TestDashboardMarks_LimitsRefuseClearly(t *testing.T) {
	m := newMarkFlow(t)
	refused := func(body map[string]any, want string) {
		t.Helper()
		rec := m.call(http.MethodPost, "/v1/dashboard/marks", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), want)
	}
	target := map[string]any{"target": "Save"}
	refused(map[string]any{"kind": "sparkle", "spec": target}, "unknown kind")
	refused(map[string]any{"kind": "highlight", "spec": map[string]any{}}, "needs a target")
	refused(map[string]any{"kind": "highlight", "spec": target, "tip": strings.Repeat("x", agentd.MarkMaxTip+1)}, "at most 280")
	refused(map[string]any{"kind": "highlight", "spec": target, "timeout_seconds": 601}, "at most 10m")
	refused(map[string]any{"kind": "arrow", "spec": target}, "needs from")

	var first int64
	for i := range agentd.MarkMaxActive {
		id := m.mark(map[string]any{"kind": "highlight", "spec": target})
		if i == 0 {
			first = id
		}
	}
	rec := m.call(http.MethodPost, "/v1/dashboard/marks", map[string]any{"kind": "highlight", "spec": target})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "too_many_marks")

	// Replacing keeps the count, and the old mark says what happened to it.
	m.mark(map[string]any{"kind": "circle", "spec": target, "replaces": first})
	replaced, _ := m.list("?state=replaced")
	require.Len(t, replaced, 1)
	assert.Equal(t, first, replaced[0].ID)
	agentID, err := db.AgentIDForConv(markAgent)
	require.NoError(t, err)
	assert.Equal(t, agentID, replaced[0].EndedBy)
}

func TestDashboardMarks_EverywayToEndOneIsRecorded(t *testing.T) {
	m := newMarkFlow(t)
	target := map[string]any{"target": "Save"}
	dismissed := m.mark(map[string]any{"kind": "highlight", "spec": target})
	acknowledged := m.mark(map[string]any{"kind": "highlight", "spec": target})
	cleared := m.mark(map[string]any{"kind": "highlight", "spec": target})
	swept := m.mark(map[string]any{"kind": "label", "spec": map[string]any{}, "tip": "hello"})
	short := m.mark(map[string]any{"kind": "highlight", "spec": target, "timeout_seconds": 0.001})
	time.Sleep(5 * time.Millisecond)

	assert.Equal(t, []int64{dismissed, acknowledged, cleared, swept}, m.tabMarks(), "an expired mark is not drawn")

	require.Equal(t, http.StatusNoContent, m.operator(fmt.Sprintf("/api/companion/marks/%d/dismiss", dismissed), nil))
	require.Equal(t, http.StatusNoContent, m.operator(fmt.Sprintf("/api/companion/marks/%d/acknowledge", acknowledged), nil))
	require.Equal(t, http.StatusNoContent, m.call(http.MethodDelete, fmt.Sprintf("/v1/dashboard/marks/%d", cleared), nil).Code)
	assert.Equal(t, http.StatusConflict, m.call(http.MethodDelete, fmt.Sprintf("/v1/dashboard/marks/%d", cleared), nil).Code, "already ended")
	require.Equal(t, http.StatusNoContent, m.operator("/api/companion/marks/dismiss", nil), "the operator's one action")
	assert.Empty(t, m.tabMarks())

	marks, _ := m.list("")
	got := map[int64]listedMark{}
	for _, mark := range marks {
		got[mark.ID] = mark
	}
	agentID, err := db.AgentIDForConv(markAgent)
	require.NoError(t, err)
	assert.Equal(t, "dismissed/operator", got[dismissed].State+"/"+got[dismissed].EndedBy)
	assert.Equal(t, "acknowledged/operator", got[acknowledged].State+"/"+got[acknowledged].EndedBy)
	assert.Equal(t, "cleared/"+agentID, got[cleared].State+"/"+got[cleared].EndedBy)
	assert.Equal(t, "dismissed/operator", got[swept].State+"/"+got[swept].EndedBy)
	assert.Equal(t, "expired/system", got[short].State+"/"+got[short].EndedBy)
}

func TestDashboardMarks_ListPagesFromTheBackend(t *testing.T) {
	m := newMarkFlow(t)
	var ids []int64
	for range 5 {
		id := m.mark(map[string]any{"kind": "label", "spec": map[string]any{}, "tip": "x"})
		ids = append(ids, id)
	}
	page, next := m.list("?limit=2")
	require.Len(t, page, 2)
	assert.Equal(t, ids[4], page[0].ID, "newest first")
	require.Equal(t, ids[3], next)
	page, next = m.list(fmt.Sprintf("?limit=3&before=%d", next))
	require.Len(t, page, 3)
	assert.Equal(t, ids[2], page[0].ID)
	page, next = m.list(fmt.Sprintf("?limit=3&before=%d", next))
	assert.Empty(t, page)
	assert.Zero(t, next)
	assert.Equal(t, http.StatusBadRequest, m.call(http.MethodGet, "/v1/dashboard/marks?state=gone", nil).Code)
}

// The operator answers at the mark and the agent learns which one, without a
// route to it and whatever groups it shares.
func TestDashboardMarks_ReplyReachesTheAgentNamingTheMark(t *testing.T) {
	m := newMarkFlow(t)
	id := m.mark(map[string]any{"kind": "highlight", "spec": map[string]any{"target": "Save"}, "tip": "Save this"})
	reply := fmt.Sprintf("/api/companion/marks/%d/reply", id)
	assert.Equal(t, http.StatusBadRequest, m.operator(reply, map[string]any{"text": "  "}))
	require.Equal(t, http.StatusAccepted, m.operator(reply, map[string]any{"text": "why though?"}))

	rows, err := db.ListAgentMessagesForConv(markAgent, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Equal(t, fmt.Sprintf(`[mark %d: "Save this"] why though?`, id), rows[0].Body)
	assert.Empty(t, rows[0].FromConv, "it comes from the operator")

	marks, _ := m.list("?state=active")
	require.Len(t, marks, 1, "the mark stays up")
	assert.Equal(t, "why though?", marks[0].Reply)

	require.Equal(t, http.StatusNoContent, m.operator(fmt.Sprintf("/api/companion/marks/%d/acknowledge", id), nil))
	assert.Equal(t, http.StatusConflict, m.operator(reply, map[string]any{"text": "late"}))

	// With nobody to tell, the history must not claim an answer.
	human := testharness.Serve(m.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/dashboard/marks",
		map[string]any{"kind": "label", "spec": map[string]any{}, "tip": "from the CLI"})))
	require.Equal(t, http.StatusOK, human.Code, human.Body.String())
	var drawn struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(human.Body.Bytes(), &drawn))
	assert.Equal(t, http.StatusConflict, m.operator(fmt.Sprintf("/api/companion/marks/%d/reply", drawn.ID), map[string]any{"text": "hello?"}))
	marks, _ = m.list("?state=active")
	require.Len(t, marks, 1)
	assert.Empty(t, marks[0].Reply)
}

func TestDashboardMarks_NeedPermissionAndHiddenTabsDrawNothing(t *testing.T) {
	m := newMarkFlow(t)
	_, err := db.RevokeAgentPermission(markAgent, agentd.PermDashboardMark)
	require.NoError(t, err)
	rec := m.call(http.MethodPost, "/v1/dashboard/marks", map[string]any{"kind": "label", "spec": map[string]any{}, "tip": "x"})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	hidden := testharness.Serve(m.dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/presence",
		map[string]any{"client": "tab", "visible": false}))
	assert.NotContains(t, hidden.Body.String(), "marks", "a hidden tab keeps what it had")
}

func TestDashboardCaptures_SavedThenRead(t *testing.T) {
	c := newCaptureFlow(t)
	c.ping("tab", true, true)
	waiting := make(chan *httptest.ResponseRecorder, 1)
	req := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/dashboard/capture", map[string]any{"save": true, "label": "before the deploy"}))
	go func() { waiting <- testharness.Serve(c.f.Mux, req) }()
	id := c.await(func() string { return c.ping("tab", true, true) })
	require.Equal(t, http.StatusNoContent, c.answer(id, map[string]any{"png": capturePNG, "context": map[string]any{"path": "/config"}}))
	rec := <-waiting
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		SavedID int64 `json:"saved_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotZero(t, resp.SavedID)

	get := func(path string) *httptest.ResponseRecorder {
		return testharness.Serve(c.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, path, nil)))
	}
	listing := get("/v1/dashboard/captures")
	assert.Contains(t, listing.Body.String(), "before the deploy")
	assert.Contains(t, listing.Body.String(), "/config", "the context survives")
	image := get(fmt.Sprintf("/v1/dashboard/captures/%d?image=1", resp.SavedID))
	require.Equal(t, http.StatusOK, image.Code)
	assert.True(t, strings.HasPrefix(image.Body.String(), "\x89PNG"))
	assert.Equal(t, http.StatusNotFound, get("/v1/dashboard/captures/999").Code)
}
