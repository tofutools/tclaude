package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Marks on live pages (tcl-2dwbad). The operator draws on top of a page in
// the dashboard and sends the marks, with an optional note, to the agent that
// showed it: POST /api/pages/{id}/marks stores the batch (in memory, as the
// page itself is) and queues an inbox message. The agent reads the batches
// with `tclaude agent page marks`, renders the page with them with `page
// snapshot`, and reads the DOM under them with `page dom`.
//
// Everything in a batch but the note comes from the dashboard's probe
// channel, which carries page content (element paths, text snippets): it is
// size- and schema-checked here as in the dashboard, kept out of the
// operator-authored inbox message, and handed to the agent as JSON data.
//
// DOM reads go through a dashboard view of the page: the daemon asks one of
// the page's open live sockets (dashboard_page_live.go), the view asks the
// probe in its frame, and the answer comes back on the same socket. With no
// view open there is nothing to read from, and the agent is told so.

const (
	pageMarksMaxBody     = 1 << 20
	pageMarksMaxBatches  = 50
	pageMarksMax         = 100
	pageMarkMaxPoints    = 2000
	pageMarkMaxText      = 500
	pageMarkMaxAnchors   = 50
	pageAnchorMaxPath    = 300
	pageAnchorMaxText    = 200
	pageNoteMax          = 4000
	pagePathMax          = 512
	pageCoordMax         = 10_000_000
	pageViewportMax      = 20_000
	pageDOMSelectorMax   = 500
	pageDOMMaxItems      = 10
	pageDOMMaxValue      = 16 * 1024
	pageDOMErrorMax      = 200
	pageDOMTimeout       = 10 * time.Second
	pageLiveReadLimit    = 512 << 10
	pageViewQueueSize    = 4
	pageDOMMaxConcurrent = 8
)

var pageMarkColor = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

var pageMarkKinds = map[string]bool{"pen": true, "arrow": true, "box": true, "text": true}

type pageXY struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type pageWH struct {
	W int `json:"w"`
	H int `json:"h"`
}

// pageAnchor is an element the probe found under a mark.
type pageAnchor struct {
	Path string `json:"path"`
	Text string `json:"text"`
	Hits int    `json:"hits"`
}

type pageMark struct {
	Kind    string       `json:"kind"`
	Color   string       `json:"color,omitempty"`
	Points  [][2]int     `json:"points"`
	Text    string       `json:"text,omitempty"`
	Anchors []pageAnchor `json:"anchors"`
}

// pageMarkBatch is one Send: the marks in page pixels, the viewport and
// scroll they were drawn at, and the frame's path then.
type pageMarkBatch struct {
	Seq      int64      `json:"seq"`
	At       time.Time  `json:"at"`
	Note     string     `json:"note,omitempty"`
	Path     string     `json:"path,omitempty"`
	Viewport pageWH     `json:"viewport"`
	Scroll   pageXY     `json:"scroll"`
	Marks    []pageMark `json:"marks"`
}

