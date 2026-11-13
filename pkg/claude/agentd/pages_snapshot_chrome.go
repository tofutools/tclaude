package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// One page open in a headless Chrome, for `page snapshot` and its watch mode
// (tk_83873896). The page is opened once and then probed: each probe reads
// what is changing (requests in flight, DOM mutations, hashes of the visible
// text, the layout and the pixels), so a snapshot waits for the page to go
// still, or the agent watches it and chooses when to capture. Final draws the
// operator's patches and marks and captures; nothing is probed after it.

// pageSnapshotBrowser is one page open in Chrome. A var-backed opener
// (pageSnapshotOpen) lets flow tests stand in for Chrome.
type pageSnapshotBrowser interface {
	// Probe reads what the page is doing now. pixels adds a hash of a small
	// screenshot, which a watch reports and the default wait skips.
	Probe(ctx context.Context, pixels bool) (pageProbe, error)
	// Frame is a screenshot of the page as it is, at scale (0 < scale <= 1).
	Frame(ctx context.Context, scale float64) ([]byte, error)
	// Final applies the page's live patches, scrolls to the operator's
	// scroll, draws the marks and captures. The page is not probed after.
	Final(ctx context.Context) ([]byte, error)
	Close()
}

// pageProbe is one look at an open page.
type pageProbe struct {
	Loaded     bool   // the navigation's load event fired
	ReadyState string // document.readyState
	Path       string
	Title      string
	InFlight   int // requests started and not finished
	// LongLived is how many of InFlight are an EventSource or have been open
	// longer than pageSnapshotLongRequest: a long-poll, a stream. They do not
	// keep a page from being still.
	LongLived int
	Requests  int // requests started since the page opened
	Mutations int // DOM mutation records since the last probe (-1: unknown)
	TextHash  string
	TextLen   int
	Layout    string // a hash of the boxes of the visible elements
	Pixels    string // a hash of a small screenshot ("" when not asked)
	Note      string // why a probe read nothing (the page is between documents)
}

// pageSnapshotLongRequest is how long a request may be open before it counts
// as long-lived (a long-poll or a stream) rather than as loading.
const pageSnapshotLongRequest = 3 * time.Second

var pageSnapshotOpen = openPageSnapshotChrome

// pageProbeInstallJS counts DOM mutations from the first byte of every
// document the page loads.
const pageProbeInstallJS = `(function(){
  if (window.__tclaudeWatch) return;
  var w = {m: 0};
  try { Object.defineProperty(window, '__tclaudeWatch', {value: w}); } catch (e) { return; }
  new MutationObserver(function(rs){ w.m += rs.length; }).observe(document, {subtree: true, childList: true, attributes: true, characterData: true});
})()`

// pageProbeJS reads the page: mutations since the last read, the visible
// text and layout as hashes, the title and the path.
const pageProbeJS = `(function(){
  var w = window.__tclaudeWatch, m = w ? w.m : -1;
  if (w) w.m = 0;
  function h(s) {
    var a = 0x811c9dc5 | 0, b = 0x9e3779b9 | 0;
    for (var i = 0; i < s.length; i++) { var c = s.charCodeAt(i); a = Math.imul(a ^ c, 16777619); b = Math.imul(b ^ c, 2246822519); }
    return ('0000000' + (a >>> 0).toString(16)).slice(-8) + ('000' + (b >>> 0).toString(16)).slice(-4);
  }
  var body = document.body, text = body ? body.innerText || '' : '';
  var boxes = [], els = body ? body.getElementsByTagName('*') : [], n = Math.min(els.length, 5000);
  for (var i = 0; i < n; i++) {
    var r = els[i].getBoundingClientRect();
    if (r.width || r.height) boxes.push(Math.round(r.x) + ',' + Math.round(r.y) + ',' + Math.round(r.width) + ',' + Math.round(r.height));
  }
  return JSON.stringify({m: m, rs: document.readyState, o: location.origin, p: location.pathname + location.search + location.hash,
    t: document.title, th: h(text), tl: text.length, lh: h(boxes.join(';')), sx: Math.round(scrollX), sy: Math.round(scrollY),
    vw: innerWidth, vh: innerHeight});
})()`

type chromeRequest struct {
	start time.Time
	long  bool // an EventSource
}

