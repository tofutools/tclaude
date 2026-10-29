package agentd_test

import (
	"bytes"
	"encoding/json"
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

type boardElemOut struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	X             float64 `json:"x"`
	Text          string  `json:"text"`
	Version       int64   `json:"version"`
	IsDeleted     bool    `json:"isDeleted"`
	ServerVersion int64   `json:"server_version"`
	Points        [][]float64
	CustomData    struct {
		Author string `json:"author"`
	} `json:"customData"`
	Junk any `json:"junk"`
}

type boardOut struct {
	ID              string         `json:"id"`
	Title           string         `json:"title"`
	Version         int64          `json:"version"`
	UpdatedBy       string         `json:"updated_by"`
	Participants    []string       `json:"participants"`
	SnapshotVersion *int64         `json:"snapshot_version"`
	Elements        []boardElemOut `json:"elements"`
}

func boardDashReq(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	r := testharness.JSONRequest(t, method, path, body)
	r.Header.Set("Origin", "http://127.0.0.1:0")
	return r
}

func decodeBoard(t *testing.T, rec *httptest.ResponseRecorder) boardOut {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var b boardOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b))
	return b
}

func rect(id string, version, nonce int64, x float64) map[string]any {
	return map[string]any{"id": id, "type": "rectangle", "x": x, "y": 0, "width": 10, "height": 10,
		"version": version, "versionNonce": nonce, "junk": "dropped",
		"customData": map[string]any{"author": "spoofed"}}
}

