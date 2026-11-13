package agentd_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// fakeSPA stands in for a single-page app open in Chrome: the first probes
// see the loading curtain with its data request in flight, the rest the
// painted page, unchanged.
type fakeSPA struct {
	probes atomic.Int32
	closed atomic.Bool
	finals atomic.Int32
	mu     sync.Mutex
	scales []float64 // each frame's scale, in order
}

func (f *fakeSPA) frameScales() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.scales...)
}

func (f *fakeSPA) Probe() (agentd.PageProbeForTest, error) {
	n := f.probes.Add(1)
	if n <= 2 {
		return agentd.PageProbeForTest{Loaded: n == 2, InFlight: 1, Requests: 2, Mutations: 5, Text: "curtain", Layout: "l0", Pixels: "p0", Title: "loading", Path: "/"}, nil
	}
	m := 0
	if n == 3 {
		m = 40
	}
	return agentd.PageProbeForTest{Loaded: true, Requests: 2, Mutations: m, Text: "painted", Layout: "l1", Pixels: "p1", Title: "ready", Path: "/"}, nil
}

func (f *fakeSPA) Frame(scale float64) ([]byte, error) {
	f.mu.Lock()
	f.scales = append(f.scales, scale)
	f.mu.Unlock()
	return []byte("frame"), nil
}

func (f *fakeSPA) Final() ([]byte, error) {
	f.finals.Add(1)
	return []byte("final"), nil
}

func (f *fakeSPA) Close() { f.closed.Store(true) }

type watchEvent struct {
	Type      string   `json:"type"`
	Session   string   `json:"session"`
	N         int      `json:"n"`
	ElapsedMS int64    `json:"elapsed_ms"`
	Loaded    bool     `json:"loaded"`
	InFlight  int      `json:"in_flight"`
	Mutations int      `json:"mutations"`
	TextHash  string   `json:"text_hash"`
	PixelHash string   `json:"pixel_hash"`
	Title     string   `json:"title"`
	Changed   []string `json:"changed"`
	StillMS   int64    `json:"still_ms"`
	CalmMS    int64    `json:"calm_ms"`
	Unsettled string   `json:"unsettled"`
	Requests  int      `json:"requests"`
	LongLived int      `json:"long_lived"`
	TextLen   int      `json:"text_len"`
	Elements  int      `json:"elements"`
	Layout    string   `json:"layout_hash"`
	Ready     string   `json:"ready_state"`
	Path      string   `json:"path"`
	Frame     string   `json:"frame"`
	PNG       string   `json:"png"`
	Reason    string   `json:"reason"`
	Code      string   `json:"code"`
	Error     string   `json:"error"`
	MarksSeq  int64    `json:"marks_seq"`
}

// watchStream is a running watch, read line by line as the daemon sends it.
type watchStream struct {
	t      *testing.T
	events chan watchEvent
}

func (e *pageEnv) watch(t *testing.T, a pageAgent, name string, body any) *watchStream {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, a.conv))
	}))
	t.Cleanup(srv.Close)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(srv.URL+"/v1/pages/"+name+"/snapshot/watch", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
	s := &watchStream{t: t, events: make(chan watchEvent, 64)}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var ev watchEvent
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				s.events <- ev
			}
		}
	}()
	return s
}

// next is the next event, or a failure after a while.
func (s *watchStream) next() watchEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		require.True(s.t, ok, "the stream ended")
		return ev
	case <-time.After(10 * time.Second):
		s.t.Fatal("no watch event within 10s")
		return watchEvent{}
	}
}

// ended says the stream closed with nothing more to say.
func (s *watchStream) ended() {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		require.False(s.t, ok, "the stream went on: %+v", ev)
	case <-time.After(10 * time.Second):
		s.t.Fatal("the stream did not end within 10s")
	}
}

func decodePNG(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return string(b)
}

