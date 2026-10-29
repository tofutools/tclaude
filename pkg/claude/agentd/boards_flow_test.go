package agentd_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	assert.Equal(t, []string{"a", "b", "c"}, []string{delta.Elements[0].ID, delta.Elements[1].ID, delta.Elements[2].ID},
		"elements keep their creation (stacking) order across edits")
	assert.Equal(t, 5.0, delta.Elements[0].X, "the lower-nonce tie winner is stored")
	assert.Zero(t, delta.Elements[1].X, "a tombstone keeps only what sync needs")

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
		{"id": "z", "type": "rectangle", "version": int64(1) << 53},
		{"id": "z", "type": "rectangle", "version": 1, "versionNonce": int64(1) << 40},
	} {
		rec := serve(http.MethodPatch, "/api/boards/plan-a", map[string]any{"elements": []any{rect("ok", 1, 1, 0), bad}})
		require.Equal(t, http.StatusBadRequest, rec.Code, "element %v body=%s", bad, rec.Body.String())
	}
	assert.Equal(t, int64(3), decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a", nil)).Version)

	// The live-element limit rolls the whole patch back.
	many := make([]map[string]any, 0, 5000)
	for i := range 4999 {
		many = append(many, rect("m"+strconv.Itoa(i), 1, 1, 0))
	}
	rec = serve(http.MethodPatch, "/api/boards/plan-a", map[string]any{"elements": many})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "too_many_elements")
	after := decodeBoard(t, serve(http.MethodGet, "/api/boards/plan-a", nil))
	assert.Equal(t, int64(3), after.Version)
	assert.Len(t, after.Elements, 2, "nothing of the refused patch is applied")

	// A body over 4 MiB is refused outright.
	big := []map[string]any{{"id": "big", "type": "text", "version": 1, "text": strings.Repeat("x", 5<<20)}}
	require.Equal(t, http.StatusRequestEntityTooLarge, serve(http.MethodPatch, "/api/boards/plan-a", map[string]any{"elements": big}).Code)

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
	require.Equal(t, http.StatusOK, put("?version=1", png), "the same version may be re-rendered")
	require.Equal(t, http.StatusConflict, put("?version=0", png), "an older snapshot never replaces a newer one")

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

func TestBoardsEventImageIsARegionAgentsCanFetch(t *testing.T) {
	f := newFlow(t)
	now := time.Unix(1_800_000_000, 0)
	t.Cleanup(agentd.ResetBoardEventsForTest(func() time.Time { return now }))
	const conv = "board-region-3333"
	f.HaveAliveSession(conv, "board-region", "tmux-board-region", f.TestCwd("region"))
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsRead, "test"))

	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, boardDashReq(t, method, path, body))
	}
	agentGet := func(path string) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, path, nil), conv))
	}
	require.Equal(t, http.StatusCreated, serve(http.MethodPost, "/api/boards", map[string]any{"id": "rg", "title": "Regions"}).Code)
	require.Equal(t, http.StatusOK, serve(http.MethodPost, "/api/boards/rg/participants", map[string]any{"agent_id": agentID}).Code)

	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("the-changed-region")...)
	image := base64.StdEncoding.EncodeToString(png)
	require.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/boards/rg/events",
		map[string]any{"text": "drew", "image": base64.StdEncoding.EncodeToString([]byte("GIF89a"))}).Code, "only PNGs")
	require.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/boards/rg/events",
		map[string]any{"text": "drew", "image": "not base64!"}).Code)

	rec := serve(http.MethodPost, "/api/boards/rg/events", map[string]any{"text": "The operator drew a rectangle.", "image": image})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"region":1`)
	rows, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].Body, "tclaude agent board snapshot rg --region 1")

	got := agentGet("/v1/boards/rg/snapshot?region=1")
	require.Equal(t, http.StatusOK, got.Code, "body=%s", got.Body.String())
	assert.Equal(t, png, got.Body.Bytes())
	assert.Equal(t, "1", got.Header().Get("X-Board-Region"))
	assert.Equal(t, http.StatusNotFound, agentGet("/v1/boards/rg/snapshot?region=2").Code)
	assert.Equal(t, http.StatusBadRequest, agentGet("/v1/boards/rg/snapshot?region=x").Code)

	// The next event's image is the next region.
	now = now.Add(3 * time.Second)
	rec = serve(http.MethodPost, "/api/boards/rg/events", map[string]any{"text": "moved it", "image": image})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"region":2`)

	// Only the newest 20 are kept, numbered on past 9 -> 10 numerically.
	for i := 3; i <= 22; i++ {
		now = now.Add(3 * time.Second)
		require.Equal(t, http.StatusOK, serve(http.MethodPost, "/api/boards/rg/events", map[string]any{"text": "again", "image": image}).Code)
	}
	assert.Equal(t, http.StatusNotFound, agentGet("/v1/boards/rg/snapshot?region=2").Code)
	assert.Equal(t, http.StatusOK, agentGet("/v1/boards/rg/snapshot?region=3").Code)
	assert.Equal(t, http.StatusOK, agentGet("/v1/boards/rg/snapshot?region=22").Code)
	now = now.Add(3 * time.Second)
	rec = serve(http.MethodPost, "/api/boards/rg/events", map[string]any{"text": "again", "image": image})
	assert.Contains(t, rec.Body.String(), `"region":23`)

	// Deleting the board deletes its event images; a new board of the same
	// id does not hand out the old board's numbers.
	require.Equal(t, http.StatusNoContent, serve(http.MethodDelete, "/api/boards/rg", nil).Code)
	require.Equal(t, http.StatusCreated, serve(http.MethodPost, "/api/boards", map[string]any{"id": "rg", "title": "Regions"}).Code)
	assert.Equal(t, http.StatusNotFound, agentGet("/v1/boards/rg/snapshot?region=1").Code)
}