type chromePage struct {
	cmd     *exec.Cmd
	conn    *cdpConn
	s       string // the page's DevTools session
	job     pageSnapshotJob
	profile string
	exited  chan struct{}
	pump    chan struct{}
	stop    context.CancelFunc
	once    sync.Once

	loaderID string

	mu       sync.Mutex
	loadedBy map[string]bool
	inflight map[string]chromeRequest
	requests int
	left     bool // a document from elsewhere was blocked: the page is leaving its origin
	final    bool
}

func openPageSnapshotChrome(ctx context.Context, job pageSnapshotJob) (pageSnapshotBrowser, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "tclaude-snapshot-")
	if err != nil {
		return nil, err
	}
	// fd 3: Chrome reads commands; fd 4: Chrome writes answers and events.
	toChrome, chromeIn, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(profile)
		return nil, err
	}
	chromeOut, fromChrome, err := os.Pipe()
	if err != nil {
		_ = toChrome.Close()
		_ = chromeIn.Close()
		_ = os.RemoveAll(profile)
		return nil, err
	}
	cmd := exec.Command(bin, chromeArgs(profile)...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(profile, "xdg-config"), "XDG_CACHE_HOME="+filepath.Join(profile, "xdg-cache"))
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	chromeProcessGroup(cmd)
	err = cmd.Start()
	_ = toChrome.Close()
	_ = fromChrome.Close()
	if err != nil {
		_ = chromeIn.Close()
		_ = chromeOut.Close()
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("starting Chrome (%s): %w", bin, err)
	}
	p := &chromePage{cmd: cmd, conn: newCDPConn(chromeIn, chromeOut), job: job, profile: profile, exited: make(chan struct{}),
		pump: make(chan struct{}), loadedBy: map[string]bool{}, inflight: map[string]chromeRequest{}}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	go func() {
		select {
		case <-ctx.Done():
			killChrome(cmd)
		case <-p.exited:
		}
	}()
	if err := p.open(ctx); err != nil {
		err = p.why(ctx, err)
		p.Close()
		return nil, err
	}
	return p, nil
}

// why adds a hint when Chrome died under the call.
func (p *chromePage) why(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, errPageLeftOrigin) {
		return err
	}
	select {
	case <-p.exited:
		if ctx.Err() == nil {
			return fmt.Errorf("chrome exited during the snapshot (under a sandbox it may need --no-sandbox: point TCLAUDE_CHROME at a wrapper): %w", err)
		}
	default:
	}
	return err
}

func (p *chromePage) Close() {
	p.once.Do(func() {
		if p.stop != nil {
			p.stop()
			<-p.pump
		}
		p.conn.close()
		killChrome(p.cmd)
		<-p.exited
		_ = os.RemoveAll(p.profile)
	})
}

func (p *chromePage) open(ctx context.Context) error {
	c := p.conn
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return err
	}
	s := attached.SessionID
	p.s = s

	// The pump runs from before the first command that can pause a request:
	// every request pauses (redirect hops pause again), and the page's own go
	// on, its documents with the token; anything else is failed.
	pumpCtx, stop := context.WithCancel(context.Background())
	p.stop = stop
	go p.runPump(pumpCtx)

	for _, step := range []struct {
		method string
		params any
	}{
		{"Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}}}},
		{"Network.enable", map[string]any{}},
		{"Page.enable", map[string]any{}},
		{"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": pageProbeInstallJS}},
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": p.job.Width, "height": p.job.Height, "deviceScaleFactor": 1, "mobile": false}},
	} {
		if err := c.call(ctx, s, step.method, step.params, nil); err != nil {
			return err
		}
	}
	var nav struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := c.call(ctx, s, "Page.navigate", map[string]any{"url": p.job.URL}, &nav); err != nil {
		return err
	}
	if nav.ErrorText == "net::ERR_BLOCKED_BY_CLIENT" {
		return errPageLeftOrigin
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("chrome could not load the page: %s", nav.ErrorText)
	}
	p.mu.Lock()
	p.loaderID = nav.LoaderID
	p.mu.Unlock()
	return nil
}