// pageViewReport is a dashboard view's last viewport and scroll.
type pageViewReport struct {
	VW   int    `json:"vw"`
	VH   int    `json:"vh"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Path string `json:"path"`
}

type pageDOMRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type pageDOMItem struct {
	Path  string      `json:"path"`
	Rect  pageDOMRect `json:"rect"`
	Value string      `json:"value"`
}

type pageDOMResult struct {
	Count int           `json:"count"`
	Items []pageDOMItem `json:"items"`
	Error string        `json:"error,omitempty"`
}

// pageViewConn is one open live socket of a page: out queues messages to
// it, pending holds its unanswered DOM reads.
type pageViewConn struct {
	out     chan []byte
	pending map[string]chan pageDOMResult
	viewAt  time.Time // when it last reported a viewport
}

// pageCollabState is a page's marks and open views, by page id. It outlives
// a re-show of the page (same id) and goes when the page closes.
type pageCollabState struct {
	seq     int64
	batches []pageMarkBatch
	view    *pageViewReport
	views   map[*pageViewConn]struct{}
	domReqs int
}

var pageCollab = struct {
	mu   sync.Mutex
	byID map[string]*pageCollabState
	req  int64
}{byID: map[string]*pageCollabState{}}

// pageCollabLocked returns the page's state, making it. Caller holds pageCollab.mu.
func pageCollabLocked(id string) *pageCollabState {
	s := pageCollab.byID[id]
	if s == nil {
		s = &pageCollabState{views: map[*pageViewConn]struct{}{}}
		pageCollab.byID[id] = s
	}
	return s
}

// forgetPageCollab drops a closed page's marks; its views end with its origin.
func forgetPageCollab(id string) {
	pageCollab.mu.Lock()
	delete(pageCollab.byID, id)
	pageCollab.mu.Unlock()
}

// ---- checks ----

func pageTextOK(s string, max int, multiline bool) bool {
	if len(s) > max*4 || utf8.RuneCountInString(s) > max || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && (!multiline || (r != '\n' && r != '\t'))
	})
}

func pageCoordOK(v int) bool { return v >= -pageCoordMax && v <= pageCoordMax }

func pageViewportOK(w, h int) bool {
	return w >= 0 && h >= 0 && w <= pageViewportMax && h <= pageViewportMax
}

func checkPageMark(m *pageMark) error {
	if !pageMarkKinds[m.Kind] {
		return errors.New("a mark is a pen, arrow, box or text")
	}
	if m.Color != "" && !pageMarkColor.MatchString(m.Color) {
		return errors.New("a mark's colour is #rgb..#rrggbbaa")
	}
	if len(m.Points) == 0 || len(m.Points) > pageMarkMaxPoints {
		return fmt.Errorf("a mark has 1 to %d points", pageMarkMaxPoints)
	}
	if m.Kind == "box" && len(m.Points) != 2 {
		return errors.New("a box mark has two corners")
	}
	for _, p := range m.Points {
		if !pageCoordOK(p[0]) || !pageCoordOK(p[1]) {
			return errors.New("a mark's point is out of range")
		}
	}
	if !pageTextOK(m.Text, pageMarkMaxText, false) || (m.Kind == "text") != (m.Text != "") {
		return fmt.Errorf("a text mark has printable text of at most %d characters; other marks have none", pageMarkMaxText)
	}
	if len(m.Anchors) > pageMarkMaxAnchors {
		return fmt.Errorf("a mark has at most %d anchors", pageMarkMaxAnchors)
	}
	for _, a := range m.Anchors {
		if a.Path == "" || !pageTextOK(a.Path, pageAnchorMaxPath, false) || !pageTextOK(a.Text, pageAnchorMaxText, false) ||
			a.Hits < 0 || a.Hits > pageMarkMaxPoints {
			return errors.New("a mark's anchor is malformed")
		}
	}
	if m.Anchors == nil {
		m.Anchors = []pageAnchor{}
	}
	return nil
}

func checkPageViewReport(v *pageViewReport) bool {
	return pageViewportOK(v.VW, v.VH) && pageCoordOK(v.X) && pageCoordOK(v.Y) && pageTextOK(v.Path, pagePathMax, false)
}

func checkPageDOMResult(r *pageDOMResult) bool {
	if r.Count < 0 || r.Count > pageCoordMax || len(r.Items) > pageDOMMaxItems || !pageTextOK(r.Error, pageDOMErrorMax, false) {
		return false
	}
	for _, it := range r.Items {
		if !pageTextOK(it.Path, pageAnchorMaxPath, false) || len(it.Value) > pageDOMMaxValue*4 ||
			utf8.RuneCountInString(it.Value) > pageDOMMaxValue || !utf8.ValidString(it.Value) {
			return false
		}
		if !pageCoordOK(it.Rect.X) || !pageCoordOK(it.Rect.Y) || !pageCoordOK(it.Rect.W) || !pageCoordOK(it.Rect.H) {
			return false
		}
	}
	return true
}

// ---- the dashboard's Send ----

type pageMarksRequest struct {
	Note     string     `json:"note"`
	Path     string     `json:"path"`
	Viewport pageWH     `json:"viewport"`
	Scroll   pageXY     `json:"scroll"`
	Marks    []pageMark `json:"marks"`
}

// POST /api/pages/{id}/marks — the operator sent marks (and/or a note). The
// batch is kept for `page marks`, and the agent is told through its inbox.
func handleDashboardPageMarks(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pageMarksMaxBody)
	var req pageMarksRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	switch {
	case !pageTextOK(req.Note, pageNoteMax, true):
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("the note must be printable and at most %d characters", pageNoteMax))
		return
	case len(req.Marks) > pageMarksMax:
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("at most %d marks at a time", pageMarksMax))
		return
	case len(req.Marks) == 0 && req.Note == "":
		writeError(w, http.StatusBadRequest, "invalid_arg", "nothing to send: no marks and no note")
		return
	case !pageTextOK(req.Path, pagePathMax, false) || !pageViewportOK(req.Viewport.W, req.Viewport.H) ||
		!pageCoordOK(req.Scroll.X) || !pageCoordOK(req.Scroll.Y):
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid path, viewport or scroll")
		return
	}
	for i := range req.Marks {
		if err := checkPageMark(&req.Marks[i]); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("mark %d: %v", i+1, err))
			return
		}
	}
	if req.Marks == nil {
		req.Marks = []pageMark{}
	}

	id := r.PathValue("id")
	livePages.mu.Lock()
	p := livePages.byID[id]
	var owner, name string
	if p != nil {
		owner, name = p.owner, p.name
	}
	livePages.mu.Unlock()
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	pageCollab.mu.Lock()
	s := pageCollabLocked(id)
	s.seq++
	batch := pageMarkBatch{
		Seq: s.seq, At: time.Now().UTC(), Note: req.Note, Path: req.Path,
		Viewport: req.Viewport, Scroll: req.Scroll, Marks: req.Marks,
	}
	s.batches = append(s.batches, batch)
	if len(s.batches) > pageMarksMaxBatches {
		s.batches = append([]pageMarkBatch(nil), s.batches[len(s.batches)-pageMarksMaxBatches:]...)
	}
	pageCollab.mu.Unlock()
	setAuditTargetLabel(r, "page "+name)

	resp := map[string]any{"seq": batch.Seq, "told_agent": false}
	if convID := humanActionLiveConv(owner); convID != "" {
		subject, body := pageMarksMessage(name, batch)
		if _, _, err := queueRegularAgentMessage(&db.AgentMessage{
			ToConv: convID, Subject: subject, Body: body,
			ToRecipients: []string{convID}, OperatorAuthored: true,
		}); err != nil {
			resp["error"] = "failed to queue message: " + err.Error()
		} else {
			resp["told_agent"] = true
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// pageMarksMessage is the inbox message for a batch. It is operator-authored,
// so it quotes only what the operator wrote (the note) and the agent's own
// page name; the element paths and snippets, which the page supplied, stay in
// `page marks`.
func pageMarksMessage(name string, b pageMarkBatch) (subject, body string) {
	var sb strings.Builder
	if len(b.Marks) == 0 {
		fmt.Fprintf(&sb, "The operator sent a note about your live page %q:\n\n%s\n", name, b.Note)
		return "[page " + name + "] note", sb.String()
	}
	anchors := 0
	for _, m := range b.Marks {
		anchors += len(m.Anchors)
	}
	noun := "marks"
	if len(b.Marks) == 1 {
		noun = "mark"
	}
	fmt.Fprintf(&sb, "The operator drew %d %s on your live page %q (batch %d, viewport %dx%d, scrolled to %d,%d; %d anchored elements).\n",
		len(b.Marks), noun, name, b.Seq, b.Viewport.W, b.Viewport.H, b.Scroll.X, b.Scroll.Y, anchors)
	if b.Note != "" {
		fmt.Fprintf(&sb, "\nNote: %s\n", b.Note)
	}
	fmt.Fprintf(&sb, "\nRead the marks and the elements under them with `tclaude agent page marks %s --since %d`; "+
		"`tclaude agent page snapshot %s -o page.png` renders the page with them, and `tclaude agent page dom %s <selector>` reads the DOM.\n",
		name, b.Seq-1, name, name)
	return "[page " + name + "] marks", sb.String()
}

// ---- views, over the live socket ----

// registerPageView adds an open live socket to its page's views.
func registerPageView(id string) (*pageViewConn, func()) {
	c := &pageViewConn{out: make(chan []byte, pageViewQueueSize), pending: map[string]chan pageDOMResult{}}
	pageCollab.mu.Lock()
	pageCollabLocked(id).views[c] = struct{}{}
	pageCollab.mu.Unlock()
	return c, func() {
		pageCollab.mu.Lock()
		if s := pageCollab.byID[id]; s != nil {
			delete(s.views, c)
		}
		for _, ch := range c.pending {
			close(ch)
		}
		c.pending = map[string]chan pageDOMResult{}
		pageCollab.mu.Unlock()
	}
}

// pageViewMessage takes one message a view sent on its live socket: a
// viewport report or a DOM read's answer. Anything else, or anything
// malformed, is dropped.
func pageViewMessage(id string, c *pageViewConn, data []byte) {
	var head struct {
		Type string `json:"type"`
		Req  string `json:"req"`
	}
	if json.Unmarshal(data, &head) != nil {
		return
	}
	switch head.Type {
	case "view":
		var v pageViewReport
		if json.Unmarshal(data, &v) != nil || !checkPageViewReport(&v) || v.VW == 0 || v.VH == 0 {
			return
		}
		pageCollab.mu.Lock()
		if s := pageCollab.byID[id]; s != nil {
			s.view = &v
			c.viewAt = time.Now()
		}
		pageCollab.mu.Unlock()
	case "dom-result":
		var res pageDOMResult
		if json.Unmarshal(data, &res) != nil || !checkPageDOMResult(&res) {
			res = pageDOMResult{Error: "the page's answer was malformed"}
		}
		pageCollab.mu.Lock()
		ch := c.pending[head.Req]
		delete(c.pending, head.Req)
		pageCollab.mu.Unlock()
		if ch != nil {
			ch <- res
			close(ch)
		}
	}
}

var errPageNoView = errors.New("no dashboard view of this page is open, so there is no DOM to read; ask the operator to open the page")

// pageDOMRead asks one open view of the page (the one that reported its
// viewport last) to read the DOM.
func pageDOMRead(ctx context.Context, id, selector, mode string) (pageDOMResult, error) {
	pageCollab.mu.Lock()
	s := pageCollab.byID[id]
	var best *pageViewConn
	if s != nil {
		for c := range s.views {
			if best == nil || c.viewAt.After(best.viewAt) {
				best = c
			}
		}
	}
	if best == nil {
		pageCollab.mu.Unlock()
		return pageDOMResult{}, errPageNoView
	}
	if s.domReqs >= pageDOMMaxConcurrent {
		pageCollab.mu.Unlock()
		return pageDOMResult{}, errors.New("too many DOM reads of this page at once")
	}
	s.domReqs++
	pageCollab.req++
	req := "d" + strconv.FormatInt(pageCollab.req, 36)
	ch := make(chan pageDOMResult, 1)
	best.pending[req] = ch
	pageCollab.mu.Unlock()
	defer func() {
		pageCollab.mu.Lock()
		s.domReqs--
		delete(best.pending, req)
		pageCollab.mu.Unlock()
	}()

	msg, _ := json.Marshal(map[string]string{"type": "dom", "req": req, "selector": selector, "mode": mode})
	select {
	case best.out <- msg:
	default:
		return pageDOMResult{}, errors.New("the dashboard view is busy; try again")
	}
	timer := time.NewTimer(pageDOMTimeout)
	defer timer.Stop()
	select {
	case res, ok := <-ch:
		if !ok {
			return pageDOMResult{}, errPageNoView
		}
		if res.Items == nil {
			res.Items = []pageDOMItem{}
		}
		return res, nil
	case <-timer.C:
		return pageDOMResult{}, errors.New("the dashboard view did not answer in time")
	case <-ctx.Done():
		return pageDOMResult{}, ctx.Err()
	}
}

// ---- agent routes ----

// ownPage finds one of the caller's pages by name.
func ownPage(w http.ResponseWriter, r *http.Request) (*livePage, bool) {
	_, agentID, ok := pageCaller(w, r)
	if !ok {
		return nil, false
	}
	name := r.PathValue("name")
	if !pageNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid page name")
		return nil, false
	}
	livePages.mu.Lock()
	p := livePages.byOwner[agentID][name]
	livePages.mu.Unlock()
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "you have no page named "+name)
		return nil, false
	}
	return p, true
}

// GET /v1/pages/{name}/marks?since=<seq> — the batches the operator sent
// after seq (all that are kept, without since).
func handlePageMarks(w http.ResponseWriter, r *http.Request) {
	p, ok := ownPage(w, r)
	if !ok {
		return
	}
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "since is a batch number")
			return
		}
		since = n
	}
	pageCollab.mu.Lock()
	out := []pageMarkBatch{}
	var last int64
	if s := pageCollab.byID[p.id]; s != nil {
		last = s.seq
		for _, b := range s.batches {
			if b.Seq > since {
				out = append(out, b)
			}
		}
	}
	pageCollab.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"page": p.name, "last_seq": last, "batches": out})
}

type pageDOMRequest struct {
	Selector string `json:"selector"`
	Mode     string `json:"mode"`
}

// POST /v1/pages/{name}/dom — a read-only DOM read through an open view.
func handlePageDOM(w http.ResponseWriter, r *http.Request) {
	p, ok := ownPage(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req pageDOMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	req.Selector = strings.TrimSpace(req.Selector)
	if req.Selector == "" || !pageTextOK(req.Selector, pageDOMSelectorMax, false) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("a selector is 1 to %d printable characters", pageDOMSelectorMax))
		return
	}
	if req.Mode == "" {
		req.Mode = "text"
	}
	if req.Mode != "text" && req.Mode != "html" && req.Mode != "styles" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "mode is text, html or styles")
		return
	}
	res, err := pageDOMRead(r.Context(), p.id, req.Selector, req.Mode)
	if err != nil {
		status := http.StatusGatewayTimeout
		if errors.Is(err, errPageNoView) {
			status = http.StatusConflict
		}
		writeError(w, status, "no_view", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// latestPageView is the viewport and scroll a snapshot renders at: the last
// view report, else the last batch's, else none.
func latestPageView(id string) (view pageViewReport, batch *pageMarkBatch) {
	pageCollab.mu.Lock()
	defer pageCollab.mu.Unlock()
	s := pageCollab.byID[id]
	if s == nil {
		return pageViewReport{}, nil
	}
	if n := len(s.batches); n > 0 {
		b := s.batches[n-1]
		batch = &b
	}
	if s.view != nil {
		return *s.view, batch
	}
	if batch != nil {
		return pageViewReport{VW: batch.Viewport.W, VH: batch.Viewport.H, X: batch.Scroll.X, Y: batch.Scroll.Y, Path: batch.Path}, batch
	}
	return pageViewReport{}, nil
}
