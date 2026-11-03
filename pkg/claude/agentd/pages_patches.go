package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Live patches (tcl-14z3fq). The agent changes its open page without a
// source change, for a quick "like this?": `page css` adds style rules, `page
// dom --set-text|--set-attr|--remove|--insert-html` changes elements, and
// `page eval` runs a script once and returns its result as JSON.
//
// Each css or dom change is a patch in the page's journal, kept here (in
// memory, as the page is) until the agent resets it or the page closes. Every
// open view gets the journal on its live socket (dashboard_page_live.go), the
// parent hands each patch to the probe in the frame (stage_probe.js), and the
// probe applies it with stage_patch.js. A view replays the journal after
// every load, so a patch survives a reload or a full HMR reload; a `page
// snapshot` renders with it too. Patches never write files. An eval is listed
// in the journal but runs once, in one view, and is not replayed.
//
// The channel: the daemon takes patches only from the page's owning agent
// (ownPage), the parent relays only what its socket delivered, and the probe
// takes commands only from its parent with the nonce of its current load.
//
// What a view answers comes from the page (its scripts can forge the
// probe's messages), so it is checked here, and for a static page that is not
// read back (pageReadBackAllowed) none of it reaches the agent: it learns
// only how many views the patch went to, and an eval is refused.

const (
	pagePatchesMax       = 100
	pagePatchesMaxBytes  = 256 << 10
	pagePatchCSSMax      = 32 << 10
	pagePatchHTMLMax     = 32 << 10
	pagePatchTextMax     = 16 << 10
	pagePatchAttrNameMax = 100
	pageEvalJSMax        = 32 << 10
	pageEvalResultMax    = 16 << 10
	// The journal lists the last evals, their scripts clipped; they never
	// count toward the patch caps.
	pageEvalsKept    = 20
	pageEvalJSKept   = 2000
	pagePatchMaxBody = 128 << 10
	pagePatchTimeout = 10 * time.Second
)

var pagePatchAttrName = regexp.MustCompile(`^[A-Za-z_:][-A-Za-z0-9_:.]*$`)

// pagePatch is one entry of a page's journal.
type pagePatch struct {
	N    int64     `json:"n"`
	Kind string    `json:"kind"` // css, dom or eval
	At   time.Time `json:"at"`
	// Replay is false for an eval: it ran once.
	Replay   bool   `json:"replay"`
	CSS      string `json:"css,omitempty"`
	Selector string `json:"selector,omitempty"`
	Op       string `json:"op,omitempty"`
	Name     string `json:"name,omitempty"`
	Value    string `json:"value,omitempty"`
	JS       string `json:"js,omitempty"`
}

// size is what a patch counts toward the journal's byte cap: an eval, kept
// only for the listing, counts nothing.
func (p pagePatch) size() int {
	if !p.Replay {
		return 0
	}
	return len(p.CSS) + len(p.Selector) + len(p.Name) + len(p.Value)
}

// pagePatchRequest is the body of POST /v1/pages/{name}/patches.
type pagePatchRequest struct {
	Kind     string `json:"kind"`
	CSS      string `json:"css"`
	Selector string `json:"selector"`
	Op       string `json:"op"`
	Name     string `json:"name"`
	Value    string `json:"value"`
}