// runPump answers paused requests and records the navigation's load and the
// page's requests. Events can come before Page.navigate answers, so the load
// is recorded by loader id: a late event from about:blank must not count.
func (p *chromePage) runPump(ctx context.Context) {
	defer close(p.pump)
	c := p.conn
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case ev := <-c.events:
			if ev.SessionID == "" || ev.SessionID != p.s {
				continue
			}
			switch ev.Method {
			case "Fetch.requestPaused":
				var e struct {
					RequestID    string `json:"requestId"`
					ResourceType string `json:"resourceType"`
					Request      struct {
						URL     string            `json:"url"`
						Headers map[string]string `json:"headers"`
					} `json:"request"`
				}
				if json.Unmarshal(ev.Params, &e) != nil {
					continue
				}
				params := map[string]any{"requestId": e.RequestID}
				method := "Fetch.continueRequest"
				allow, h := snapshotRequest(e.Request.URL, e.ResourceType, p.job.Origin, p.job.Token, e.Request.Headers)
				switch {
				case !allow:
					method = "Fetch.failRequest"
					params["errorReason"] = "BlockedByClient"
					if e.ResourceType == "Document" {
						p.mu.Lock()
						p.left = true
						p.mu.Unlock()
					}
				case h != nil:
					params["headers"] = h
				}
				go func() { _ = c.call(ctx, p.s, method, params, nil) }()
			case "Network.requestWillBeSent":
				var e struct {
					RequestID string `json:"requestId"`
					Type      string `json:"type"`
				}
				if json.Unmarshal(ev.Params, &e) != nil {
					continue
				}
				p.mu.Lock()
				if _, seen := p.inflight[e.RequestID]; !seen { // a redirect hop keeps its id
					p.requests++
					p.inflight[e.RequestID] = chromeRequest{start: time.Now(), long: e.Type == "EventSource"}
				}
				p.mu.Unlock()
			case "Network.loadingFinished", "Network.loadingFailed":
				var e struct {
					RequestID string `json:"requestId"`
				}
				if json.Unmarshal(ev.Params, &e) != nil {
					continue
				}
				p.mu.Lock()
				delete(p.inflight, e.RequestID)
				p.mu.Unlock()
			case "Page.lifecycleEvent":
				var e struct {
					LoaderID string `json:"loaderId"`
					Name     string `json:"name"`
				}
				if json.Unmarshal(ev.Params, &e) != nil || e.Name != "load" {
					continue
				}
				p.mu.Lock()
				p.loadedBy[e.LoaderID] = true
				p.mu.Unlock()
			}
		}
	}
}

func (p *chromePage) Probe(ctx context.Context, pixels bool) (pageProbe, error) {
	p.mu.Lock()
	out := pageProbe{Loaded: p.loadedBy[p.loaderID], InFlight: len(p.inflight), Requests: p.requests, Mutations: -1}
	now := time.Now()
	for _, r := range p.inflight {
		if r.long || now.Sub(r.start) > pageSnapshotLongRequest {
			out.LongLived++
		}
	}
	left, final := p.left, p.final
	p.mu.Unlock()
	if left {
		return out, errPageLeftOrigin
	}
	if final {
		return out, errors.New("the page was captured: it is not probed after")
	}
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := p.conn.call(ctx, p.s, "Runtime.evaluate", map[string]any{"expression": pageProbeJS, "returnByValue": true}, &res); err != nil {
		if hard := p.hard(ctx); hard != nil {
			return out, p.why(ctx, err)
		}
		// Between two documents there is no context to evaluate in.
		out.Note = "between documents"
		return out, nil
	}
	var v struct {
		M  int    `json:"m"`
		RS string `json:"rs"`
		O  string `json:"o"`
		P  string `json:"p"`
		T  string `json:"t"`
		TH string `json:"th"`
		TL int    `json:"tl"`
		LH string `json:"lh"`
		SX int    `json:"sx"`
		SY int    `json:"sy"`
		VW int    `json:"vw"`
		VH int    `json:"vh"`
	}
	if res.ExceptionDetails != nil || json.Unmarshal([]byte(res.Result.Value), &v) != nil {
		out.Note = "the page did not answer the probe"
		return out, nil
	}
	if v.O != p.job.Origin {
		if out.Loaded {
			return out, errPageLeftOrigin
		}
		out.Note = "not yet on the page" // still about:blank
		return out, nil
	}
	out.Mutations, out.ReadyState, out.Path, out.Title = v.M, v.RS, v.P, v.T
	out.TextHash, out.TextLen, out.Layout = v.TH, v.TL, v.LH
	if pixels {
		png, err := p.shot(ctx, 0.25, v.SX, v.SY, v.VW, v.VH)
		if err != nil {
			return out, p.why(ctx, err)
		}
		sum := sha256.Sum256(png)
		out.Pixels = hex.EncodeToString(sum[:6])
	}
	return out, nil
}

