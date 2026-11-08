package agentd_test

import (
	"bytes"
	"encoding/xml"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent with read access exports a drawing board no dashboard
// has ever rendered: the whole board as PNG, a part of it as SVG, and a
// region; it is refused without read access, and told which ids are not
// there.
func TestDrawingBoardExportRendersWithoutADashboard(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()
	operator := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, drawingBoardDashReq(t, method, path, body))
	}
	require.Equal(t, http.StatusCreated, operator(http.MethodPost, "/api/drawing-boards", map[string]any{"id": "arch", "title": "Architecture"}).Code)
	rec := operator(http.MethodPatch, "/api/drawing-boards/arch", map[string]any{"elements": []any{
		rect("api", 1, 1, 0), rect("db", 1, 2, 200),
		map[string]any{"id": "api-db", "type": "arrow", "x": 10, "y": 5, "points": [][]float64{{0, 0}, {190, 0}}, "version": 1, "versionNonce": 3,
			"startBinding": map[string]any{"elementId": "api"}, "endBinding": map[string]any{"elementId": "db"}},
		map[string]any{"id": "note", "type": "text", "x": 0, "y": 300, "text": "<b>& notes", "version": 1, "versionNonce": 4},
	}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	conv := "ccccdddd-0000-0000-0000-000000000001"
	f.HaveConvWithTitle(conv, "viewer")
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	export := func(body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/drawing-boards/arch/export", body), conv))
	}

	require.Equal(t, http.StatusForbidden, export(map[string]any{}).Code, "exporting needs read access to the drawing board")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermDrawingBoardsRead, "test"))

	rec = export(map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "1", rec.Header().Get("X-Drawing-board-Version"))
	assert.Equal(t, "4", rec.Header().Get("X-Drawing-board-Export-Elements"))
	whole, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err, "the reply is a PNG")

	rec = export(map[string]any{"format": "svg", "elements": []string{"api", "db"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Equal(t, "sandbox", rec.Header().Get("Content-Security-Policy"), "an SVG never runs from the daemon's origin")
	assert.Equal(t, "3", rec.Header().Get("X-Drawing-board-Export-Elements"), "the arrow between the two comes along")
	assert.NoError(t, xml.Unmarshal(rec.Body.Bytes(), new(struct{})), "the SVG is well-formed")
	assert.NotContains(t, rec.Body.String(), `data-element="note"`)

	rec = export(map[string]any{"region": map[string]any{"x": -5, "y": 290, "width": 100, "height": 50}, "scale": 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	part, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 100, part.Bounds().Dx(), "a region is cropped to itself")
	assert.Less(t, part.Bounds().Dx(), whole.Bounds().Dx())

	rec = export(map[string]any{"elements": []string{"api", "ghost"}})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "ghost")
	rec = export(map[string]any{"region": map[string]any{"x": 5000, "y": 5000, "width": 10, "height": 10}})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "an empty region has nothing to export")
	assert.Equal(t, http.StatusBadRequest, export(map[string]any{"format": "gif"}).Code)

	// The dashboard exports through its own route, under its session.
	rec = operator(http.MethodPost, "/api/drawing-boards/arch/export", map[string]any{"format": "svg"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "&lt;b&gt;&amp; notes")
}