func TestBoardsDashboardCRUDMergeAndDeltas(t *testing.T) {
	_ = newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, boardDashReq(t, method, path, body))
	}
	patch := func(elems ...map[string]any) (int64, int) {
		rec := serve(http.MethodPatch, "/api/boards/plan-a", map[string]any{"elements": elems})
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var out struct {
			Version  int64 `json:"version"`
			Accepted int   `json:"accepted"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out.Version, out.Accepted
	}

	// Create derives the id from the title; a second create conflicts.
	rec := serve(http.MethodPost, "/api/boards", map[string]any{"title": "Plan A!"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"id":"plan-a"`)
	require.Equal(t, http.StatusConflict, serve(http.MethodPost, "/api/boards", map[string]any{"id": "plan-a", "title": "x"}).Code)
	require.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/boards", map[string]any{"id": "Bad_ID", "title": "x"}).Code)

	// First write: both accepted, version 1, author forced to the operator,
	// unknown keys dropped.
	v, n := patch(rect("a", 1, 50, 0), rect("b", 1, 50, 0))
	require.Equal(t, int64(1), v)
	require.Equal(t, 2, n)
	b := decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a", nil))
	require.Len(t, b.Elements, 2)
	for _, e := range b.Elements {
		assert.Equal(t, "operator", e.CustomData.Author)
		assert.Nil(t, e.Junk)
		assert.Equal(t, int64(1), e.ServerVersion)
	}

	// Lower version loses; an equal version with a higher nonce loses; a
	// patch that accepts nothing leaves the board version alone.
	v, n = patch(rect("a", 0+1, 60, 99))
	require.Equal(t, int64(1), v)
	require.Equal(t, 0, n)
	// Equal version, lower nonce wins; higher version wins.
	v, n = patch(rect("a", 1, 10, 5), rect("b", 2, 999, 7))
	require.Equal(t, int64(2), v)
	require.Equal(t, 2, n)

	// Tombstone b; it disappears from the full read but rides the delta.
	tomb := rect("b", 3, 1, 7)
	tomb["isDeleted"] = true
	v, _ = patch(tomb, rect("c", 1, 1, 0))
	require.Equal(t, int64(3), v)
	full := decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a", nil))
	ids := []string{}
	for _, e := range full.Elements {
		ids = append(ids, e.ID)
	}
	assert.ElementsMatch(t, []string{"a", "c"}, ids)
	delta := decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a?since=2", nil))
	require.Len(t, delta.Elements, 2)
	deleted := map[string]bool{}
	for _, e := range delta.Elements {
		deleted[e.ID] = e.IsDeleted
		assert.Equal(t, int64(3), e.ServerVersion)
	}
	assert.Equal(t, map[string]bool{"b": true, "c": false}, deleted)
	delta = decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a?since=1", nil))
	require.Len(t, delta.Elements, 3)
	assert.Equal(t, 5.0, delta.Elements[0].X, "the lower-nonce tie winner is stored")

	// Validation rejects are all-or-nothing.
	for _, bad := range []map[string]any{
		{"id": "bad id", "type": "rectangle", "version": 1},
		{"id": "z", "type": "star", "version": 1},
		{"id": "z", "type": "rectangle", "version": 0},
		{"id": "z", "type": "rectangle", "version": 1, "x": 2e6},
		{"id": "z", "type": "rectangle", "version": 1, "strokeColor": "red"},
		{"id": "z", "type": "rectangle", "version": 1, "opacity": 101},
		{"id": "z", "type": "text", "version": 1, "text": "bell\x07"},
		{"id": "z", "type": "arrow", "version": 1, "points": [][]float64{{0, 0, 0}}},
	} {
		rec := serve(http.MethodPatch, "/api/boards/plan-a", map[string]any{"elements": []any{rect("ok", 1, 1, 0), bad}})
		require.Equal(t, http.StatusBadRequest, rec.Code, "element %v body=%s", bad, rec.Body.String())
	}
	assert.Equal(t, int64(3), decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a", nil)).Version)

	// List, then delete.
	rec = serve(http.MethodGet, "/api/boards", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"element_count":2`)
	require.Equal(t, http.StatusNoContent, serve(http.MethodDelete, "/api/boards/plan-a", nil).Code)
	require.Equal(t, http.StatusNotFound, serve(http.MethodGet, "/api/boards/plan-a", nil).Code)
	require.Equal(t, http.StatusNotFound, serve(http.MethodPatch, "/api/boards/plan-a", map[string]any{"elements": []any{}}).Code)
}

func TestBoardsAgentRoutesNeedSlugsAndForceAuthor(t *testing.T) {
	f := newFlow(t)
	const conv = "board-agent-conv"
	f.HaveConvWithTitle(conv, "drawer")
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}

	require.Equal(t, http.StatusForbidden, call(http.MethodGet, "/v1/boards", nil).Code)
	require.Equal(t, http.StatusForbidden, call(http.MethodPost, "/v1/boards", map[string]any{"id": "x", "title": "x"}).Code)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsRead, "test"))
	require.Equal(t, http.StatusOK, call(http.MethodGet, "/v1/boards", nil).Code)
	require.Equal(t, http.StatusForbidden, call(http.MethodPost, "/v1/boards", map[string]any{"id": "x", "title": "x"}).Code, "read does not imply write")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsWrite, "test"))

	rec := call(http.MethodPost, "/v1/boards", map[string]any{"id": "arch", "title": "Architecture"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = call(http.MethodPatch, "/v1/boards/arch", map[string]any{"elements": []any{rect("n1", 1, 1, 0)}})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	b := decodeBoard(t, call(http.MethodGet, "/v1/boards/arch", nil))
	require.Len(t, b.Elements, 1)
	assert.Equal(t, agentID, b.Elements[0].CustomData.Author, "the server stamps the calling agent, not the client's value")
	assert.Equal(t, agentID, b.UpdatedBy)
	assert.Equal(t, []string{agentID}, b.Participants, "a patching agent joins the board")

	require.Equal(t, http.StatusNotFound, call(http.MethodGet, "/v1/boards/arch/snapshot", nil).Code, "no snapshot yet")
}

func TestBoardsSnapshotPutGet(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(r *http.Request) *httptest.ResponseRecorder { return testharness.Serve(dash, r) }
	require.Equal(t, http.StatusCreated, serve(boardDashReq(t, http.MethodPost, "/api/boards", map[string]any{"id": "snap", "title": "Snap"})).Code)
	require.Equal(t, http.StatusOK, serve(boardDashReq(t, http.MethodPatch, "/api/boards/snap", map[string]any{"elements": []any{rect("a", 1, 1, 0)}})).Code)
	put := func(query string, body []byte) int {
		r := httptest.NewRequest(http.MethodPut, "/api/boards/snap/snapshot"+query, bytes.NewReader(body))
		r.Header.Set("Origin", "http://127.0.0.1:0")
		r.Header.Set("Content-Type", "image/png")
		return serve(r).Code
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fake-image-data")...)
	require.Equal(t, http.StatusBadRequest, put("?version=1", []byte("GIF89a not a png")), "magic bytes are checked")
	require.Equal(t, http.StatusBadRequest, put("", png), "version is required")
	require.Equal(t, http.StatusBadRequest, put("?version=7", png), "a snapshot cannot be ahead of the board")
	require.Equal(t, http.StatusRequestEntityTooLarge, put("?version=1", append(png, make([]byte, 8<<20)...)))
	require.Equal(t, http.StatusOK, put("?version=1", png))

	rec := serve(boardDashReq(t, http.MethodGet, "/api/boards/snap/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, png, rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))

	// The agent read carries the snapshot's version and the board's.
	const conv = "snap-reader"
	f.HaveConvWithTitle(conv, "snap-reader")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsRead, "test"))
	require.Equal(t, http.StatusOK, serve(boardDashReq(t, http.MethodPatch, "/api/boards/snap", map[string]any{"elements": []any{rect("a", 2, 1, 0)}})).Code)
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/boards/snap/snapshot", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, png, rec.Body.Bytes())
	assert.Equal(t, "1", rec.Header().Get("X-Board-Snapshot-Version"))
	assert.Equal(t, "2", rec.Header().Get("X-Board-Version"))
	b := decodeBoard(t, serve(boardDashReq(t, http.MethodGet, "/api/boards/snap", nil)))
	require.NotNil(t, b.SnapshotVersion)
	assert.Equal(t, int64(1), *b.SnapshotVersion)

	require.Equal(t, http.StatusNoContent, serve(boardDashReq(t, http.MethodDelete, "/api/boards/snap", nil)).Code)
	require.Equal(t, http.StatusNotFound, serve(boardDashReq(t, http.MethodGet, "/api/boards/snap/snapshot", nil)).Code)
}

func TestBoardsEventsMailOnlineParticipantsRateLimited(t *testing.T) {
	f := newFlow(t)
	now := time.Unix(1_800_000_000, 0)
	t.Cleanup(agentd.ResetBoardEventsForTest(func() time.Time { return now }))
	const (
		online  = "board-online-1111"
		offline = "board-offline-2222"
	)
	f.HaveAliveSession(online, "board-online", "tmux-board-online", f.TestCwd("online"))
	f.HaveConvWithTitle(offline, "board-offline")
	onlineID, _, err := db.EnsureAgentForConv(online, "test")
	require.NoError(t, err)
	offlineID, _, err := db.EnsureAgentForConv(offline, "test")
	require.NoError(t, err)

	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, boardDashReq(t, method, path, body))
	}
	require.Equal(t, http.StatusCreated, serve(http.MethodPost, "/api/boards", map[string]any{"id": "ev", "title": "Events"}).Code)
	for _, id := range []string{onlineID, offlineID} {
		rec := serve(http.MethodPost, "/api/boards/ev/participants", map[string]any{"agent_id": id})
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	}
	require.Equal(t, http.StatusNotFound, serve(http.MethodPost, "/api/boards/ev/participants", map[string]any{"agent_id": "agt_nope"}).Code)

	require.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/boards/ev/events", map[string]any{"text": "bad\x1b[31m"}).Code)
	require.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/boards/ev/events", map[string]any{"text": strings.Repeat("x", 2001)}).Code)
	rec := serve(http.MethodPost, "/api/boards/ev/events", map[string]any{"text": "Operator added a box\nnear the API"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, http.StatusTooManyRequests, serve(http.MethodPost, "/api/boards/ev/events", map[string]any{"text": "again"}).Code)

	rows, err := db.ListAgentMessagesForConv(online, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "[board ev]", rows[0].Subject)
	assert.True(t, strings.HasPrefix(rows[0].Body, "Operator added a box\nnear the API\n\n"))
	assert.Contains(t, rows[0].Body, "tclaude agent board show ev")
	assert.True(t, db.IsOperatorAgentMessage(rows[0].ID))
	offlineRows, err := db.ListAgentMessagesForConv(offline, 10)
	require.NoError(t, err)
	assert.Empty(t, offlineRows, "offline participants are not mailed")

	now = now.Add(3 * time.Second)
	require.Equal(t, http.StatusOK, serve(http.MethodPost, "/api/boards/ev/events", map[string]any{"text": "again"}).Code)

	// Removing a participant stops its mail.
	rec = serve(http.MethodPost, "/api/boards/ev/participants", map[string]any{"agent_id": onlineID, "remove": true})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), onlineID)
}
