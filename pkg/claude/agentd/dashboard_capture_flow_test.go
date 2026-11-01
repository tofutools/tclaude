package agentd_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

var capturePNG = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nrest"))

type captureFlow struct {
	t    *testing.T
	f    *testharness.Flow
	dash http.Handler
}

func newCaptureFlow(t *testing.T) *captureFlow {
	t.Helper()
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	agentd.ResetDashboardCaptureForTest()
	t.Cleanup(agentd.ResetDashboardCaptureForTest)
	return &captureFlow{t: t, f: f, dash: dashMessageMux(t)}
}

// ping reports a tab and returns the capture aimed at it, if any.
func (c *captureFlow) ping(client string, focused, visible bool) string {
	c.t.Helper()
	rec := testharness.Serve(c.dash, testharness.JSONRequest(c.t, http.MethodPost, "/api/companion/presence",
		map[string]any{"client": client, "focused": focused, "visible": visible}))
	require.Equal(c.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Capture struct {
			ID string `json:"id"`
		} `json:"capture"`
	}
	require.NoError(c.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Capture.ID
}

func (c *captureFlow) answer(id string, body map[string]any) int {
	c.t.Helper()
	return testharness.Serve(c.dash, testharness.JSONRequest(c.t, http.MethodPost, "/api/companion/capture/"+id, body)).Code
}

// request asks for a capture on its own goroutine, as the waiting CLI does.
func (c *captureFlow) request() <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	req := agentd.AsHumanPeer(testharness.JSONRequest(c.t, http.MethodPost, "/v1/dashboard/capture", map[string]any{}))
	go func() { out <- testharness.Serve(c.f.Mux, req) }()
	return out
}

// await polls the tabs until one receives the request.
func (c *captureFlow) await(ping func() string) string {
	c.t.Helper()
	var id string
	require.Eventually(c.t, func() bool {
		id = ping()
		return id != ""
	}, 5*time.Second, 5*time.Millisecond)
	return id
}

func TestDashboardCapture_ClearErrorsWithoutAVisibleTab(t *testing.T) {
	c := newCaptureFlow(t)
	rec := <-c.request()
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no_dashboard")

	c.ping("hidden", false, false)
	rec = <-c.request()
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "dashboard_hidden", "a tab the operator cannot see is not captured")
}

func TestDashboardCapture_APollingPageIsNotCalledClosed(t *testing.T) {
	c := newCaptureFlow(t)
	rec := <-c.request()
	assert.Contains(t, rec.Body.String(), "no dashboard has connected since the daemon started")

	// A popped-out board polls but can never be captured.
	board := testharness.Serve(c.dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot?standalone=1", nil))
	require.Equal(t, http.StatusOK, board.Code)
	rec = <-c.request()
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// A page polling its snapshot exists even if its presence loop never ran.
	polled := testharness.Serve(c.dash, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, polled.Code)
	rec = <-c.request()
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "dashboard_silent")
	assert.Contains(t, rec.Body.String(), "reloading that page")
	assert.NotContains(t, rec.Body.String(), "no dashboard is open")

	// Its goodbye ends that: a closed page is not told to reload.
	c.ping("tab", true, true)
	gone := testharness.Serve(c.dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/presence",
		map[string]any{"client": "tab", "gone": true}))
	require.Equal(t, http.StatusNoContent, gone.Code)
	rec = <-c.request()
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "last heard from")
}

