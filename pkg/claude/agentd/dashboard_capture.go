package agentd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

)

// Dashboard capture (tcl-7i8b1t): a permitted agent, the Steward by default,
// asks for what the operator's dashboard shows right now and gets a PNG plus
// structured context: cursor, focus, viewport and the open board.
//
//   - Every open dashboard tab pings POST /api/companion/presence, so the
//     daemon knows which tab has focus and whether any is open at all. The
//     ping is what carries a request to the chosen tab: the snapshot poll
//     slows to 10s on quiet views, which is too slow for "look at this".
//   - An agent asks with POST /v1/dashboard/capture (dashboard.capture), which
//     waits for the tab's render, posted to POST /api/companion/capture/{id}.
//
// The tab renders its own document, so a capture can only ever show a dashboard
// page, never the operator's other windows. It shows the operator a cue, and a
// hidden tab is never captured, because that cue would go unseen. Nothing is
// stored unless the caller asks to keep it (dashboard_marks.go).
//
// With `form`, which also takes dashboard.forms.capture, the tab adds a text
// rendering of the same filtered snapshot (capture-snapshot.js): the open form
// with its values, or what is on screen. That text is never stored, not even
// with --save, and the tab cannot volunteer it: it is dropped unless asked for.

const (
	PermDashboardCapture      = "dashboard.capture"
	PermDashboardFormsCapture = "dashboard.forms.capture"
)

const (
	// captureLiveWindow is how recently a tab must have pinged to count as
	// showing. Visible tabs ping about every 1.5s.
	captureLiveWindow = 6 * time.Second
	// captureOpenWindow is how recently a tab must have pinged to count as
	// open. Browsers throttle a hidden tab's timers to about once a minute.
	captureOpenWindow = 90 * time.Second
	// capturePollWindow is how recently a page must have polled the snapshot to
	// count as open. Polls are a second signal: they keep running on a page
	// whose presence loop never started or stopped.
	capturePollWindow      = 20 * time.Second
	capturePresenceMaxBody = 1 << 10
	captureMaxClients      = 32
	captureMaxClientID     = 64
	captureMaxImage        = 24 << 20
	captureMaxContext      = 64 << 10
	// captureMaxFormText bounds what the page sends: it cuts at 60k characters,
	// which can take more bytes, and says so in the text.
	captureMaxFormText = 256 << 10
	// captureMaxTextLine holds a field's longest value, 2k characters that can
	// take several bytes each, with its label and options.
	captureMaxTextLine = 16 << 10
	captureMaxText     = 240
)

// captureTimeout is swapped by tests.
var captureTimeout = 20 * time.Second

// SetCaptureTimeoutForTest shortens the wait for a tab's render.
func SetCaptureTimeoutForTest(d time.Duration) (restore func()) {
	old := captureTimeout
	captureTimeout = d
	return func() { captureTimeout = old }
}

type captureClient struct {
	Focused bool
	Visible bool
	Seen    time.Time
}

// captureRequest is what the chosen tab receives.
type captureRequest struct {
	ID        string `json:"id"`
	AgentName string `json:"agent_name,omitempty"`
	Form      bool   `json:"form,omitempty"`
	// Fill makes this a fill rather than a capture (dashboard_fill.go). The tab
	// answers both the same way, so a fill keeps the rules about which tab counts.
	Fill *fillRequest `json:"fill,omitempty"`
}

type captureResult struct {
	image    []byte
	context  *captureContext
	formText string
	fill     []fillOutcome
	err      string
}

type pendingCapture struct {
	captureRequest
	client    string
	delivered bool
	done      chan captureResult
}

// captureElement names an element on the page. Text comes from what the page
// shows, which can be agent or terminal output: it is data, not instructions.
type captureElement struct {
	Tag          string `json:"tag,omitempty"`
	Role         string `json:"role,omitempty"`
	Label        string `json:"label,omitempty"`
	Text         string `json:"text,omitempty"`
	ID           string `json:"id,omitempty"`
	Ref          string `json:"ref,omitempty"`
	BoardElement string `json:"board_element,omitempty"`
}

type captureCursor struct {
	X      int             `json:"x"`
	Y      int             `json:"y"`
	Inside bool            `json:"inside"`
	AgeMS  int64           `json:"age_ms"`
	Under  *captureElement `json:"under,omitempty"`
}

