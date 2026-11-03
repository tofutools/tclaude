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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
	pageSnapshotMaxSide     = 4096
	pageSnapshotSettle      = 300 * time.Millisecond
	pageSnapshotMaxBody     = 4096
	pageSnapshotMaxInFlight = 2
)

var pageSnapshotSlots = make(chan struct{}, pageSnapshotMaxInFlight)

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
}

// pageSnapshotRender renders a job to PNG bytes. A var so flow tests can
// stand in for Chrome.
var pageSnapshotRender = renderPageSnapshotChrome

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
}

// POST /v1/pages/{name}/snapshot — {seq?, no_marks?}: the page as a PNG with
// the latest (or the given) batch of marks.
func handlePageSnapshot(w http.ResponseWriter, r *http.Request) {
	p, ok := ownPage(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pageSnapshotMaxBody)
	var req pageSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	livePages.mu.Lock()
	stage, startPath := p.stage, p.path
	livePages.mu.Unlock()
	origin := stageListenOrigin(stage)
	if origin == "" {
		writeError(w, http.StatusServiceUnavailable, "no_stage", "the dashboard is not listening, so the page has no origin to render")
		return
	}

	view, latest := latestPageView(p.id)
	batch := latest
	if req.Seq > 0 {
		batch = pageMarkBatchBySeq(p.id, req.Seq)
		if batch == nil {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no batch %d of marks is kept for this page", req.Seq))
			return
		}
	}
	if req.NoMarks {
		batch = nil
	}
	job := pageSnapshotJob{
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
	resp := pageSnapshotResponse{Width: job.Width, Height: job.Height, ScrollX: job.ScrollX, ScrollY: job.ScrollY, Path: path}
	if batch != nil {
		job.OverlaySV = pageMarksSVG(batch.Marks)
		resp.MarksSeq = batch.Seq
	}

	select {
	case pageSnapshotSlots <- struct{}{}:
		defer func() { <-pageSnapshotSlots }()
	default:
		writeError(w, http.StatusServiceUnavailable, "busy", "another page snapshot is rendering; try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pageSnapshotTimeout)
	defer cancel()
	png, err := pageSnapshotRender(ctx, job)
	if err != nil {
		code := "snapshot_failed"
		if errors.Is(err, errNoChrome) {
			code = "no_chrome"
		}
		writeError(w, http.StatusServiceUnavailable, code, err.Error())
		return
	}
	resp.PNG = base64.StdEncoding.EncodeToString(png)
	writeJSON(w, http.StatusOK, resp)
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
// background networking, component updates, sync, extensions or proxy.
func chromeArgs(profile string) []string {
	return []string{
		"--headless=new", "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		"--disable-extensions", "--disable-default-apps", "--disable-features=Translate,OptimizationHints,MediaRouter",
		"--no-proxy-server", "--mute-audio", "--hide-scrollbars", "--password-store=basic", "--use-mock-keychain",
		"about:blank",
	}
}

func renderPageSnapshotChrome(ctx context.Context, job pageSnapshotJob) ([]byte, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "tclaude-snapshot-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(profile) }()
	cmd := exec.CommandContext(ctx, bin, chromeArgs(profile)...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(profile, "xdg-config"), "XDG_CACHE_HOME="+filepath.Join(profile, "xdg-cache"))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting Chrome (%s): %w", bin, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()

	wsURL, err := chromeDevToolsURL(ctx, profile, exited)
	if err != nil {
		return nil, err
	}
	conn, err := dialCDP(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	defer conn.close()
	return conn.screenshot(ctx, job)
}

// chromeDevToolsURL waits for Chrome to write its DevTools port into the
// profile (DevToolsActivePort: the port, then the browser's path).
func chromeDevToolsURL(ctx context.Context, profile string, exited <-chan struct{}) (string, error) {
	file := filepath.Join(profile, "DevToolsActivePort")
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if f, err := os.Open(file); err == nil {
			sc := bufio.NewScanner(f)
			var lines []string
			for sc.Scan() {
				lines = append(lines, strings.TrimSpace(sc.Text()))
			}
			_ = f.Close()
			if len(lines) >= 2 && lines[1] != "" {
				if port, err := strconv.Atoi(lines[0]); err == nil && port > 0 && port < 65536 && strings.HasPrefix(lines[1], "/devtools/") {
					return "ws://127.0.0.1:" + lines[0] + lines[1], nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("chrome did not start in time: %w", ctx.Err())
		case <-exited:
			return "", errors.New("chrome exited before it was ready (under a sandbox it may need --no-sandbox: point TCLAUDE_CHROME at a wrapper)")
		case <-tick.C:
		}
	}
}

// ---- a minimal DevTools protocol client ----

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

type cdpConn struct {
	ws      *websocket.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	next    int64
	pending map[int64]chan cdpMessage
	events  chan cdpMessage
	done    chan struct{}
}

func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second} // no proxy
	ws, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to Chrome: %w", err)
	}
	ws.SetReadLimit(64 << 20)
	c := &cdpConn{ws: ws, pending: map[int64]chan cdpMessage{}, events: make(chan cdpMessage, 256), done: make(chan struct{})}
	go c.read()
	return c, nil
}

func (c *cdpConn) close() { _ = c.ws.Close() }

func (c *cdpConn) read() {
	defer close(c.done)
	for {
		var m cdpMessage
		if err := c.ws.ReadJSON(&m); err != nil {
			return
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
		select {
		case c.events <- m:
		default: // nobody needs a flood of events; the ones we wait on are few
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
	c.wmu.Lock()
	err = c.ws.WriteJSON(cdpMessage{ID: id, Method: method, SessionID: session, Params: raw})
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

// snapshotHeadersFor decides what a paused request goes on with: the page's
// own documents get the snapshot token; anything else goes unchanged (nil).
func snapshotHeadersFor(requestURL, resourceType, origin, token string, headers map[string]string) []map[string]string {
	if resourceType != "Document" || token == "" {
		return nil
	}
	u, err := url.Parse(requestURL)
	if err != nil || u.Scheme+"://"+u.Host != origin {
		return nil
	}
	out := make([]map[string]string, 0, len(headers)+1)
	for k, v := range headers {
		if strings.EqualFold(k, stageSnapshotHeader) {
			continue
		}
		out = append(out, map[string]string{"name": k, "value": v})
	}
	return append(out, map[string]string{"name": stageSnapshotHeader, "value": token})
}

func (c *cdpConn) screenshot(ctx context.Context, job pageSnapshotJob) ([]byte, error) {
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return nil, err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	s := attached.SessionID

	// The page's documents pause here for the token; everything else is not
	// intercepted at all.
	if err := c.call(ctx, s, "Fetch.enable", map[string]any{"patterns": []map[string]any{
		{"urlPattern": job.Origin + "/*", "resourceType": "Document", "requestStage": "Request"},
	}}, nil); err != nil {
		return nil, err
	}
	for _, step := range []struct {
		method string
		params any
	}{
		{"Page.enable", map[string]any{}},
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": job.Width, "height": job.Height, "deviceScaleFactor": 1, "mobile": false}},
	} {
		if err := c.call(ctx, s, step.method, step.params, nil); err != nil {
			return nil, err
		}
	}

	loaded := make(chan struct{})
	pump := make(chan struct{})
	pumpCtx, stopPump := context.WithCancel(ctx)
	defer func() { stopPump(); <-pump }()
	go func() {
		defer close(pump)
		once := sync.Once{}
		for {
			select {
			case <-pumpCtx.Done():
				return
			case <-c.done:
				return
			case ev := <-c.events:
				if ev.SessionID != s {
					continue
				}
				switch ev.Method {
				case "Fetch.requestPaused":
					var p struct {
						RequestID    string `json:"requestId"`
						ResourceType string `json:"resourceType"`
						Request      struct {
							URL     string            `json:"url"`
							Headers map[string]string `json:"headers"`
						} `json:"request"`
					}
					if json.Unmarshal(ev.Params, &p) != nil {
						continue
					}
					params := map[string]any{"requestId": p.RequestID}
					if h := snapshotHeadersFor(p.Request.URL, p.ResourceType, job.Origin, job.Token, p.Request.Headers); h != nil {
						params["headers"] = h
					}
					go func() { _ = c.call(pumpCtx, s, "Fetch.continueRequest", params, nil) }()
				case "Page.loadEventFired":
					once.Do(func() { close(loaded) })
				}
			}
		}
	}()

	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := c.call(ctx, s, "Page.navigate", map[string]any{"url": job.URL}, &nav); err != nil {
		return nil, err
	}
	if nav.ErrorText != "" {
		return nil, fmt.Errorf("chrome could not load the page: %s", nav.ErrorText)
	}
	select {
	case <-loaded:
	case <-ctx.Done():
		return nil, fmt.Errorf("the page did not finish loading: %w", ctx.Err())
	}
	select {
	case <-time.After(pageSnapshotSettle):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	overlay, _ := json.Marshal(job.OverlaySV)
	expr := fmt.Sprintf(`(function(){
  window.scrollTo(%d, %d);
  var svg = %s;
  if (svg) {
    var host = document.createElement('div');
    host.setAttribute('data-tclaude-marks', '');
    host.style.cssText = 'position:absolute;left:0;top:0;width:0;height:0;overflow:visible;z-index:2147483647;pointer-events:none';
    host.innerHTML = svg;
    document.documentElement.appendChild(host);
  }
  return new Promise(function(r){ requestAnimationFrame(function(){ requestAnimationFrame(function(){ r(true); }); }); });
})()`, job.ScrollX, job.ScrollY, overlay)
	if err := c.call(ctx, s, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true}, nil); err != nil {
		return nil, err
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := c.call(ctx, s, "Page.captureScreenshot", map[string]any{"format": "png"}, &shot); err != nil {
		return nil, err
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return nil, fmt.Errorf("chrome sent an unreadable screenshot: %w", err)
	}
	return png, nil
}