// hard is the error that ends the page: Chrome gone or the caller done.
func (p *chromePage) hard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-p.conn.done:
		return errors.New("the connection to Chrome closed")
	case <-p.exited:
		return errors.New("chrome exited")
	default:
		return nil
	}
}

// shot captures the viewport at scale. A clip is in document coordinates,
// so it starts at the scroll.
func (p *chromePage) shot(ctx context.Context, scale float64, sx, sy, vw, vh int) ([]byte, error) {
	params := map[string]any{"format": "png"}
	if scale > 0 && scale < 1 && vw > 0 && vh > 0 {
		params["clip"] = map[string]any{"x": sx, "y": sy, "width": vw, "height": vh, "scale": scale}
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := p.conn.call(ctx, p.s, "Page.captureScreenshot", params, &shot); err != nil {
		return nil, err
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return nil, fmt.Errorf("chrome sent an unreadable screenshot: %w", err)
	}
	return png, nil
}

// onOrigin checks the page is still the page: its own origin, no document
// from elsewhere blocked.
func (p *chromePage) onOrigin(ctx context.Context) (sx, sy, vw, vh int, err error) {
	var where struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := p.conn.call(ctx, p.s, "Runtime.evaluate", map[string]any{
		"expression": "JSON.stringify([location.origin, Math.round(scrollX), Math.round(scrollY), innerWidth, innerHeight])", "returnByValue": true,
	}, &where); err != nil {
		return 0, 0, 0, 0, p.why(ctx, err)
	}
	var v []any
	if json.Unmarshal([]byte(where.Result.Value), &v) != nil || len(v) != 5 {
		return 0, 0, 0, 0, errors.New("could not read where the page is")
	}
	p.mu.Lock()
	left := p.left
	p.mu.Unlock()
	if o, _ := v[0].(string); o != p.job.Origin || left {
		return 0, 0, 0, 0, errPageLeftOrigin
	}
	num := func(x any) int { f, _ := x.(float64); return int(f) }
	return num(v[1]), num(v[2]), num(v[3]), num(v[4]), nil
}

func (p *chromePage) Frame(ctx context.Context, scale float64) ([]byte, error) {
	sx, sy, vw, vh, err := p.onOrigin(ctx)
	if err != nil {
		return nil, err
	}
	png, err := p.shot(ctx, scale, sx, sy, vw, vh)
	return png, p.why(ctx, err)
}

func (p *chromePage) Final(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	p.final = true
	p.mu.Unlock()
	// The page may have navigated itself; only its own origin is captured.
	if _, _, _, _, err := p.onOrigin(ctx); err != nil {
		return nil, err
	}
	job := p.job
	overlay, _ := json.Marshal(job.OverlaySV)
	patches := job.Patches
	if patches == "" {
		patches = "[]"
	}
	expr := fmt.Sprintf(`(function(){
  %s
  var patches = %s;
  for (var i = 0; i < patches.length; i++) {
    try { tclaudeApplyPatch(document, patches[i]); } catch (e) { /* as in the operator's view: a patch that fails is skipped */ }
  }
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
})()`, stagePatchJS, patches, job.ScrollX, job.ScrollY, overlay)
	var evaluated struct {
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := p.conn.call(ctx, p.s, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true}, &evaluated); err != nil {
		return nil, p.why(ctx, err)
	}
	if d := evaluated.ExceptionDetails; d != nil {
		why := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			why = d.Exception.Description
		}
		if len(why) > 200 {
			why = why[:200]
		}
		return nil, fmt.Errorf("could not draw the marks on the page (its CSP may forbid it): %s", why)
	}
	png, err := p.shot(ctx, 1, 0, 0, 0, 0)
	return png, p.why(ctx, err)
}

// ---- deciding the page is still ----

// The default snapshot waits for the page to go still: loaded, then no
// loading request in flight, no DOM mutation and the same text and layout for
// pageSnapshotQuiet, at least pageSnapshotSettle after the load, and at most
// pageSnapshotSettleMax. A page that never goes still (an animation driven by
// script, a ticking clock) is captured at the cap, and the answer says why.
const (
	pageSnapshotSettle    = 300 * time.Millisecond
	pageSnapshotQuiet     = 500 * time.Millisecond
	pageSnapshotSettleMax = 10 * time.Second
	pageSnapshotProbeTick = 100 * time.Millisecond
)

