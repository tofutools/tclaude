package agentd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// `tclaude agent page snapshot` (tcl-2dwbad): the daemon renders one of the
// agent's pages in a headless Chrome on the host, at the operator's viewport
// and scroll, with the operator's marks drawn on top, and hands back a PNG.
//
// Chrome is the host's own: found on PATH (or TCLAUDE_CHROME), never
// downloaded. Without one the snapshot fails with an error that says so.
// Chrome runs with a throwaway profile and its background networking off,
// and talks to the daemon over the DevTools protocol on a loopback port; the
// client below speaks just the handful of commands a screenshot needs.
//
// The stage serves a document only to a frame (Sec-Fetch-Dest: iframe) or to
// a request carrying the page's snapshot token (stage.go). The token goes on
// the page's own document requests only, added by intercepting them
// (Fetch.requestPaused); it is never set as an extra header for every
// request, which would hand it to any other host the page loads from.

const (
	pageSnapshotTimeout     = 45 * time.Second
	pageSnapshotDefaultW    = 1280
	pageSnapshotDefaultH    = 800
	pageSnapshotMinSide     = 200
	pageSnapshotMaxSide     = 2560
	pageSnapshotMaxBody     = 4096
	pageSnapshotMaxInFlight = 2
)

var pageSnapshotSlots = make(chan struct{}, pageSnapshotMaxInFlight)

// pageSnapshotBusy holds the agents with a snapshot rendering: one at a time
// each, so one agent cannot hold every slot.
var pageSnapshotBusy = struct {
	mu sync.Mutex
	by map[string]bool
}{by: map[string]bool{}}

// pageSnapshotJob is what a render needs.
type pageSnapshotJob struct {
	URL       string // the page's stage URL at the path to render
	Origin    string // the page's origin: only its documents get the token
	Token     string // the page's snapshot token
	Width     int
	Height    int
	ScrollX   int
	ScrollY   int
	OverlaySV string // an SVG of the marks, in document coordinates ("" for none)
	Patches   string // the page's replayed live patches, a JSON array (tcl-14z3fq)
	pageID    string // the page's id, for the watch registry
}

// pageSnapshotRender renders a job to PNG bytes once the page has gone calm
// (capped). A var so flow tests can stand in for Chrome.
var pageSnapshotRender = renderPageSnapshot

type pageSnapshotRequest struct {
	Seq     int64 `json:"seq"`
	NoMarks bool  `json:"no_marks"`
}