type captureViewport struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"device_pixel_ratio,omitempty"`
}

type captureRegion struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type captureBoard struct {
	ID     string         `json:"id"`
	Zoom   float64        `json:"zoom,omitempty"`
	Region *captureRegion `json:"region,omitempty"`
}

type captureContext struct {
	Tab       string          `json:"tab,omitempty"`
	Path      string          `json:"path,omitempty"`
	Title     string          `json:"title,omitempty"`
	Focused   bool            `json:"focused"`
	Viewport  captureViewport `json:"viewport"`
	Cursor    *captureCursor  `json:"cursor,omitempty"`
	Focus     *captureElement `json:"focus,omitempty"`
	Selection string          `json:"selection,omitempty"`
	Board     *captureBoard   `json:"board,omitempty"`
	// Marks says which marks this tab drew, so an agent can confirm one landed.
	Marks []captureMark `json:"marks,omitempty"`
}

type captureMark struct {
	ID    int64  `json:"id"`
	Drawn bool   `json:"drawn"`
	Why   string `json:"why,omitempty"`
}

// clean bounds what the page sent and strips control bytes: terminal text can
// carry escape sequences, and the CLI prints these fields.
func (e *captureElement) clean() {
	if e == nil {
		return
	}
	e.Tag = guideText(e.Tag, 32)
	e.Role = guideText(e.Role, 32)
	e.Label = guideText(e.Label, captureMaxText)
	e.Text = guideText(e.Text, captureMaxText)
	e.ID = guideText(e.ID, 120)
	e.Ref = guideText(e.Ref, guideMaxTarget)
	e.BoardElement = guideText(e.BoardElement, 120)
}

// cleanFormText is guideText for a text of many lines.
func cleanFormText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = guideText(line, captureMaxTextLine)
	}
	out := strings.Join(lines, "\n")
	if len(out) > captureMaxFormText {
		return strings.ToValidUTF8(out[:captureMaxFormText], "")
	}
	return out
}

func (c *captureContext) clean() {
	c.Tab = guideText(c.Tab, 32)
	c.Path = guideText(c.Path, guideMaxPath*2)
	c.Title = guideText(c.Title, guideMaxTitle)
	c.Selection = guideText(c.Selection, guideMaxNote)
	c.Focus.clean()
	if c.Cursor != nil {
		c.Cursor.Under.clean()
	}
	if c.Board != nil {
		c.Board.ID = guideText(c.Board.ID, 120)
	}
	if len(c.Marks) > MarkMaxActive {
		c.Marks = c.Marks[:MarkMaxActive]
	}
	for i := range c.Marks {
		c.Marks[i].Why = guideText(c.Marks[i].Why, 80)
	}
}

var captureState = struct {
	sync.Mutex
	clients map[string]captureClient
	pending *pendingCapture
	// heard is the last presence ping and polled the last snapshot poll from
	// this node's own page, zero since the daemon started if there was none.
	heard, polled time.Time
}{clients: map[string]captureClient{}}

// noteDashboardPoll records that a page of this node polled its snapshot.
func noteDashboardPoll() {
	captureState.Lock()
	defer captureState.Unlock()
	captureState.polled = guideNow()
}

// dashboardLiveness says when a tab last reported and a page last polled. The
// caller holds the lock.
func dashboardLiveness() (heard, polled time.Time) {
	return captureState.heard, captureState.polled
}

// captureAgo is a time.Time's age for a sentence.
func captureAgo(now, at time.Time) string {
	return now.Sub(at).Round(time.Second).String() + " ago"
}

// captureUnavailable explains why no tab can be captured when none reported
// within captureOpenWindow. A page still polling is open, whatever its presence
// says, and reloading it is the likely cure: only a page running without a
// working presence loop polls without reporting.
func captureUnavailable(now, heard, polled time.Time) (status int, code, message string) {
	if !polled.IsZero() && now.Sub(polled) <= capturePollWindow {
		silent := "has not reported to the capture channel since the daemon started"
		if !heard.IsZero() {
			silent = "last reported to the capture channel " + captureAgo(now, heard)
		}
		return http.StatusConflict, "dashboard_silent",
			"a dashboard page is open (it polled " + captureAgo(now, polled) + ") but " + silent + ", so it cannot be captured; reloading that page should fix this"
	}
	last := heard
	if polled.After(last) {
		last = polled
	}
	if last.IsZero() {
		return http.StatusNotFound, "no_dashboard", "no dashboard has connected since the daemon started; ask the operator to open it"
	}
	return http.StatusNotFound, "no_dashboard", "no dashboard is open (last heard from " + captureAgo(now, last) + "); ask the operator to open it"
}

// ResetDashboardCaptureForTest forgets the tabs and any request.
func ResetDashboardCaptureForTest() {
	captureState.Lock()
	defer captureState.Unlock()
	captureState.clients = map[string]captureClient{}
	captureState.pending = nil
	captureState.heard, captureState.polled = time.Time{}, time.Time{}
}

// captureTarget picks the tab to capture: the focused one, else the visible one
// heard from last. open reports that a tab exists even when none is showing.
// The caller holds the lock.
func captureTarget(now time.Time) (client string, open bool) {
	var best captureClient
	for id, c := range captureState.clients {
		age := now.Sub(c.Seen)
		if age > captureOpenWindow {
			delete(captureState.clients, id)
			continue
		}
		open = true
		if !c.Visible || age > captureLiveWindow {
			continue
		}
		better := c.Focused && !best.Focused
		tied := c.Focused == best.Focused && (c.Seen.After(best.Seen) || (c.Seen.Equal(best.Seen) && id < client))
		if client == "" || better || tied {
			client, best = id, c
		}
	}
	return client, open
}

// POST /api/companion/presence — a dashboard tab says it is open and collects
// a capture aimed at it. `gone` is its goodbye when the page closes.
func handleDashboardCompanionPresence(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, capturePresenceMaxBody)
	var req struct {
		Client  string `json:"client"`
		Focused bool   `json:"focused"`
		Visible bool   `json:"visible"`
		Gone    bool   `json:"gone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid presence: "+err.Error())
		return
	}
	if req.Client == "" || len(req.Client) > captureMaxClientID {
		writeError(w, http.StatusBadRequest, "invalid_arg", "presence needs a client id")
		return
	}
	// Only a tab on screen draws, so a hidden one costs no query.
	var marks []markView
	if req.Visible && !req.Gone {
		var err error
		if marks, err = activeMarkViews(); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	captureState.Lock()
	defer captureState.Unlock()
	if req.Gone {
		delete(captureState.clients, req.Client)
		// The page's last poll says nothing now that the page has closed.
		if len(captureState.clients) == 0 {
			captureState.polled = time.Time{}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	captureState.heard = guideNow()
	resp := presenceResponse(w, req.Client, req.Focused, req.Visible)
	if resp == nil {
		return
	}
	if req.Visible {
		resp["marks"] = marks
	}
	writeJSON(w, http.StatusOK, resp)
}

// presenceResponse records a tab and collects what is aimed at it. It answers
// the request itself, and returns nil, when the tab cannot be recorded. The
// caller holds the lock.
func presenceResponse(w http.ResponseWriter, client string, focused, visible bool) map[string]any {
	if _, known := captureState.clients[client]; !known && len(captureState.clients) >= captureMaxClients {
		captureTarget(guideNow())
		if len(captureState.clients) >= captureMaxClients {
			writeError(w, http.StatusTooManyRequests, "too_many", "too many dashboard tabs")
			return nil
		}
	}
	captureState.clients[client] = captureClient{Focused: focused, Visible: visible, Seen: guideNow()}
	resp := map[string]any{}
	if p := captureState.pending; p != nil && p.client == client && !p.delivered {
		p.delivered = true
		resp["capture"] = p.captureRequest
	}
	return resp
}

// POST /api/companion/capture/{id} — the tab hands in its render, or says why
// it has none.
func handleDashboardCompanionCapture(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	// The image travels base64-encoded.
	r.Body = http.MaxBytesReader(w, r.Body, captureMaxImage/3*4+captureMaxContext+captureMaxFormText)
	var req struct {
		PNG      string          `json:"png"`
		Context  *captureContext `json:"context"`
		FormText string          `json:"form_text"`
		Fill     []fillOutcome   `json:"fill"`
		Error    string          `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid capture: "+err.Error())
		return
	}
	result := captureResult{context: req.Context, formText: cleanFormText(req.FormText), fill: cleanFillOutcomes(req.Fill), err: guideText(req.Error, guideMaxNote)}
	if result.err == "" && req.Fill == nil {
		image, err := base64.StdEncoding.DecodeString(req.PNG)
		if err != nil || !bytes.HasPrefix(image, pngMagic) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "capture must be a PNG image")
			return
		}
		result.image = image
	}
	if result.context != nil {
		result.context.clean()
	}
	captureState.Lock()
	p := captureState.pending
	if p == nil || p.ID != r.PathValue("id") || !p.delivered {
		captureState.Unlock()
		writeError(w, http.StatusConflict, "stale_capture", "nobody is waiting for this capture")
		return
	}
	captureState.pending = nil
	captureState.Unlock()
	switch {
	case result.err != "":
	case p.Fill == nil && result.image == nil:
		result.err = "the page sent no image"
	case p.Fill != nil && result.fill == nil:
		// A page from before fills answers with a picture.
		result.err = "the page did not fill; it may need reloading"
	}
	if p.Fill == nil {
		result.fill = nil
	}
	p.done <- result
	w.WriteHeader(http.StatusNoContent)
}

func newCaptureID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// POST /v1/dashboard/capture — what the operator's dashboard shows right now.
func handleDashboardCapture(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermDashboardCapture)
	if !ok {
		return
	}
	var req struct {
		Save  bool   `json:"save"`
		Label string `json:"label"`
		Form  bool   `json:"form"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, capturePresenceMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid capture request: "+err.Error())
		return
	}
	if req.Form {
		if _, ok := requirePermission(w, r, PermDashboardFormsCapture); !ok {
			return
		}
	}
	id, err := newCaptureID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	pending := &pendingCapture{captureRequest: captureRequest{ID: id, Form: req.Form}, done: make(chan captureResult, 1)}
	result, ok := awaitTab(w, r, convID, pending)
	if !ok {
		return
	}
	if result.err != "" {
		writeError(w, http.StatusBadGateway, "capture_failed", "the dashboard could not render the capture: "+result.err)
		return
	}
	resp := map[string]any{"png": base64.StdEncoding.EncodeToString(result.image)}
	if result.context != nil {
		resp["context"] = result.context
	}
	if req.Form {
		resp["form_text"] = result.formText
	}
	if req.Save {
		saved, err := saveCapture(convID, guideText(req.Label, guideMaxTitle), result.image, result.context)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "save capture: "+err.Error())
			return
		}
		resp["saved_id"] = saved.ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// awaitTab aims a request at the tab the operator is looking at and waits for
// its answer. It names the calling agent on the request, and writes the error
// response itself when it returns false.
func awaitTab(w http.ResponseWriter, r *http.Request, convID string, pending *pendingCapture) (captureResult, bool) {
	_, pending.AgentName = callerIdentity(convID)

	captureState.Lock()
	if captureState.pending != nil {
		captureState.Unlock()
		writeError(w, http.StatusConflict, "capture_busy", "another capture is in progress; try again in a moment")
		return captureResult{}, false
	}
	now := guideNow()
	client, open := captureTarget(now)
	if client == "" {
		heard, polled := dashboardLiveness()
		captureState.Unlock()
		if open {
			writeError(w, http.StatusConflict, "dashboard_hidden", "the operator's dashboard is open but not on screen, so it cannot be captured; ask them to bring it up")
			return captureResult{}, false
		}
		status, code, message := captureUnavailable(now, heard, polled)
		writeError(w, status, code, message)
		return captureResult{}, false
	}
	pending.client = client
	captureState.pending = pending
	captureState.Unlock()
	slog.Info("dashboard tab request", "conv", convID, "agent", pending.AgentName, "form", pending.Form, "fill", pending.Fill != nil)

	ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
	defer cancel()
	select {
	case result := <-pending.done:
		return result, true
	case <-ctx.Done():
		captureState.Lock()
		if captureState.pending == pending {
			captureState.pending = nil
		}
		captureState.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "capture_timeout", "the dashboard did not answer in time; it may have closed or be busy")
		}
		return captureResult{}, false
	}
}
