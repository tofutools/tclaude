package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Live patches (tcl-14z3fq): `page css`, `page dom --set-*` and `page eval`
// through the open views' live sockets, the per-page journal each view
// replays after a reload, and `page patches --reset`.

// pageViewSocket is a dashboard view of a page, as its live socket sees it.
type pageViewSocket struct {
	t    *testing.T
	conn *websocket.Conn
}

func (e *pageEnv) openView(t *testing.T, id string) *pageViewSocket {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/api/pages/"+id+"/live", http.Header{"Origin": {e.srv.URL}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &pageViewSocket{t: t, conn: conn}
}

// next reads the next message, skipping the reachability word.
func (v *pageViewSocket) next() map[string]any {
	v.t.Helper()
	for {
		require.NoError(v.t, v.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		var m map[string]any
		require.NoError(v.t, v.conn.ReadJSON(&m))
		if m["type"] != "reachable" && m["type"] != "unreachable" {
			return m
		}
	}
}

// journal reads the next journal message: its patches' numbers and css.
func (v *pageViewSocket) journal() (msg map[string]any, ns []int, css []string) {
	v.t.Helper()
	msg = v.next()
	require.Equal(v.t, "patches", msg["type"], msg)
	for _, p := range msg["patches"].([]any) {
		pm := p.(map[string]any)
		ns = append(ns, int(pm["n"].(float64)))
		if c, ok := pm["css"].(string); ok {
			css = append(css, c)
		}
	}
	return msg, ns, css
}

type patchOut struct {
	code int
	body map[string]any
	raw  string
}

func (e *pageEnv) async(t *testing.T, a pageAgent, method, path string, body any) chan patchOut {
	done := make(chan patchOut, 1)
	go func() {
		rec := e.call(t, a, method, path, body)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		done <- patchOut{rec.Code, m, rec.Body.String()}
	}()
	return done
}

// waitViews polls the page's patch listing until the server counts n open
// views of it.
func (e *pageEnv) waitViews(t *testing.T, a pageAgent, page string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		rec := e.call(t, a, http.MethodGet, "/v1/pages/"+page+"/patches", nil)
		var out struct {
			Views int `json:"views"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			return false
		}
		return out.Views == n
	}, 10*time.Second, 10*time.Millisecond, "page %s: the server never counted %d open views", page, n)
}

func TestPageLivePatchIsJournaledReplayedAndReset(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "patch", true)
	p, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// No view open: the patch is kept, and applies when one opens.
	rec = e.call(t, a, http.MethodPost, "/v1/pages/app/patches", map[string]any{"kind": "css", "css": "button { color: blue }"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"views":0`)
	assert.Contains(t, rec.Body.String(), `"patch":1`)

	view := e.openView(t, p.ID)
	_, ns, css := view.journal()
	assert.Equal(t, []int{1}, ns, "a view starts with the journal, to replay on its first load")
	assert.Equal(t, []string{"button { color: blue }"}, css)

	// A DOM patch goes to the open view, which answers for it.
	done := e.async(t, a, http.MethodPost, "/v1/pages/app/patches",
		map[string]any{"kind": "dom", "selector": "h1", "op": "set-text", "value": "Pay now"})
	msg, ns, _ := view.journal()
	assert.Equal(t, []int{1, 2}, ns)
	require.NotEmpty(t, msg["req"], "the newest patch is answered for")
	last := msg["patches"].([]any)[1].(map[string]any)
	assert.Equal(t, "h1", last["selector"])
	assert.Equal(t, "Pay now", last["value"])
	require.NoError(t, view.conn.WriteJSON(map[string]any{"type": "patch-result", "req": msg["req"], "applied": 1}))
	out := <-done
	require.Equal(t, http.StatusOK, out.code, out.raw)
	assert.EqualValues(t, 2, out.body["patch"])
	assert.Equal(t, []any{map[string]any{"applied": float64(1)}}, out.body["results"])

	// A reload is a fresh socket: it gets the whole journal again.
	again := e.openView(t, p.ID)
	_, ns, _ = again.journal()
	assert.Equal(t, []int{1, 2}, ns)
	require.NoError(t, again.conn.Close())
	// The server drops the closed view only once its read loop sees the
	// close; until then the eval below could go to it, and the open view
	// would wait for an ask that never comes.
	e.waitViews(t, a, "app", 1)

	// An eval runs once in one view, is listed, and is not replayed.
	done = e.async(t, a, http.MethodPost, "/v1/pages/app/eval", map[string]any{"js": "document.title"})
	ask := view.next()
	require.Equal(t, "eval", ask["type"])
	assert.Equal(t, "document.title", ask["js"])
	require.NoError(t, view.conn.WriteJSON(map[string]any{"type": "eval-result", "req": ask["req"], "json": `"Checkout"`}))
	out = <-done
	require.Equal(t, http.StatusOK, out.code, out.raw)
	assert.Equal(t, "Checkout", out.body["json"])
	assert.EqualValues(t, 3, out.body["patch"])

	// Malformed JSON from the page is not passed on.
	done = e.async(t, a, http.MethodPost, "/v1/pages/app/eval", map[string]any{"js": "1"})
	ask = view.next()
	require.NoError(t, view.conn.WriteJSON(map[string]any{"type": "eval-result", "req": ask["req"], "json": `{"half":`}))
	out = <-done
	require.Equal(t, http.StatusOK, out.code, out.raw)
	assert.Contains(t, out.body["error"], "malformed")

	rec = e.call(t, a, http.MethodGet, "/v1/pages/app/patches", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var journal struct {
		Seq     int64 `json:"seq"`
		Patches []struct {
			N      int64  `json:"n"`
			Kind   string `json:"kind"`
			Replay bool   `json:"replay"`
		} `json:"patches"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &journal))
	require.Len(t, journal.Patches, 4)
	assert.Equal(t, "css", journal.Patches[0].Kind)
	assert.True(t, journal.Patches[1].Replay)
	assert.Equal(t, "eval", journal.Patches[2].Kind)
	assert.False(t, journal.Patches[2].Replay, "an eval ran once")

	// A snapshot renders the page with its replayed patches.
	var jobs []agentd.PageSnapshotJobForTest
	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(j agentd.PageSnapshotJobForTest) ([]byte, error) {
		jobs = append(jobs, j)
		return []byte("\x89PNG"), nil
	}))
	rec = e.call(t, a, http.MethodPost, "/v1/pages/app/snapshot", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"patches":2`)
	require.Len(t, jobs, 1)
	assert.Contains(t, jobs[0].Patches, "button { color: blue }")
	assert.NotContains(t, jobs[0].Patches, "document.title", "an eval is not replayed")

	// Reset #1 drops one patch; the view hears and reloads without it.
	rec = e.call(t, a, http.MethodDelete, "/v1/pages/app/patches?n=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msg, ns, _ = view.journal()
	assert.Equal(t, []int{2}, ns)
	assert.Nil(t, msg["req"], "a reset asks no answer")
	rec = e.call(t, a, http.MethodDelete, "/v1/pages/app/patches?n=1", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = e.call(t, a, http.MethodDelete, "/v1/pages/app/patches", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msg, ns, _ = view.journal()
	assert.Empty(t, ns)
	assert.EqualValues(t, 4, msg["seq"], "the last number stays: the header says the page shows its source again")

	// Closing the page drops the journal.
	require.NoError(t, view.conn.Close())
	rec = e.call(t, a, http.MethodPost, "/v1/pages/app/patches", map[string]any{"kind": "css", "css": "a{}"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusNoContent, e.call(t, a, http.MethodDelete, "/v1/pages/app", nil).Code)
	_, rec = e.show(t, a, ":"+strconv.Itoa(devServer(t, "z")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code)
	rec = e.call(t, a, http.MethodGet, "/v1/pages/app/patches", nil)
	assert.Contains(t, rec.Body.String(), `"patches":[]`)
}

func TestPageLivePatchesAreCheckedAndOwned(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "owner", true)
	b := e.agent(t, "other", true)
	_, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	for _, body := range []map[string]any{
		{"kind": "js", "css": "x"},
		{"kind": "css", "css": "  "},
		{"kind": "dom", "selector": "", "op": "remove"},
		{"kind": "dom", "selector": "a", "op": "outer-html", "value": "x"},
		{"kind": "dom", "selector": "a", "op": "set-attr", "name": "on click", "value": "x"},
		{"kind": "dom", "selector": "a", "op": "insert-html", "value": ""},
		{"kind": "css", "css": strings.Repeat("a", 33<<10)},
	} {
		rec := e.call(t, a, http.MethodPost, "/v1/pages/app/patches", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", body, rec.Body.String())
	}
	rec = e.call(t, a, http.MethodPost, "/v1/pages/app/eval", map[string]any{"js": " "})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Only the page's own agent reaches its journal.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/pages/app/patches"}, {http.MethodGet, "/v1/pages/app/patches"},
		{http.MethodDelete, "/v1/pages/app/patches"}, {http.MethodPost, "/v1/pages/app/eval"},
	} {
		rec := e.call(t, b, c.method, c.path, map[string]any{"kind": "css", "css": "a{}", "js": "1"})
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
	}
	rec = e.call(t, a, http.MethodPost, "/v1/pages/app/eval", map[string]any{"js": "1"})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "not open")
	rec = e.call(t, a, http.MethodGet, "/v1/pages/app/patches", nil)
	assert.Contains(t, rec.Body.String(), `"patches":[]`, "an eval that never ran is not journaled")
}

func TestPageLivePatchOnAPageNotReadBackTellsNothingOfThePage(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "patchguard", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "harness-builtin", siteProfile(a.cwd), claudeSandbox("on", false))
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	view := e.openView(t, p.ID)
	view.journal()
	done := e.async(t, a, http.MethodPost, "/v1/pages/site/patches", map[string]any{"kind": "dom", "selector": "#secret", "op": "remove"})
	// The view never answers: how long it takes is the page's to choose,
	// so the agent's call does not wait on it.
	var out patchOut
	select {
	case out = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a patch on a page that is not read back waited on the view")
	}
	msg, ns, _ := view.journal()
	assert.Equal(t, []int{1}, ns, "the view still gets the patch")
	assert.Nil(t, msg["req"], "and is asked for no answer")
	require.Equal(t, http.StatusOK, out.code, out.raw)
	assert.Equal(t, true, out.body["withheld"])
	assert.EqualValues(t, 1, out.body["views"])
	assert.NotContains(t, out.body, "results")
}