func TestPageSnapshotWatchReportsTicksAndCapturesWhenTheAgentAsks(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "watcher", true)
	other := e.agent(t, "other", true)
	pg, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+pg.ID+"/marks", sampleMarks())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var mu sync.Mutex
	var browsers []*fakeSPA
	var jobs []agentd.PageSnapshotJobForTest
	t.Cleanup(agentd.SetPageSnapshotBrowserForTest(func(j agentd.PageSnapshotJobForTest) (agentd.PageBrowserForTest, error) {
		mu.Lock()
		defer mu.Unlock()
		b := &fakeSPA{}
		browsers = append(browsers, b)
		jobs = append(jobs, j)
		return b, nil
	}))

	s := e.watch(t, a, "app", map[string]any{"every_ms": 100, "for_ms": 60_000, "frame_every": 2, "frame_scale": 0.25})
	start := s.next()
	require.Equal(t, "start", start.Type)
	require.NotEmpty(t, start.Session)
	assert.EqualValues(t, 1, start.MarksSeq, "the final capture draws the latest marks")
	mu.Lock()
	require.Len(t, jobs, 1)
	assert.Contains(t, jobs[0].Overlay, "<rect")
	b := browsers[0]
	mu.Unlock()

	// The page goes from the curtain to painted, then still.
	first := s.next()
	assert.Equal(t, "tick", first.Type)
	assert.Equal(t, 1, first.N)
	assert.Equal(t, 1, first.InFlight)
	assert.Equal(t, "loading", first.Title)
	assert.Contains(t, first.Changed, "net", "a request still loading is a change")
	assert.Equal(t, "frame", decodePNG(t, first.Frame), "a frame with tick 1, 3, 5, ...")
	assert.Equal(t, 0.25, b.frameScales()[0], "tick frames at the scale asked")
	second := s.next()
	assert.Empty(t, second.Frame)
	assert.True(t, second.Loaded)
	var painted, still watchEvent
	for still.Type == "" {
		ev := s.next()
		require.Equal(t, "tick", ev.Type)
		if ev.Title == "ready" && painted.Type == "" {
			painted = ev
			assert.Contains(t, ev.Changed, "text")
			assert.Contains(t, ev.Changed, "pixels")
		}
		if painted.Type != "" && ev.StillMS >= 300 {
			still = ev
		}
	}
	assert.Empty(t, still.Changed)
	assert.Positive(t, still.ElapsedMS)

	// While it watches, the agent's plain snapshot waits its turn.
	rec = e.call(t, a, http.MethodPost, "/v1/pages/app/snapshot", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

	capture := func(who pageAgent, keep bool) *httptest.ResponseRecorder {
		t.Helper()
		return e.call(t, who, http.MethodPost, "/v1/pages/app/snapshot/watch/"+start.Session+"/capture", map[string]any{"keep": keep})
	}
	// Another agent cannot reach the watch, even knowing its session.
	assert.Equal(t, http.StatusNotFound, capture(other, false).Code)

	// A frame on request, and the watch goes on.
	rec = capture(a, true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "frame", out["captured"])
	assert.Equal(t, "frame", decodePNG(t, out["png"].(string)))
	assert.Nil(t, out["marks_seq"], "a frame has no marks drawn")
	scales := b.frameScales()
	assert.Contains(t, scales, 1.0, "a requested frame is full size")
	assert.Contains(t, scales, 0.25)
	assert.Equal(t, "tick", s.next().Type)

	// The final capture: of this open page, with the marks, and it ends the watch.
	rec = capture(a, false)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "final", out["captured"])
	assert.Equal(t, "final", decodePNG(t, out["png"].(string)))
	assert.EqualValues(t, 1, out["marks_seq"])
	assert.GreaterOrEqual(t, out["still_ms"].(float64), 300.0)
	assert.Equal(t, "/checkout?step=2", out["path"])
	e.assertNoToken(t, rec.Body.String())
	var end watchEvent
	for end.Type == "" || end.Type == "tick" {
		end = s.next()
	}
	assert.Equal(t, "end", end.Type)
	assert.Equal(t, "captured", end.Reason)
	s.ended()
	assert.EqualValues(t, 1, b.finals.Load())
	require.Eventually(t, b.closed.Load, 5*time.Second, 10*time.Millisecond, "the browser closes with the watch")
	mu.Lock()
	assert.Len(t, browsers, 1, "the capture reused the watched page")
	mu.Unlock()

	// Once ended, the session is gone, and the agent may snapshot again.
	assert.Equal(t, http.StatusNotFound, capture(a, false).Code)
	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(agentd.PageSnapshotJobForTest) ([]byte, error) { return []byte("png"), nil }))
	require.Eventually(t, func() bool {
		return e.call(t, a, http.MethodPost, "/v1/pages/app/snapshot", nil).Code == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
}

func TestPageSnapshotWatchCapturesAtItsEnd(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "timed", true)
	_, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	b := &fakeSPA{}
	t.Cleanup(agentd.SetPageSnapshotBrowserForTest(func(agentd.PageSnapshotJobForTest) (agentd.PageBrowserForTest, error) {
		return b, nil
	}))

	for _, bad := range []map[string]any{{"frame_scale": 2}, {"frame_scale": 0.01}, {"frame_every": -1}} {
		rec := e.call(t, a, http.MethodPost, "/v1/pages/app/snapshot/watch", bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", bad, rec.Body.String())
	}

	// for_ms below a second is a second: the watch's own clock ends it.
	s := e.watch(t, a, "app", map[string]any{"every_ms": 200, "for_ms": 1})
	require.Equal(t, "start", s.next().Type)
	var ticks int
	var final watchEvent
	for final.Type == "" || final.Type == "tick" {
		final = s.next()
		if final.Type == "tick" {
			ticks++
			assert.Empty(t, final.Frame, "no frames unless asked")
		}
	}
	require.Equal(t, "final", final.Type, "%+v", final)
	assert.Equal(t, "for", final.Reason)
	assert.Equal(t, "final", decodePNG(t, final.PNG))
	assert.GreaterOrEqual(t, final.ElapsedMS, int64(1000))
	assert.GreaterOrEqual(t, ticks, 3)
	s.ended()

	// A page that leaves its origin ends the watch with a clear error.
	t.Cleanup(agentd.SetPageSnapshotBrowserForTest(func(agentd.PageSnapshotJobForTest) (agentd.PageBrowserForTest, error) {
		return leavingPage{&fakeSPA{}}, nil
	}))
	s = e.watch(t, a, "app", map[string]any{"every_ms": 100})
	require.Equal(t, "start", s.next().Type)
	ev := s.next()
	assert.Equal(t, "error", ev.Type)
	assert.Equal(t, "left_origin", ev.Code)
	s.ended()
}

type leavingPage struct{ *fakeSPA }

func (leavingPage) Probe() (agentd.PageProbeForTest, error) {
	return agentd.PageProbeForTest{}, agentd.ErrPageLeftOriginForTest
}