func checkPagePatch(req pagePatchRequest) (pagePatch, error) {
	switch req.Kind {
	case "css":
		if strings.TrimSpace(req.CSS) == "" || !pageTextOK(req.CSS, pagePatchCSSMax, true) {
			return pagePatch{}, fmt.Errorf("css is 1 to %d printable characters", pagePatchCSSMax)
		}
		return pagePatch{Kind: "css", CSS: req.CSS, Replay: true}, nil
	case "dom":
		sel := strings.TrimSpace(req.Selector)
		if sel == "" || !pageTextOK(sel, pageDOMSelectorMax, false) {
			return pagePatch{}, fmt.Errorf("a selector is 1 to %d printable characters", pageDOMSelectorMax)
		}
		p := pagePatch{Kind: "dom", Selector: sel, Op: req.Op, Replay: true}
		switch req.Op {
		case "set-text":
			if !pageTextOK(req.Value, pagePatchTextMax, true) {
				return pagePatch{}, fmt.Errorf("the text is at most %d printable characters", pagePatchTextMax)
			}
			p.Value = req.Value
		case "set-attr":
			if len(req.Name) > pagePatchAttrNameMax || !pagePatchAttrName.MatchString(req.Name) {
				return pagePatch{}, errors.New("an attribute name is letters, digits and -_:. (NAME=VALUE)")
			}
			if !pageTextOK(req.Value, pagePatchTextMax, false) {
				return pagePatch{}, fmt.Errorf("the attribute value is at most %d printable characters", pagePatchTextMax)
			}
			p.Name, p.Value = req.Name, req.Value
		case "insert-html":
			if req.Value == "" || !pageTextOK(req.Value, pagePatchHTMLMax, true) {
				return pagePatch{}, fmt.Errorf("the HTML is 1 to %d printable characters", pagePatchHTMLMax)
			}
			p.Value = req.Value
		case "remove":
		default:
			return pagePatch{}, errors.New("a DOM patch is set-text, set-attr, remove or insert-html")
		}
		return p, nil
	default:
		return pagePatch{}, errors.New("a patch is css or dom")
	}
}

// pagePatchStateLocked is the journal as a view gets it: the replayed
// patches and the last patch number (which says a reset left none). req,
// when set, asks the view to answer for the newest patch. Caller holds
// pageCollab.mu.
func pagePatchStateLocked(s *pageCollabState, req string) []byte {
	return pagePatchStateMessage(pagePatchReplayJSONLocked(s), s.patchSeq, req)
}