func TestDashboardCapture_ReturnsTheFocusedTabsRender(t *testing.T) {
	c := newCaptureFlow(t)
	c.ping("background", false, true)
	c.ping("focused", true, true)

	waiting := c.request()
	id := c.await(func() string {
		assert.Empty(t, c.ping("background", false, true), "only the focused tab is asked")
		return c.ping("focused", true, true)
	})
	assert.Empty(t, c.ping("focused", true, true), "the request is delivered once")

	require.Equal(t, http.StatusNoContent, c.answer(id, map[string]any{
		"png": capturePNG,
		"context": map[string]any{
			"tab": "boards", "path": "/boards/sketch", "focused": true,
			"viewport": map[string]any{"width": 1400, "height": 850},
			"cursor":   map[string]any{"x": 10, "y": 20, "inside": true, "under": map[string]any{"tag": "div", "text": "ok\x1b[31m"}},
			"board":    map[string]any{"id": "sketch", "zoom": 1, "region": map[string]any{"x": 1, "y": 2, "width": 3, "height": 4}},
		},
	}))
	rec := <-waiting
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		PNG     string `json:"png"`
		Context struct {
			Path   string `json:"path"`
			Cursor struct {
				Under struct {
					Text string `json:"text"`
				} `json:"under"`
			} `json:"cursor"`
			Board struct {
				ID string `json:"id"`
			} `json:"board"`
		} `json:"context"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, capturePNG, resp.PNG)
	assert.Equal(t, "/boards/sketch", resp.Context.Path)
	assert.Equal(t, "sketch", resp.Context.Board.ID)
	assert.Equal(t, "ok[31m", resp.Context.Cursor.Under.Text, "control bytes never reach the CLI")

	assert.Equal(t, http.StatusConflict, c.answer(id, map[string]any{"png": capturePNG}), "a spent capture takes no second image")
}

func TestDashboardCapture_RenderFailureAndOneAtATime(t *testing.T) {
	c := newCaptureFlow(t)
	c.ping("tab", true, true)
	waiting := c.request()
	id := c.await(func() string { return c.ping("tab", true, true) })

	busy := <-c.request()
	assert.Equal(t, http.StatusConflict, busy.Code)
	assert.Contains(t, busy.Body.String(), "capture_busy")

	assert.Equal(t, http.StatusBadRequest, c.answer(id, map[string]any{"png": base64.StdEncoding.EncodeToString([]byte("not a png"))}))
	require.Equal(t, http.StatusNoContent, c.answer(id, map[string]any{"error": "this browser could not draw the page"}))
	rec := <-waiting
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "could not draw the page")
}

// A tab that vanishes mid-request frees the next one, and a tab that says
// goodbye stops being a candidate at once.
func TestDashboardCapture_TimesOutAndForgetsClosedTabs(t *testing.T) {
	c := newCaptureFlow(t)
	t.Cleanup(agentd.SetCaptureTimeoutForTest(30 * time.Millisecond))
	c.ping("tab", true, true)
	rec := <-c.request()
	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)

	gone := testharness.Serve(c.dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/presence",
		map[string]any{"client": "tab", "gone": true}))
	require.Equal(t, http.StatusNoContent, gone.Code)
	rec = <-c.request()
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// formRequest asks for a capture with its text, as `guide capture --form` does.
func (c *captureFlow) formRequest(body map[string]any) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	req := agentd.AsHumanPeer(testharness.JSONRequest(c.t, http.MethodPost, "/v1/dashboard/capture", body))
	go func() { out <- testharness.Serve(c.f.Mux, req) }()
	return out
}

func TestDashboardCapture_FormTextIsAskedForAndKeptOutOfSavedCaptures(t *testing.T) {
	c := newCaptureFlow(t)
	c.ping("tab", true, true)

	waiting := c.formRequest(map[string]any{"form": true, "save": true})
	// The tab learns it was asked, which is also what its cue says.
	var delivered struct {
		Capture struct {
			ID   string `json:"id"`
			Form bool   `json:"form"`
		} `json:"capture"`
	}
	require.Eventually(t, func() bool {
		rec := testharness.Serve(c.dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/presence",
			map[string]any{"client": "tab", "focused": true, "visible": true}))
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &delivered))
		return delivered.Capture.ID != ""
	}, 5*time.Second, 5*time.Millisecond)
	assert.True(t, delivered.Capture.Form)

	require.Equal(t, http.StatusNoContent, c.answer(delivered.Capture.ID, map[string]any{
		"png": capturePNG, "context": map[string]any{"path": "/config"},
		"form_text": "Form: Spawn\nName [text]: \"reviewer\"\x1b[31m",
	}))
	rec := <-waiting
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		FormText string `json:"form_text"`
		SavedID  int64  `json:"saved_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Form: Spawn\nName [text]: \"reviewer\"[31m", resp.FormText, "lines survive, control bytes do not")

	saved := testharness.Serve(c.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/dashboard/captures", nil)))
	require.Equal(t, http.StatusOK, saved.Code)
	assert.Contains(t, saved.Body.String(), "/config", "the context is kept")
	assert.NotContains(t, saved.Body.String(), "reviewer", "form values are not")
}

