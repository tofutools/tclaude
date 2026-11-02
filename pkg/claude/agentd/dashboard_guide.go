package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// Dashboard guidance (tcl-z7sbof): the "useful Clippy" half of the companion.
// A permitted agent — the Steward by default — can see which dashboard view
// the operator is on, and can point at it: switch the operator's dashboard to
// a view and highlight a control there with a short note. It can never press a
// button or change state through this surface; a cue only moves and marks the
// operator's own view, and the operator acts.
//
//   - The dashboard reports its current view (POST /api/companion/view) as the
//     operator navigates. Agents read it with GET /v1/dashboard/view
//     (dashboard.view.read).
//   - An agent posts a cue (POST /v1/dashboard/cue, dashboard.guide). Cues are
//     held in memory for a short while and ride the dashboard snapshot
//     (companion_cues); the companion island plays each one once.
//
// Both are in memory only: a daemon restart forgets them, which is correct
// for "where is the operator looking right now" and "point at this now".

const (
	PermDashboardViewRead = "dashboard.view.read"
	PermDashboardGuide    = "dashboard.guide"
)

const (
	guideMaxCues     = 16
	guideCueTTL      = 45 * time.Second
	guideMaxPath     = 240
	guideMaxTarget   = 120
	guideMaxNote     = 400
	guideMaxTitle    = 160
	guideViewMaxBody = 4 << 10
	// A cue carries up to guideMaxMarks marks of two texts each; texts are
	// cut to length after decoding, so the body may carry them long.
	guideCueMaxBody = 32 << 10
)

// guideTabs are the dashboard's top-level tabs a cue may switch to.
var guideTabs = map[string]bool{
	"groups": true, "processes": true, "terminals": true, "messages": true, "jobs": true,
	"usage": true, "config": true, "access": true, "diagnostics": true, "plugins": true,
	"map": true, "fleet-admin": true, "boards": true,
}

type dashboardView struct {
	Tab   string    `json:"tab"`
	Path  string    `json:"path"`
	Title string    `json:"title,omitempty"`
	At    time.Time `json:"at"`
}

// cueMark is a drawn mark on the operator's view (tcl-0waby5): a circle or
// underline around the control whose visible text is Target, or an arrow
// from the control named From to the one named Target.
type cueMark struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	From   string `json:"from,omitempty"`
}

const guideMaxMarks = 6

type companionCue struct {
	ID        int64     `json:"id"`
	AgentID   string    `json:"agent_id,omitempty"`
	AgentName string    `json:"agent_name,omitempty"`
	Tab       string    `json:"tab,omitempty"`
	Path      string    `json:"path,omitempty"`
	Target    string    `json:"target,omitempty"`
	Note      string    `json:"note,omitempty"`
	Marks     []cueMark `json:"marks,omitempty"`
	// Open is an agent the operator's companion window should open: the
	// Steward handing them the agent that needs them.
	Open      string    `json:"open,omitempty"`
	At        time.Time `json:"at"`
	ExpiresAt time.Time `json:"expires_at"`
}

var guideState = struct {
	sync.Mutex
	view   dashboardView
	cues   []companionCue
	nextID int64
}{}

// guideNow is swapped by tests.
var guideNow = time.Now

// ResetDashboardGuideForTest clears the in-memory view and cues.
func ResetDashboardGuideForTest() {
	guideState.Lock()
	defer guideState.Unlock()
	guideState.view = dashboardView{}
	guideState.cues = nil
	guideState.nextID = 0
}