type pageSnapshotResponse struct {
	PNG      string `json:"png"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	ScrollX  int    `json:"scroll_x"`
	ScrollY  int    `json:"scroll_y"`
	Path     string `json:"path"`
	MarksSeq int64  `json:"marks_seq,omitempty"`
	// Patches is how many live patches the page was rendered with.
	Patches int `json:"patches,omitempty"`
	// Settle is how the wait for the page to go calm went.
	Settle *pageSettle `json:"settle,omitempty"`
	// WatchMS is how long the page had been watched, for a watch's capture.
	WatchMS int64 `json:"watch_ms,omitempty"`
	// StillMS is how long the page had not changed, for a watch's capture.
	StillMS *int64 `json:"still_ms,omitempty"`
	// CalmMS is how long the page had been calm, for a watch's capture.
	CalmMS *int64 `json:"calm_ms,omitempty"`
	// Captured is final (the capture that ended a watch) or frame (a frame
	// of a watch, which goes on), for a watch's capture.
	Captured string `json:"captured,omitempty"`
}

// POST /v1/pages/{name}/snapshot — {seq?, no_marks?}: the page as a PNG with
// the latest (or the given) batch of marks, once it has gone calm (capped).
func handlePageSnapshot(w http.ResponseWriter, r *http.Request) {
	var req pageSnapshotRequest
	t, ok := pageSnapshotPrepare(w, r, &req)
	if !ok {
		return
	}
	job, resp := t.job, t.resp
	release, ok := pageSnapshotAcquire(w, t.owner)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), pageSnapshotTimeout)
	defer cancel()
	png, settle, err := pageSnapshotRender(ctx, job)
	if err != nil {
		writePageSnapshotError(w, err)
		return
	}
	resp.PNG = base64.StdEncoding.EncodeToString(png)
	resp.Settle = &settle
	writeJSON(w, http.StatusOK, resp)
}

func writePageSnapshotError(w http.ResponseWriter, err error) {
	code, status := "snapshot_failed", http.StatusServiceUnavailable
	switch {
	case errors.Is(err, errNoChrome):
		code = "no_chrome"
	case errors.Is(err, errPageNotLoaded):
		code, status = "not_loaded", http.StatusConflict
	}
	writeError(w, status, code, err.Error())
}

// pageSnapshotTarget is what a snapshot renders, and for whom.
type pageSnapshotTarget struct {
	job    pageSnapshotJob
	resp   pageSnapshotResponse
	owner  string // the page's agent
	caller string // the agent asking (a group member, for a shared page)
}

// pageSnapshotPrepare reads the request into req and works out what to
// render: the page, its path, viewport, scroll, marks and patches. It writes
// the error itself when it says no.
func pageSnapshotPrepare(w http.ResponseWriter, r *http.Request, req any) (t pageSnapshotTarget, ok bool) {
	p, _, caller, ok := pageForAs(w, r, PermGroupsPagesRead)
	if !ok {
		return t, false
	}
	var job pageSnapshotJob
	var resp pageSnapshotResponse
	r.Body = http.MaxBytesReader(w, r.Body, pageSnapshotMaxBody)
	if err := json.NewDecoder(r.Body).Decode(req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return t, false
	}
	var base pageSnapshotRequest
	switch v := req.(type) {
	case *pageSnapshotRequest:
		base = *v
	case *pageWatchRequest:
		base = v.pageSnapshotRequest
	}
	livePages.mu.Lock()
	stage, startPath, kind, owner := p.stage, p.path, p.kind, p.owner
	livePages.mu.Unlock()
	if err := pageReadBackAllowed(kind, stage, false); err != nil {
		writeError(w, http.StatusConflict, "static_source", err.Error())
		return t, false
	}
	if stage.otherHostsAllowed() {
		writeError(w, http.StatusConflict, "other_hosts", "this page may load other hosts (the operator allowed it), so it is not rendered for you: "+
			"a snapshot would show what the daemon's network reaches")
		return t, false
	}
	origin := stageListenOrigin(stage)
	if origin == "" {
		writeError(w, http.StatusServiceUnavailable, "no_stage", "the dashboard is not listening, so the page has no origin to render")
		return t, false
	}

	view, latest := latestPageView(p.id)
	batch := latest
	if base.Seq > 0 {
		batch = pageMarkBatchBySeq(p.id, base.Seq)
		if batch == nil {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no batch %d of marks is kept for this page", base.Seq))
			return t, false
		}
	}
	if base.NoMarks {
		batch = nil
	}
	// Marks are drawn where they were drawn: at their batch's path, viewport
	// and scroll. Without marks, the page is rendered as the operator last saw it.
	if batch != nil {
		view = pageViewReport{VW: batch.Viewport.W, VH: batch.Viewport.H, X: batch.Scroll.X, Y: batch.Scroll.Y, Path: batch.Path}
	}
	job = pageSnapshotJob{
		Origin: origin, Token: stage.snapshotToken,
		Width: clampSnapshotSide(view.VW, pageSnapshotDefaultW), Height: clampSnapshotSide(view.VH, pageSnapshotDefaultH),
		ScrollX: view.X, ScrollY: view.Y,
	}
	path := view.Path
	if !pageStartPathOK(path) {
		path = startPath
	}
	if !pageStartPathOK(path) {
		path = "/"
	}
	job.URL = origin + path
	resp = pageSnapshotResponse{Width: job.Width, Height: job.Height, ScrollX: job.ScrollX, ScrollY: job.ScrollY, Path: path}
	if batch != nil {
		job.OverlaySV = pageMarksSVG(batch.Marks)
		resp.MarksSeq = batch.Seq
	}
	// The page as the operator sees it: with its live patches.
	job.Patches, resp.Patches = pagePatchesForSnapshot(p.id)
	job.pageID = p.id
	return pageSnapshotTarget{job: job, resp: resp, owner: owner, caller: caller}, true
}

// pageSnapshotAcquire takes the page owner's one render and one of the
// host's few, or writes why not.
func pageSnapshotAcquire(w http.ResponseWriter, owner string) (release func(), ok bool) {
	pageSnapshotBusy.mu.Lock()
	if pageSnapshotBusy.by[owner] {
		pageSnapshotBusy.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "busy", "your previous page snapshot (or a watch) is still running")
		return nil, false
	}
	pageSnapshotBusy.by[owner] = true
	pageSnapshotBusy.mu.Unlock()
	free := func() {
		pageSnapshotBusy.mu.Lock()
		delete(pageSnapshotBusy.by, owner)
		pageSnapshotBusy.mu.Unlock()
	}
	select {
	case pageSnapshotSlots <- struct{}{}:
		return func() { <-pageSnapshotSlots; free() }, true
	default:
		free()
		writeError(w, http.StatusServiceUnavailable, "busy", "another page snapshot is rendering; try again")
		return nil, false
	}
}

func clampSnapshotSide(v, def int) int {
	if v <= 0 {
		return def
	}
	return min(max(v, pageSnapshotMinSide), pageSnapshotMaxSide)
}

func pageMarkBatchBySeq(id string, seq int64) *pageMarkBatch {
	pageCollab.mu.Lock()
	defer pageCollab.mu.Unlock()
	if s := pageCollab.byID[id]; s != nil {
		for i := range s.batches {
			if s.batches[i].Seq == seq {
				b := s.batches[i]
				return &b
			}
		}
	}
	return nil
}

// stageListenOrigin is the page's origin on the running stage listener, as
// the dashboard frames it ("" when the stage is not listening).
func stageListenOrigin(stage *stagePage) string {
	if stage == nil {
		return ""
	}
	return pageOrigin(stage.token)
}

// pageMarksSVG draws a batch's marks as SVG in document coordinates, as the
// dashboard's layer does. Colours are checked marks' colours; text is
// escaped.
func pageMarksSVG(marks []pageMark) string {
	if len(marks) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1" style="position:absolute;left:0;top:0;overflow:visible;pointer-events:none">`)
	for _, m := range marks {
		color := m.Color
		if !pageMarkColor.MatchString(color) {
			color = "#e03131"
		}
		stroke := fmt.Sprintf(`fill="none" stroke="%s" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"`, color)
		pts := m.Points
		switch m.Kind {
		case "box":
			if len(pts) != 2 {
				continue
			}
			x, y := min(pts[0][0], pts[1][0]), min(pts[0][1], pts[1][1])
			w, h := absInt(pts[1][0]-pts[0][0]), absInt(pts[1][1]-pts[0][1])
			fmt.Fprintf(&sb, `<rect x="%d" y="%d" width="%d" height="%d" rx="%d" %s/>`, x, y, w, h, min(10, w/4, h/4), stroke)
		case "pen":
			sb.WriteString(`<polyline points="`)
			for i, p := range pts {
				if i > 0 {
					sb.WriteByte(' ')
				}
				fmt.Fprintf(&sb, "%d,%d", p[0], p[1])
			}
			fmt.Fprintf(&sb, `" %s/>`, stroke)
		case "arrow":
			if len(pts) < 2 {
				continue
			}
			a, b := pts[len(pts)-2], pts[len(pts)-1]
			angle := math.Atan2(float64(b[1]-a[1]), float64(b[0]-a[0]))
			const size = 14.5
			hx := func(d float64) float64 { return float64(b[0]) - size*math.Cos(angle+d) }
			hy := func(d float64) float64 { return float64(b[1]) - size*math.Sin(angle+d) }
			fmt.Fprintf(&sb, `<path d="M%d,%d L%d,%d M%.1f,%.1f L%d,%d L%.1f,%.1f" %s/>`,
				pts[0][0], pts[0][1], b[0], b[1], hx(-0.45), hy(-0.45), b[0], b[1], hx(0.45), hy(0.45), stroke)
		case "text":
			fmt.Fprintf(&sb, `<text x="%d" y="%d" font-size="20" font-family="system-ui,sans-serif" fill="%s" stroke="#fff" stroke-width="4" paint-order="stroke">%s</text>`,
				pts[0][0], pts[0][1]+20, color, html.EscapeString(m.Text))
		}
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

var errPageLeftOrigin = errors.New("the page tried to load a document from outside its own origin (a redirect, a navigation or a frame); " +
	"a snapshot renders only the page itself")

// ---- Chrome ----

var errNoChrome = errors.New("no Chrome or Chromium found on this host for page snapshots: install one, or set TCLAUDE_CHROME " +
	"to its binary (tclaude never downloads a browser)")

// findChrome finds the host's Chrome: TCLAUDE_CHROME, then the usual names on
// PATH, then the macOS app bundles.
func findChrome() (string, error) {
	if v := strings.TrimSpace(os.Getenv("TCLAUDE_CHROME")); v != "" {
		if p, err := exec.LookPath(v); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("TCLAUDE_CHROME=%q is not an executable", v)
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	return "", errNoChrome
}

// chromeArgs start a headless Chrome on a throwaway profile, quiet: no
// background networking, component updates, sync, extensions or proxy. The
// DevTools protocol runs over a pipe (fds 3 and 4), never a port: a port on
// loopback would let any local process drive the browser and read the
// snapshot token from its requests.
func chromeArgs(profile string) []string {
	return []string{
		"--headless=new", "--remote-debugging-pipe",
		"--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		"--disable-extensions", "--disable-default-apps", "--block-new-web-contents", "--disable-features=Translate,OptimizationHints,MediaRouter",
		"--no-proxy-server", "--mute-audio", "--hide-scrollbars", "--password-store=basic", "--use-mock-keychain",
		"about:blank",
	}
}

// ---- a minimal DevTools protocol client, over Chrome's pipe ----

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// cdpMaxMessage bounds one message from Chrome (a screenshot is the largest).
const cdpMaxMessage = 64 << 20

type cdpConn struct {
	in      io.WriteCloser // Chrome's fd 3
	out     io.ReadCloser  // Chrome's fd 4
	wmu     sync.Mutex
	mu      sync.Mutex
	next    int64
	pending map[int64]chan cdpMessage
	events  chan cdpMessage
	done    chan struct{}
	quit    chan struct{} // closed by close: an undelivered event is dropped
	once    sync.Once
}

func newCDPConn(in io.WriteCloser, out io.ReadCloser) *cdpConn {
	c := &cdpConn{in: in, out: out, pending: map[int64]chan cdpMessage{}, events: make(chan cdpMessage, 1024),
		done: make(chan struct{}), quit: make(chan struct{})}
	go c.read()
	return c
}

func (c *cdpConn) close() {
	c.once.Do(func() {
		close(c.quit)
		_ = c.in.Close()
		_ = c.out.Close()
	})
}

// read takes Chrome's NUL-separated JSON messages off the pipe.
func (c *cdpConn) read() {
	defer close(c.done)
	r := bufio.NewReaderSize(c.out, 64<<10)
	var buf []byte
	for {
		chunk, err := r.ReadSlice(0)
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(buf) > cdpMaxMessage {
				return
			}
			continue
		}
		if err != nil {
			return
		}
		raw := buf[:len(buf)-1]
		buf = nil
		var m cdpMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		// Every event is delivered: the page's request count and its paused
		// requests both depend on it. The pump drains them for the page's life.
		select {
		case c.events <- m:
		case <-c.quit:
			return
		}
	}
}

func (c *cdpConn) call(ctx context.Context, session, method string, params any, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan cdpMessage, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	msg, err := json.Marshal(cdpMessage{ID: id, Method: method, SessionID: session, Params: raw})
	if err != nil {
		return err
	}
	c.wmu.Lock()
	_, err = c.in.Write(append(msg, 0))
	c.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("chrome %s: %w", method, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("chrome %s: %s", method, m.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.done:
		return fmt.Errorf("chrome %s: the connection closed", method)
	case <-ctx.Done():
		return fmt.Errorf("chrome %s: %w", method, ctx.Err())
	}
}

// snapshotRequest decides what a paused request does. The snapshot renders
// the page's own origin and nothing else: a request anywhere else (a
// redirect, a navigation, a frame, an image) is failed, so the agent never
// gets pixels of another host the daemon's network reaches. The page's own
// documents go on with the snapshot token; its other requests go on as they
// are (nil headers).
func snapshotRequest(requestURL, resourceType, origin, token string, headers map[string]string) (allow bool, withHeaders []map[string]string) {
	u, err := url.Parse(requestURL)
	if err != nil || u.Scheme+"://"+u.Host != origin {
		return false, nil
	}
	if resourceType != "Document" || token == "" {
		return true, nil
	}
	out := make([]map[string]string, 0, len(headers)+1)
	for k, v := range headers {
		if strings.EqualFold(k, stageSnapshotHeader) {
			continue
		}
		out = append(out, map[string]string{"name": k, "value": v})
	}
	return true, append(out, map[string]string{"name": stageSnapshotHeader, "value": token})
}