func TestBoardsPresenceAgentsAndOperatorView(t *testing.T) {
	f := newFlow(t)
	now := time.Unix(1_800_000_000, 0)
	t.Cleanup(agentd.ResetBoardEventsForTest(func() time.Time { return now }))
	const conv = "board-presence-conv"
	f.HaveConvWithTitle(conv, "presence")
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsRead, "test"))
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsWrite, "test"))
	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, boardDashReq(t, method, path, body))
	}
	agent := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	type presence struct {
		Agents []struct {
			AgentID string `json:"agent_id"`
			State   string `json:"state"`
		} `json:"agents"`
		OperatorView *struct {
			X, Y, Width, Height float64
			AgeSeconds          int `json:"age_seconds"`
		} `json:"operator_view"`
	}
	read := func(rec *httptest.ResponseRecorder) presence {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var out struct{ Presence presence }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out.Presence
	}
	states := func(p presence) map[string]string {
		m := map[string]string{}
		for _, a := range p.Agents {
			m[a.AgentID] = a.State
		}
		return m
	}
	require.Equal(t, http.StatusCreated, serve(http.MethodPost, "/api/boards", map[string]any{"id": "pr", "title": "Presence"}).Code)
	assert.Empty(t, read(serve(http.MethodGet, "/api/boards/pr", nil)).Agents)

	// An agent read is "looking"; an edit is "drawing" for a few seconds.
	read(agent(http.MethodGet, "/v1/boards/pr", nil))
	assert.Equal(t, map[string]string{agentID: "looking"}, states(read(serve(http.MethodGet, "/api/boards/pr?since=0", nil))))
	require.Equal(t, http.StatusOK, agent(http.MethodPatch, "/v1/boards/pr", map[string]any{"elements": []any{rect("n1", 1, 1, 0)}}).Code)
	assert.Equal(t, map[string]string{agentID: "drawing"}, states(read(serve(http.MethodGet, "/api/boards/pr", nil))))
	now = now.Add(10 * time.Second)
	assert.Equal(t, map[string]string{agentID: "looking"}, states(read(serve(http.MethodGet, "/api/boards/pr", nil))))
	now = now.Add(time.Minute)
	assert.Empty(t, read(serve(http.MethodGet, "/api/boards/pr", nil)).Agents)

	// The operator's view reaches agent reads while it is fresh.
	require.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{"x": 0, "y": 0, "width": 0, "height": 10}).Code)
	require.Equal(t, http.StatusNotFound, serve(http.MethodPost, "/api/boards/nope/view", map[string]any{"x": 0, "y": 0, "width": 10, "height": 10}).Code)
	require.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{"x": -40, "y": 10, "width": 800, "height": 600}).Code)
	now = now.Add(5 * time.Second)
	v := read(agent(http.MethodGet, "/v1/boards/pr", nil)).OperatorView
	require.NotNil(t, v)
	assert.Equal(t, []float64{-40, 10, 800, 600}, []float64{v.X, v.Y, v.Width, v.Height})
	assert.Equal(t, 5, v.AgeSeconds)
	// Another editor tab clearing its own view leaves this one.
	require.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{"client": "tab-2"}).Code)
	require.NotNil(t, read(agent(http.MethodGet, "/v1/boards/pr", nil)).OperatorView)
	require.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{"client": "tab-2", "x": 500, "y": 0, "width": 10, "height": 10}).Code)
	assert.Equal(t, 500.0, read(agent(http.MethodGet, "/v1/boards/pr", nil)).OperatorView.X, "the newest view is the operator's")
	require.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{"client": "tab-2"}).Code)
	require.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{}).Code)
	assert.Nil(t, read(agent(http.MethodGet, "/v1/boards/pr", nil)).OperatorView, "{} clears the view")
	require.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/boards/pr/view", map[string]any{"x": 0, "y": 0, "width": 10, "height": 10}).Code)
	now = now.Add(3 * time.Minute)
	assert.Nil(t, read(agent(http.MethodGet, "/v1/boards/pr", nil)).OperatorView, "a stale view is dropped")
}