// activeCompanionCues returns the unexpired cues, oldest first, for the
// dashboard snapshot.
func activeCompanionCues() []companionCue {
	guideState.Lock()
	defer guideState.Unlock()
	now := guideNow()
	out := make([]companionCue, 0, len(guideState.cues))
	for _, cue := range guideState.cues {
		if cue.ExpiresAt.After(now) {
			out = append(out, cue)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// guideText keeps printable text only (newlines become spaces), trimmed and
// clipped to max bytes on a rune boundary. Everything an agent sends here is
// rendered as text by the dashboard; this bounds it and strips control bytes.
func guideText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsPrint(r):
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}

// guidePath accepts a same-origin dashboard path: it starts with a single "/",
// carries no scheme, backslash or control characters, and is bounded.
func guidePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", true
	}
	if len(p) > guideMaxPath || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsAny(p, "\\ \t\r\n") || strings.Contains(p, "://") {
		return "", false
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return p, true
}

// guideCuePath is guidePath for cues: a cue only points, so it may not name a
// location whose restore has side effects. A /terminals/<agent> selection
// reattaches that agent's browser terminal, taking the session over from any
// other web view, so cues may open the Terminals tab but not a terminal.
func guideCuePath(p string) (string, bool) {
	p, ok := guidePath(p)
	if !ok {
		return "", false
	}
	if p == "" {
		return "", true
	}
	// The browser resolves dot segments and the router decodes escapes, so the
	// location is judged as they will read it, and a path that means something
	// other than what it says is refused outright.
	pathname, _, _ := strings.Cut(p, "?")
	pathname, _, _ = strings.Cut(pathname, "#")
	decoded, err := url.PathUnescape(pathname)
	if err != nil {
		return "", false
	}
	clean := path.Clean(decoded)
	if clean != strings.TrimSuffix(decoded, "/") && decoded != "/" {
		return "", false
	}
	if strings.HasPrefix(clean, "/terminals/") {
		return "", false
	}
	return p, true
}

// guideViewPath validates a reported view: the pathname must be a dashboard
// path, while the query (a Messages search, say) is kept as printable text.
func guideViewPath(p string) (string, bool) {
	pathname, query, _ := strings.Cut(strings.TrimSpace(p), "?")
	pathname, ok := guidePath(pathname)
	if !ok {
		return "", false
	}
	if query != "" {
		query = guideText(query, guideMaxPath)
		if query != "" {
			pathname += "?" + query
		}
	}
	if len(pathname) > guideMaxPath*2 {
		pathname = pathname[:guideMaxPath*2]
	}
	return pathname, true
}

// POST /api/companion/view — the dashboard reports the operator's view.
func handleDashboardCompanionView(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, guideViewMaxBody)
	var req struct {
		Tab   string `json:"tab"`
		Path  string `json:"path"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid view: "+err.Error())
		return
	}
	path, ok := guideViewPath(req.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path must be a dashboard path")
		return
	}
	view := dashboardView{Tab: guideText(req.Tab, 32), Path: path, Title: guideText(req.Title, guideMaxTitle), At: guideNow()}
	guideState.Lock()
	guideState.view = view
	guideState.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/dashboard/view — which dashboard view the operator is on.
func handleDashboardViewRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermDashboardViewRead); !ok {
		return
	}
	guideState.Lock()
	view := guideState.view
	guideState.Unlock()
	resp := map[string]any{"known": !view.At.IsZero()}
	if !view.At.IsZero() {
		resp["tab"] = view.Tab
		resp["path"] = view.Path
		resp["title"] = view.Title
		resp["at"] = view.At
		resp["age_seconds"] = int(guideNow().Sub(view.At).Seconds())
	}
	captureState.Lock()
	heard, polled := dashboardLiveness()
	captureState.Unlock()
	if !heard.IsZero() {
		resp["tab_heard_age_seconds"] = int(guideNow().Sub(heard).Seconds())
	}
	if !polled.IsZero() {
		resp["page_polled_age_seconds"] = int(guideNow().Sub(polled).Seconds())
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1/dashboard/cue — move the operator's dashboard to a view and/or
// highlight a control there with a note.
func handleDashboardCue(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermDashboardGuide)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, guideCueMaxBody)
	var req struct {
		Tab    string    `json:"tab"`
		Path   string    `json:"path"`
		Target string    `json:"target"`
		Note   string    `json:"note"`
		Marks  []cueMark `json:"marks"`
		Open   string    `json:"open"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid cue: "+err.Error())
		return
	}
	if len(req.Marks) > guideMaxMarks {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("at most %d marks per cue", guideMaxMarks))
		return
	}
	marks := make([]cueMark, 0, len(req.Marks))
	for _, m := range req.Marks {
		mark := cueMark{Kind: strings.ToLower(strings.TrimSpace(m.Kind)), Target: guideText(m.Target, guideMaxTarget), From: guideText(m.From, guideMaxTarget)}
		switch {
		case mark.Kind != "circle" && mark.Kind != "underline" && mark.Kind != "arrow":
			writeError(w, http.StatusBadRequest, "invalid_arg", "a mark is circle, underline or arrow")
			return
		case mark.Target == "":
			writeError(w, http.StatusBadRequest, "invalid_arg", "a mark needs the visible text of its target")
			return
		case (mark.Kind == "arrow") != (mark.From != ""):
			writeError(w, http.StatusBadRequest, "invalid_arg", "an arrow mark needs from and target; other marks take no from")
			return
		}
		marks = append(marks, mark)
	}
	tab := strings.TrimSpace(strings.ToLower(req.Tab))
	if tab != "" && !guideTabs[tab] {
		writeError(w, http.StatusBadRequest, "invalid_arg", "unknown tab "+guideText(tab, 32)+"; known: groups, processes, terminals, messages, jobs, usage, config, access, diagnostics, plugins, map, fleet-admin, boards")
		return
	}
	path, ok := guideCuePath(req.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path must be a dashboard path such as /processes/templates/<id> (a cue may open the Terminals tab, not a terminal)")
		return
	}
	target := guideText(req.Target, guideMaxTarget)
	note := guideText(req.Note, guideMaxNote)
	open := ""
	if sel := strings.TrimSpace(req.Open); sel != "" {
		// The cached resolver: no filesystem rescan on a miss.
		res, _, err := agent.ResolveSelectorCached(sel)
		if err != nil || res == nil || res.AgentID == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "open names no agent: "+guideText(sel, 64))
			return
		}
		open = res.AgentID
	}
	if tab == "" && path == "" && target == "" && note == "" && len(marks) == 0 && open == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a cue needs a tab, a path, a target, a note, a mark or an agent to open")
		return
	}
	cue := companionCue{Tab: tab, Path: path, Target: target, Note: note, Marks: marks, Open: open}
	cue.AgentID, cue.AgentName = callerIdentity(convID)
	now := guideNow()
	guideState.Lock()
	guideState.nextID++
	cue.ID = guideState.nextID
	cue.At = now
	cue.ExpiresAt = now.Add(guideCueTTL)
	guideState.cues = append(guideState.cues, cue)
	if len(guideState.cues) > guideMaxCues {
		guideState.cues = guideState.cues[len(guideState.cues)-guideMaxCues:]
	}
	guideState.Unlock()
	captureState.Lock()
	client, _ := captureTarget(now)
	captureState.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": cue.ID, "expires_at": cue.ExpiresAt, "dashboard_on_screen": client != ""})
}
