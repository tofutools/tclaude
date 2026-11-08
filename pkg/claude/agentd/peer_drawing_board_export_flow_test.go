package agentd_test

import (
	"bytes"
	"encoding/json"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A peer exports a drawing board it reads (drawing-board export --node, the
// editor's File › Export on a peer's page): groups.resources.read in the
// board's group, as JSON with the image base64-encoded in it, since a peer
// view carries JSON only. A board outside its groups is not found.
func TestPeerDrawingBoardExport(t *testing.T) {
	h := newPeerProcessHarness(t)
	sec := h.f.HaveGroup("secret")
	for id, groupID := range map[string]int64{"plan": h.group.ID, "hidden": sec.ID, "loose": 0} {
		_, err := db.CreateCollabDrawingBoard(id, id, "operator", 500)
		require.NoError(t, err)
		if groupID > 0 {
			require.NoError(t, db.SetCollabDrawingBoardGroup(id, groupID))
		}
	}
	export := func(id string, body map[string]any) (int, string) {
		t.Helper()
		return h.call("POST", "/api/drawing-boards/"+id+"/export", body)
	}

	code, body := export("plan", map[string]any{})
	require.Equal(t, 404, code, "without a grant in its group the board is not the peer's to know: %s", body)

	h.grant(t, agentd.PermGroupsResourcesRead, "group=crew")
	code, body = export("plan", map[string]any{})
	require.Equal(t, 422, code, "an empty board has nothing to export: %s", body)
	code, body = export("hidden", map[string]any{})
	require.Equal(t, 404, code, "a board outside the peer's groups is not its to know: %s", body)

	h.grant(t, agentd.PermGroupsResourcesWrite, "group=crew")
	code, body = h.call("PATCH", "/api/drawing-boards/plan", map[string]any{"elements": []map[string]any{rect("box", 1, 2, 0)}})
	require.Equal(t, 200, code, body)

	var out struct {
		ContentType string `json:"content_type"`
		Filename    string `json:"filename"`
		Data        []byte `json:"data"`
		Width       int    `json:"width"`
		Version     int64  `json:"version"`
		Elements    int    `json:"elements"`
	}
	code, body = export("plan", map[string]any{"format": "png"})
	require.Equal(t, 200, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, "image/png", out.ContentType)
	assert.Equal(t, "plan.png", out.Filename)
	assert.Equal(t, 1, out.Elements)
	assert.Positive(t, out.Version)
	img, err := png.Decode(bytes.NewReader(out.Data))
	require.NoError(t, err, "the data is the PNG")
	assert.Equal(t, out.Width, img.Bounds().Dx())

	code, body = export("plan", map[string]any{"format": "svg"})
	require.Equal(t, 200, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, "image/svg+xml", out.ContentType)
	assert.Contains(t, string(out.Data), "<svg")

	code, body = export("loose", map[string]any{})
	require.Equal(t, 404, code, "a board in no group is no peer's: %s", body)
	h.trustUnrestricted(t)
	code, body = export("loose", map[string]any{})
	require.Equal(t, 422, code, "unrestricted trust reaches it as this node's operator does: %s", body)
}