func TestDashboardCapture_TextNobodyAskedForIsDropped(t *testing.T) {
	c := newCaptureFlow(t)
	c.ping("tab", true, true)
	waiting := c.request()
	id := c.await(func() string { return c.ping("tab", true, true) })
	require.Equal(t, http.StatusNoContent, c.answer(id, map[string]any{"png": capturePNG, "form_text": "Name: reviewer"}))
	rec := <-waiting
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "reviewer")
	assert.NotContains(t, rec.Body.String(), "form_text")
}

func TestDashboardCapture_FormTextNeedsItsOwnPermission(t *testing.T) {
	c := newCaptureFlow(t)
	const conv = "capt-aaaa-bbbb-cccc-000000000002"
	c.f.HaveConvWithTitle(conv, "worker")
	c.f.HaveGroup("team")
	c.f.HaveMember("team", conv)
	c.f.HaveAliveSession(conv, "capt", "tclaude-capt", c.f.TestCwd("work"))
	c.ping("tab", true, true)
	ask := func() *httptest.ResponseRecorder {
		return testharness.Serve(c.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/dashboard/capture", map[string]any{"form": true}), conv))
	}
	t.Cleanup(agentd.SetCaptureTimeoutForTest(10 * time.Millisecond))

	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermDashboardFormsCapture, "test"))
	assert.Equal(t, http.StatusForbidden, ask().Code, "the text comes with a capture, which takes dashboard.capture too")

	_, err := db.RevokeAgentPermission(conv, agentd.PermDashboardFormsCapture)
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermDashboardCapture, "test"))
	refused := ask()
	assert.Equal(t, http.StatusForbidden, refused.Code)
	assert.Contains(t, refused.Body.String(), agentd.PermDashboardFormsCapture, "capture alone does not read forms, and the refusal says what is missing")

	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermDashboardFormsCapture, "test"))
	assert.Equal(t, http.StatusGatewayTimeout, ask().Code, "with both, it reaches the tab")
}

func TestDashboardFormsCapture_IsNotGivenToASteward(t *testing.T) {
	c := newStewardCarryFlow(t)
	conv := c.summon()
	_, held := stewardOverrideHeld(t, conv, agentd.PermDashboardFormsCapture)
	assert.False(t, held, "form values are opt-in, for the Steward like any agent")
}

func TestDashboardCapture_NeedsPermission(t *testing.T) {
	c := newCaptureFlow(t)
	const conv = "capt-aaaa-bbbb-cccc-000000000001"
	c.f.HaveConvWithTitle(conv, "worker")
	c.f.HaveGroup("team")
	c.f.HaveMember("team", conv)
	c.f.HaveAliveSession(conv, "capt", "tclaude-capt", c.f.TestCwd("work"))
	c.ping("tab", true, true)
	ask := func() int {
		return testharness.Serve(c.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/dashboard/capture", map[string]any{}), conv)).Code
	}
	assert.Equal(t, http.StatusForbidden, ask())

	t.Cleanup(agentd.SetCaptureTimeoutForTest(10 * time.Millisecond))
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermDashboardCapture, "test"))
	assert.Equal(t, http.StatusGatewayTimeout, ask(), "granted, it reaches the tab")
}