// pagePatchReplayJSONLocked encodes the replayed patches once, for every view
// a change goes to. It goes over a websocket, not into HTML, so <, > and &
// stay as they are (escaped, an HTML-heavy journal would grow sixfold).
func pagePatchReplayJSONLocked(s *pageCollabState) []byte {
	replay := []pagePatch{}
	for _, p := range s.patches {
		if p.Replay {
			replay = append(replay, p)
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(replay)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func pagePatchStateMessage(replay []byte, seq int64, req string) []byte {
	var b bytes.Buffer
	b.Grow(len(replay) + 64)
	b.WriteString(`{"type":"patches","seq":`)
	b.WriteString(strconv.FormatInt(seq, 10))
	if req != "" {
		// req is the daemon's own id: "q" and base-36 digits.
		b.WriteString(`,"req":"` + req + `"`)
	}
	b.WriteString(`,"patches":`)
	b.Write(replay)
	b.WriteString(`}`)
	return b.Bytes()
}

// pagePatchResult is a view's answer for a patch. State is "" when the probe
// ran it, "loading" when the frame has not loaded yet (it applies when it
// does), and "without" when the operator is viewing the page without patches.
type pagePatchResult struct {
	Applied int    `json:"applied"`
	State   string `json:"state,omitempty"`
	Error   string `json:"error,omitempty"`
}

func checkPagePatchResult(r *pagePatchResult) bool {
	return r.Applied >= 0 && r.Applied <= pageCoordMax && (r.State == "" || r.State == "loading" || r.State == "without") &&
		pageTextOK(r.Error, pageDOMErrorMax, false)
}

// pagePatchBroadcast sends the journal, with the newest patch to answer for,
// to every open view, and collects the answers. A view whose queue is full or
// that does not answer in time is counted with an error.
func pagePatchBroadcast(ctx context.Context, id string) []pagePatchResult {
	type ask struct {
		c   *pageViewConn
		req string
		ch  chan []byte
	}
	pageCollab.mu.Lock()
	s := pageCollab.byID[id]
	var asks []ask
	if s != nil {
		replay := pagePatchReplayJSONLocked(s)
		for c := range s.views {
			pageCollab.req++
			req := "q" + strconv.FormatInt(pageCollab.req, 36)
			a := ask{c: c, req: req}
			select {
			case c.out <- pagePatchStateMessage(replay, s.patchSeq, req):
				a.ch = make(chan []byte, 1)
				c.pending["patch-result:"+req] = a.ch
			default:
			}
			asks = append(asks, a)
		}
	}
	pageCollab.mu.Unlock()

	out := make([]pagePatchResult, len(asks))
	deadline := time.Now().Add(pagePatchTimeout)
	for i, a := range asks {
		if a.ch == nil {
			out[i] = pagePatchResult{Error: "the dashboard view is busy"}
			continue
		}
		data, ok, err := pageViewWait(ctx, a.ch, time.Until(deadline))
		pageViewForget(a.c, "patch-result", a.req)
		var res pagePatchResult
		switch {
		case err != nil:
			res = pagePatchResult{Error: err.Error()}
		case !ok:
			res = pagePatchResult{Error: "the dashboard view closed"}
		case json.Unmarshal(data, &res) != nil || !checkPagePatchResult(&res):
			res = pagePatchResult{Error: "the page's answer was malformed"}
		}
		out[i] = res
	}
	return out
}

// pagePatchNotify sends the journal to every open view, answering nothing:
// after a reset, a view whose load has a dropped patch reloads and replays
// the rest; for a page that is not read back, a new patch goes this way too.
// A view with a full queue picks it up with the next change. It returns how
// many views it was sent to.
func pagePatchNotify(id string) int {
	pageCollab.mu.Lock()
	defer pageCollab.mu.Unlock()
	s := pageCollab.byID[id]
	if s == nil {
		return 0
	}
	msg := pagePatchStateLocked(s, "")
	sent := 0
	for c := range s.views {
		select {
		case c.out <- msg:
			sent++
		default:
		}
	}
	return sent
}

var errPagePatchClosed = errors.New("the page closed")

// pagePatchAppend adds a patch to an open page's journal and returns it
// numbered. A page that closed meanwhile gets no journal.
func pagePatchAppend(id string, p pagePatch) (pagePatch, error) {
	livePages.mu.Lock()
	open := livePages.byID[id] != nil
	pageCollab.mu.Lock()
	livePages.mu.Unlock()
	defer pageCollab.mu.Unlock()
	if !open {
		return pagePatch{}, errPagePatchClosed
	}
	s := pageCollabLocked(id)
	if !p.Replay {
		pagePatchKeepEvalLocked(s, &p)
		return p, nil
	}
	replayed := 0
	for _, pt := range s.patches {
		if pt.Replay {
			replayed++
		}
	}
	if replayed >= pagePatchesMax {
		return pagePatch{}, fmt.Errorf("the page has %d live patches, the most it keeps; drop some with `page patches --reset`", pagePatchesMax)
	}
	if s.patchBytes+p.size() > pagePatchesMaxBytes {
		return pagePatch{}, fmt.Errorf("the page's live patches would pass %d KiB; drop some with `page patches --reset`", pagePatchesMaxBytes>>10)
	}
	s.patchSeq++
	p.N = s.patchSeq
	p.At = time.Now().UTC()
	s.patches = append(s.patches, p)
	s.patchBytes += p.size()
	return p, nil
}

// POST /v1/pages/{name}/patches {kind:"css",css} or
// {kind:"dom",selector,op,name?,value?} — add a live patch and apply it in
// every open view.
func handlePagePatch(w http.ResponseWriter, r *http.Request) {
	p, group, _, ok := pageForAs(w, r, PermGroupsPagesPatch)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pagePatchMaxBody)
	var req pagePatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	patch, err := checkPagePatch(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// An attribute (onclick, a javascript: URL) or inserted HTML (an
	// onerror) can carry script, so on another agent's page those are
	// eval's to grant, not patch's: CSS, set-text and remove stay patches.
	if group != nil && patch.Kind == "dom" && (patch.Op == "set-attr" || patch.Op == "insert-html") {
		if _, ok := requirePermission(w, r, PermGroupsPagesManage, ActionContext{Group: group.Name}); !ok {
			return
		}
	}
	livePages.mu.Lock()
	kind, stage := p.kind, p.stage
	livePages.mu.Unlock()
	readBack := pageReadBackAllowed(kind, stage, true) == nil
	patch, err = pagePatchAppend(p.id, patch)
	if err != nil {
		pagePatchAppendError(w, err)
		return
	}
	if !readBack {
		// The answers are the page's content, and so is how long they take:
		// for a page that is not read back, nothing waits on the view.
		views := pagePatchNotify(p.id)
		writeJSON(w, http.StatusOK, map[string]any{"page": p.name, "patch": patch.N, "views": views, "withheld": true})
		return
	}
	results := pagePatchBroadcast(r.Context(), p.id)
	writeJSON(w, http.StatusOK, map[string]any{"page": p.name, "patch": patch.N, "views": len(results), "results": results})
}

// pagePatchKeepEvalLocked numbers an eval and lists it, its script clipped,
// dropping the oldest eval past pageEvalsKept. Caller holds pageCollab.mu.
func pagePatchKeepEvalLocked(s *pageCollabState, p *pagePatch) {
	if r := []rune(p.JS); len(r) > pageEvalJSKept {
		p.JS = string(r[:pageEvalJSKept-1]) + "…"
	}
	evals := 0
	for _, pt := range s.patches {
		if !pt.Replay {
			evals++
		}
	}
	if evals >= pageEvalsKept {
		for i, pt := range s.patches {
			if !pt.Replay {
				s.patches = append(s.patches[:i], s.patches[i+1:]...)
				break
			}
		}
	}
	s.patchSeq++
	p.N = s.patchSeq
	p.At = time.Now().UTC()
	s.patches = append(s.patches, *p)
}

func pagePatchAppendError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPagePatchClosed) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeError(w, http.StatusConflict, "too_many_patches", err.Error())
}

// GET /v1/pages/{name}/patches — the page's journal.
func handlePagePatches(w http.ResponseWriter, r *http.Request) {
	p, ok := pageFor(w, r, PermGroupsPagesRead)
	if !ok {
		return
	}
	pageCollab.mu.Lock()
	out := []pagePatch{}
	var seq int64
	views := 0
	if s := pageCollab.byID[p.id]; s != nil {
		out = append(out, s.patches...)
		seq = s.patchSeq
		views = len(s.views)
	}
	pageCollab.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"page": p.name, "seq": seq, "views": views, "patches": out})
}