func TestBoardsBelongToAGroupWhoseMembersGetEvents(t *testing.T) {
	f := newFlow(t)
	now := time.Unix(1_800_000_000, 0)
	t.Cleanup(agentd.ResetBoardEventsForTest(func() time.Time { return now }))
	const (
		member   = "board-grp-member-1111"
		outsider = "board-grp-outsider-2222"
	)
	f.HaveAliveSession(member, "member", "tmux-board-member", f.TestCwd("member"))
	f.HaveConvWithTitle(outsider, "outsider")
	f.HaveGroup("design")
	f.HaveMember("design", member)
	memberID, _, err := db.EnsureAgentForConv(member, "test")
	require.NoError(t, err)
	_, _, err = db.EnsureAgentForConv(outsider, "test")
	require.NoError(t, err)
	for _, conv := range []string{member, outsider} {
		require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsWrite, "test"))
		require.NoError(t, db.GrantAgentPermission(conv, agentd.PermBoardsRead, "test"))
	}
	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(dash, boardDashReq(t, method, path, body))
	}
	agent := func(conv, method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	type groupOut struct {
		Group        string   `json:"group"`
		GroupMembers []string `json:"group_members"`
	}
	group := func(rec *httptest.ResponseRecorder) groupOut {
		t.Helper()
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, "body=%s", rec.Body.String())
		var out groupOut
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	// An agent can only make a board for a group it is in.
	assert.Equal(t, http.StatusForbidden, agent(outsider, http.MethodPost, "/v1/boards", map[string]any{"id": "x", "group": "design"}).Code)
	assert.Equal(t, http.StatusNotFound, agent(member, http.MethodPost, "/v1/boards", map[string]any{"id": "x", "group": "nope"}).Code)
	got := group(agent(member, http.MethodPost, "/v1/boards", map[string]any{"id": "ux", "title": "UX", "group": "design"}))
	assert.Equal(t, groupOut{Group: "design", GroupMembers: []string{memberID}}, got)

	// The operator links and unlinks boards; members get events uninvited.
	require.Equal(t, http.StatusCreated, serve(http.MethodPost, "/api/boards", map[string]any{"id": "flow", "title": "Flow"}).Code)
	assert.Equal(t, http.StatusNotFound, serve(http.MethodPost, "/api/boards/flow/group", map[string]any{"group": "nope"}).Code)
	assert.Equal(t, groupOut{Group: "design", GroupMembers: []string{memberID}}, group(serve(http.MethodPost, "/api/boards/flow/group", map[string]any{"group": "design"})))
	rec := serve(http.MethodPost, "/api/boards/flow/events", map[string]any{"text": "The operator drew a box."})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), memberID, "a group member gets the event without an invitation")
	rows, err := db.ListAgentMessagesForConv(member, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "[board flow]", rows[0].Subject)

	var list struct {
		Boards []struct{ ID, Group string }
	}
	require.NoError(t, json.Unmarshal(serve(http.MethodGet, "/api/boards", nil).Body.Bytes(), &list))
	groups := map[string]string{}
	for _, b := range list.Boards {
		groups[b.ID] = b.Group
	}
	assert.Equal(t, map[string]string{"ux": "design", "flow": "design"}, groups)

	assert.Equal(t, groupOut{}, group(serve(http.MethodPost, "/api/boards/flow/group", map[string]any{"group": ""})))
	now = now.Add(3 * time.Second)
	rec = serve(http.MethodPost, "/api/boards/flow/events", map[string]any{"text": "again"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), memberID, "an unlinked board stops mailing the group")
}