// pageChangeTracker follows a page from probe to probe: what changed since
// the last one, and since when nothing has.
type pageChangeTracker struct {
	prev       *pageProbe
	lastChange time.Duration
}

// observe takes a probe made at `at` (since the page opened) and says what
// changed since the previous one. Requests still loading count as a change:
// a page waiting on its data is not still.
func (t *pageChangeTracker) observe(p pageProbe, at time.Duration) []string {
	var changed []string
	add := func(what string, yes bool) {
		if yes {
			changed = append(changed, what)
		}
	}
	prev := t.prev
	if prev == nil {
		prev = &pageProbe{}
	}
	add("load", p.Loaded != prev.Loaded)
	add("net", p.InFlight-p.LongLived > 0 || p.Requests != prev.Requests)
	add("dom", p.Mutations != 0)
	add("text", p.TextHash != prev.TextHash)
	add("layout", p.Layout != prev.Layout)
	add("pixels", p.Pixels != "" && prev.Pixels != "" && p.Pixels != prev.Pixels)
	add("title", p.Title != prev.Title)
	add("path", p.Path != prev.Path)
	if p.Note != "" {
		changed = append(changed, "busy")
	}
	if len(changed) > 0 || t.prev == nil {
		t.lastChange = at
	}
	t.prev = &p
	return changed
}

// still is how long nothing has changed, as of at.
func (t *pageChangeTracker) still(at time.Duration) time.Duration {
	if t.prev == nil {
		return 0
	}
	return at - t.lastChange
}

// pageSettle is how the default wait went.
type pageSettle struct {
	WaitedMS int64 `json:"waited_ms"`
	Settled  bool  `json:"settled"`
	// Why says what kept the page from going still, when it did not.
	Why string `json:"why,omitempty"`
}

// pageSettleWait decides, probe by probe, when the default snapshot captures.
type pageSettleWait struct {
	track  pageChangeTracker
	loadAt time.Duration
	loaded bool
	last   []string
	probe  pageProbe
}

// observe takes a probe made at `at` and says whether to capture now.
func (w *pageSettleWait) observe(p pageProbe, at time.Duration) (capture bool, out pageSettle) {
	w.last = w.track.observe(p, at)
	w.probe = p
	if p.Loaded && !w.loaded {
		w.loaded, w.loadAt = true, at
	}
	out.WaitedMS = at.Milliseconds()
	if !w.loaded {
		return false, out
	}
	since := at - w.loadAt
	if since >= pageSnapshotSettle && w.track.still(at) >= pageSnapshotQuiet {
		out.Settled = true
		return true, out
	}
	if since >= pageSnapshotSettleMax {
		out.Why = w.why()
		return true, out
	}
	return false, out
}

func (w *pageSettleWait) why() string {
	var parts []string
	for _, c := range w.last {
		switch c {
		case "net":
			parts = append(parts, fmt.Sprintf("%d requests still loading", w.probe.InFlight-w.probe.LongLived))
		case "dom":
			parts = append(parts, "the DOM kept changing")
		case "text", "layout":
			parts = append(parts, "its "+c+" kept changing")
		default:
			parts = append(parts, c+" changed")
		}
	}
	if len(parts) == 0 {
		return "it was still for less than " + pageSnapshotQuiet.String()
	}
	return strings.Join(parts, ", ")
}

// renderPageSnapshot is the default snapshot: open the page, wait for it to
// go still (capped), capture.
func renderPageSnapshot(ctx context.Context, job pageSnapshotJob) ([]byte, pageSettle, error) {
	b, err := pageSnapshotOpen(ctx, job)
	if err != nil {
		return nil, pageSettle{}, err
	}
	defer b.Close()
	start := time.Now()
	var wait pageSettleWait
	var settle pageSettle
	for {
		p, err := b.Probe(ctx, false)
		if err != nil {
			return nil, settle, err
		}
		capture, s := wait.observe(p, time.Since(start))
		settle = s
		if capture {
			break
		}
		select {
		case <-time.After(pageSnapshotProbeTick):
		case <-ctx.Done():
			if !wait.loaded {
				return nil, settle, fmt.Errorf("the page did not finish loading: %w", ctx.Err())
			}
			return nil, settle, ctx.Err()
		}
	}
	png, err := b.Final(ctx)
	return png, settle, err
}