// DELETE /v1/pages/{name}/patches[?n=N] — drop every patch, or patch N. Open
// views reload without them.
func handlePagePatchesReset(w http.ResponseWriter, r *http.Request) {
	p, ok := pageFor(w, r, PermGroupsPagesPatch)
	if !ok {
		return
	}
	var only int64
	if v := r.URL.Query().Get("n"); v != "" {
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "#"), 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "n is a patch number")
			return
		}
		only = n
	}
	pageCollab.mu.Lock()
	dropped := []int64{}
	left := 0
	if s := pageCollab.byID[p.id]; s != nil {
		keep := s.patches[:0:0]
		s.patchBytes = 0
		for _, pt := range s.patches {
			if only == 0 || pt.N == only {
				dropped = append(dropped, pt.N)
				continue
			}
			keep = append(keep, pt)
			s.patchBytes += pt.size()
		}
		s.patches = keep
		left = len(keep)
	}
	pageCollab.mu.Unlock()
	if only != 0 && len(dropped) == 0 {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("the page has no live patch #%d", only))
		return
	}
	pagePatchNotify(p.id)
	writeJSON(w, http.StatusOK, map[string]any{"page": p.name, "dropped": dropped, "left": left})
}

type pageEvalRequest struct {
	JS string `json:"js"`
}

// pageEvalResult is a view's answer for an eval: the value as JSON text, or
// an error.
type pageEvalResult struct {
	JSON  string `json:"json"`
	Error string `json:"error,omitempty"`
}

func checkPageEvalResult(r *pageEvalResult) bool {
	return len(r.JSON) <= pageEvalResultMax && utf8.ValidString(r.JSON) && (r.JSON == "" || json.Valid([]byte(r.JSON))) &&
		pageTextOK(r.Error, pageDOMErrorMax, false)
}

// POST /v1/pages/{name}/eval {js} — run a script once in one open view and
// return its result as JSON. It is journaled, not replayed.
func handlePageEval(w http.ResponseWriter, r *http.Request) {
	p, ok := pageFor(w, r, PermGroupsPagesManage)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pagePatchMaxBody)
	var req pageEvalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.JS) == "" || !pageTextOK(req.JS, pageEvalJSMax, true) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("the script is 1 to %d printable characters", pageEvalJSMax))
		return
	}
	livePages.mu.Lock()
	kind, stage := p.kind, p.stage
	livePages.mu.Unlock()
	if err := pageReadBackAllowed(kind, stage, true); err != nil {
		writeError(w, http.StatusConflict, "static_source", err.Error())
		return
	}
	patch, err := pagePatchAppend(p.id, pagePatch{Kind: "eval", JS: req.JS})
	if err != nil {
		pagePatchAppendError(w, err)
		return
	}
	pageCollab.mu.Lock()
	best := bestPageViewLocked(pageCollab.byID[p.id])
	var (
		reqID string
		ch    chan []byte
	)
	if best != nil {
		reqID, ch = pageViewAskLocked(best, "eval-result", map[string]any{"type": "eval", "js": req.JS})
	}
	pageCollab.mu.Unlock()
	if best == nil {
		pagePatchDrop(p.id, patch.N)
		writeError(w, http.StatusConflict, "no_view", "the page is not open: no dashboard view of it is open to run the script in; ask the operator to open the page")
		return
	}
	if ch == nil {
		pagePatchDrop(p.id, patch.N)
		writeError(w, http.StatusServiceUnavailable, "busy", "the dashboard view is busy; try again")
		return
	}
	data, answered, err := pageViewWait(r.Context(), ch, pagePatchTimeout)
	pageViewForget(best, "eval-result", reqID)
	var res pageEvalResult
	switch {
	case err != nil:
		res.Error = err.Error()
	case !answered:
		res.Error = "the dashboard view closed"
	case json.Unmarshal(data, &res) != nil || !checkPageEvalResult(&res):
		res = pageEvalResult{Error: "the page's answer was malformed"}
	}
	out := map[string]any{"page": p.name, "patch": patch.N, "json": json.RawMessage("null")}
	if res.JSON != "" && res.Error == "" {
		out["json"] = json.RawMessage(res.JSON)
	}
	if res.Error != "" {
		out["error"] = res.Error
	}
	writeJSON(w, http.StatusOK, out)
}

// pagePatchDrop takes back an eval that never ran.
func pagePatchDrop(id string, n int64) {
	pageCollab.mu.Lock()
	defer pageCollab.mu.Unlock()
	s := pageCollab.byID[id]
	if s == nil {
		return
	}
	for i, p := range s.patches {
		if p.N == n {
			s.patches = append(s.patches[:i], s.patches[i+1:]...)
			s.patchBytes -= p.size()
			return
		}
	}
}

// pagePatchesForSnapshot is the replayed journal, as JSON, for a snapshot.
func pagePatchesForSnapshot(id string) (string, int) {
	pageCollab.mu.Lock()
	defer pageCollab.mu.Unlock()
	s := pageCollab.byID[id]
	if s == nil {
		return "[]", 0
	}
	replay := []pagePatch{}
	for _, p := range s.patches {
		if p.Replay {
			replay = append(replay, p)
		}
	}
	data, _ := json.Marshal(replay)
	return string(data), len(replay)
}
