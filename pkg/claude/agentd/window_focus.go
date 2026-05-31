package agentd

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Bulk window-focus endpoint for the dashboard. One POST route,
// /api/agent-windows, behind the same cookie + Origin pin as every
// other /api mutation. It is purely a DESKTOP-WINDOW concern: it
// raises, opens or dismisses the terminal windows of a set of agents.
// It NEVER stops, kills or detaches an agent PROCESS — every agent
// keeps running untouched; only the windows move.
//
// Two directions, picked by the request body:
//   - "focus"   — raise the OS terminal window attached to each agent's
//                 tmux session, opening a fresh window when none is
//                 open. The same per-agent call POST /api/jump/{conv}
//                 makes, applied in bulk.
//   - "unfocus" — detach every tmux client from each agent's session,
//                 so the windows go away and the desktop is
//                 decluttered. The agent process keeps running; the
//                 window can be brought back at any time with focus.
//
// unfocus is detach-only — it runs `tmux detach-client`, never a
// window-close. tmux clients are per-tty, so detaching is per window
// OR per tab: a sibling tab and the window frame are never touched. A
// terminal that tclaude spawned (the focus button / auto-focus open a
// window or tab whose sole command is `tclaude session attach`) exits
// and closes ITSELF once detach ends that attach process — and only
// that one tab/window. A shell the human attached by hand just returns
// to its prompt. We deliberately do not hunt down the OS window and
// close it: that would be the heavy-handed thing that closes a whole
// multi-tab window.
//
// Two scopes, picked the same way as the shutdown / power-on buttons:
//   - {"scope":"group","group":"<name>"} — the members of one group.
//   - {"scope":"all"}                    — every active agent on the
//     dashboard roster (db.ListActiveAgents): grouped and ungrouped
//     alike.
//
// The dashboard's selection modal narrows the set further: an optional
// "convs" list is the explicit subset the human ticked (by role, by
// agent, by filter — all selected by default). When "convs" is absent
// the whole scope is acted on — the select-all default, and the
// pure-scope path the flow test drives. A "convs" entry outside the
// resolved scope is dropped, so the group modal can never reach an
// agent outside its group.
//
// Permission: this endpoint follows the same gate as the per-agent
// focus endpoint (handleDashboardJumpAPI) and the shutdown / power-on
// endpoints — the dashboard cookie + Origin pin IS the human-consent
// layer. Focusing a window is a human-desktop operation with no /v1
// twin and no permission slug, so there is no shared permission-checked
// handler to funnel through (the asDashboardHumanPeer pattern from
// commit 6a1ade5 applies only where such a /v1 handler exists). This
// is the consistent pattern, not a divergent one.

// Per-agent outcome strings. Stable wire values — the dashboard reads
// them back into the result toast.
const (
	windowFocused  = "focused"   // focus dispatched (raised or opened a window)
	windowDetached = "detached"  // unfocus detached >=1 tmux client (window dismissed)
	windowNoWindow = "no_window" // unfocus ran but the agent had no window open — a no-op
	windowFailed   = "failed"    // the underlying tmux op errored
)

// agentWindowOutcome is the per-agent result of one window op.
type agentWindowOutcome struct {
	ConvID  string `json:"conv_id"`
	Title   string `json:"title,omitempty"`
	Outcome string `json:"outcome"`          // focused | detached | no_window | failed
	Detail  string `json:"detail,omitempty"` // human-readable note (window count / error)
}

// agentWindowsResp is the wire shape returned by /api/agent-windows.
// Agents is always non-nil so the dashboard can iterate it
// unconditionally.
type agentWindowsResp struct {
	Direction string               `json:"direction"`
	Scope     string               `json:"scope"`
	Group     string               `json:"group,omitempty"`
	Targeted  int                  `json:"targeted"`
	Focused   int                  `json:"focused"`
	Detached  int                  `json:"detached"`
	NoWindow  int                  `json:"no_window"`
	Failed    int                  `json:"failed"`
	Agents    []agentWindowOutcome `json:"agents"`
}

// focusAgentWindow raises — or, when none is open, opens — the OS
// terminal window attached to one agent's tmux session. It is the
// per-agent unit the bulk endpoint dispatches for direction "focus",
// the same call POST /api/jump/{conv} makes for a single agent.
//
// Seam: production focuses for real via
// session.TryFocusAttachedSessionWithID (per-platform: AppleScript /
// wmctrl / PowerShell); flow tests swap in a recorder. The OS-window
// effect is not unit-testable — tests assert the dispatch set instead.
// Mirrors the openTerminal seam in dir.go.
var focusAgentWindow = func(sess *db.SessionRow) {
	// Pass the session ID explicitly so the WSL focus path can match
	// "tclaude:<id>" titles — same reason handleDashboardJumpAPI does.
	session.TryFocusAttachedSessionWithID(sess.TmuxSession, sess.ID)
}

// detachAgentWindows detaches every tmux client attached to one
// agent's session — the agent PROCESS keeps running untouched; only
// the terminal windows go away. Returns the number of clients
// detached (0 when the agent had no window open — a clean no-op).
// The per-agent unit dispatched for direction "unfocus".
//
// Seam, same rationale as focusAgentWindow.
var detachAgentWindows = func(sess *db.SessionRow) (int, error) {
	return session.DetachSessionClients(sess.TmuxSession)
}

// handleAgentWindows is the cookie-auth endpoint behind the
// dashboard's group-level and whole-dashboard window focus/unfocus
// triggers. Body:
//
//	POST /api/agent-windows
//	  {"direction":"focus"|"unfocus",
//	   "scope":"group","group":"<name>"}   — one group's agents
//	  {"direction":"focus"|"unfocus","scope":"all"} — every active agent
//	  optional: "convs": ["<conv-id>", …]  — the modal's explicit
//	                                         selection; absent → all in
//	                                         scope (select-all default)
//
// Window-only: it never stops an agent process. See the file comment.
func handleAgentWindows(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Direction string   `json:"direction"`
		Scope     string   `json:"scope"`
		Group     string   `json:"group"`
		Convs     []string `json:"convs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	direction := strings.TrimSpace(body.Direction)
	switch direction {
	case "focus", "unfocus":
	default:
		http.Error(w, "invalid direction "+strconv.Quote(direction)+
			` (expected "focus" or "unfocus")`, http.StatusBadRequest)
		return
	}

	scope := strings.TrimSpace(body.Scope)
	groupName := ""
	var universe []string
	switch scope {
	case "group":
		groupName = strings.TrimSpace(body.Group)
		if groupName == "" {
			http.Error(w, `scope "group" requires a non-empty "group" name`, http.StatusBadRequest)
			return
		}
		g, err := db.GetAgentGroupByName(groupName)
		if err != nil {
			http.Error(w, "group lookup: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if g == nil {
			http.Error(w, "no such group "+groupName, http.StatusNotFound)
			return
		}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			http.Error(w, "list members: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, m := range members {
			universe = append(universe, m.ConvID)
		}
	case "all":
		agents, err := db.ListActiveAgents()
		if err != nil {
			http.Error(w, "list agents: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range agents {
			universe = append(universe, a.ConvID)
		}
	default:
		http.Error(w, "invalid scope "+strconv.Quote(scope)+
			` (expected "group" or "all")`, http.StatusBadRequest)
		return
	}

	resp := runWindowOp(direction, scope, groupName, universe, body.Convs)
	writeJSON(w, http.StatusOK, resp)
}

// runWindowOp applies one window direction (focus|unfocus) to the agents
// in `universe`, narrowed by the modal's explicit `convs` selection
// (empty → the whole universe). It resolves each selected conv to its
// live tmux session, dispatches the per-agent op in parallel, and folds
// the per-agent outcomes into the summary the dashboard renders.
//
// Pure over its inputs and the two per-agent seams. The dashboard HTTP
// handler calls it after parsing/validating the request; the tray's
// "Unfocus all agents" item calls it via unfocusAllAgentWindows without
// an HTTP round-trip, since the tray runs inside agentd.
func runWindowOp(direction, scope, group string, universe, convs []string) agentWindowsResp {
	// Narrow the scope to the modal's explicit selection when one is
	// provided; an entry outside the resolved universe is dropped.
	targets := selectWindowTargets(universe, convs)

	// Resolve each selected conv to its live tmux session. Offline
	// agents have neither a session to focus nor a window to detach, so
	// they drop out here with no outcome row — same collection rule as
	// the shutdown buttons.
	type aliveTarget struct {
		convID string
		sess   *db.SessionRow
	}
	var alive []aliveTarget
	for _, convID := range targets {
		if sess := pickAliveSession(convID); sess != nil {
			alive = append(alive, aliveTarget{convID: convID, sess: sess})
		}
	}

	// Apply the op to every agent in PARALLEL so one slow tmux call
	// can't delay the rest — same shape as runShutdown.
	outcomes := make([]agentWindowOutcome, len(alive))
	var wg sync.WaitGroup
	for i := range alive {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = applyWindowOp(direction, alive[i].convID, alive[i].sess)
		}(i)
	}
	wg.Wait()

	// Deterministic order so the dashboard list (and the flow test)
	// don't depend on goroutine scheduling.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].ConvID < outcomes[j].ConvID })

	resp := agentWindowsResp{
		Direction: direction,
		Scope:     scope,
		Group:     group,
		Targeted:  len(outcomes),
		Agents:    outcomes,
	}
	for _, o := range outcomes {
		switch o.Outcome {
		case windowFocused:
			resp.Focused++
		case windowDetached:
			resp.Detached++
		case windowNoWindow:
			resp.NoWindow++
		default:
			resp.Failed++
		}
	}
	return resp
}

// unfocusAllAgentWindows detaches the terminal windows of every active
// agent on the dashboard roster — the in-process twin of POST
// /api/agent-windows with {"direction":"unfocus","scope":"all"}. The
// tray's "Unfocus all agents" item calls it directly: the tray runs
// inside agentd, so it needs no socket round-trip. Window-only — no
// agent process is stopped, and every detached window can be brought
// back with focus. Returns the per-agent outcome summary, or an error
// when the active-agent roster can't be read.
func unfocusAllAgentWindows() (agentWindowsResp, error) {
	agents, err := db.ListActiveAgents()
	if err != nil {
		return agentWindowsResp{}, err
	}
	universe := make([]string, 0, len(agents))
	for _, a := range agents {
		universe = append(universe, a.ConvID)
	}
	return runWindowOp("unfocus", "all", "", universe, nil), nil
}

// selectWindowTargets resolves the set of conv-ids the bulk op acts
// on: the scope `universe`, optionally narrowed to `convs`. An empty
// `convs` returns the whole universe (the select-all default).
// Otherwise the result is the intersection — a `convs` entry outside
// the universe is ignored, so the group modal cannot be used to reach
// an agent outside its group. Always de-duplicated.
func selectWindowTargets(universe, convs []string) []string {
	inUniverse := make(map[string]bool, len(universe))
	for _, id := range universe {
		if id != "" {
			inUniverse[id] = true
		}
	}
	seen := make(map[string]bool, len(universe))
	out := []string{}
	keep := func(id string) {
		if id == "" || seen[id] || !inUniverse[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(convs) == 0 {
		for _, id := range universe {
			keep(id)
		}
		return out
	}
	for _, id := range convs {
		keep(strings.TrimSpace(id))
	}
	return out
}

// applyWindowOp runs one direction's per-agent window op and maps it
// to an outcome. focus is best-effort (TryFocusAttachedSession logs
// but never errors, exactly like the per-agent jump endpoint);
// unfocus reports how many windows it dismissed.
func applyWindowOp(direction, convID string, sess *db.SessionRow) agentWindowOutcome {
	out := agentWindowOutcome{ConvID: convID, Title: agent.FreshTitle(convID)}
	if direction == "focus" {
		focusAgentWindow(sess)
		out.Outcome = windowFocused
		return out
	}
	// unfocus — detach every tmux client; the agent process is untouched.
	n, err := detachAgentWindows(sess)
	switch {
	case err != nil:
		out.Outcome = windowFailed
		out.Detail = "detach failed: " + err.Error()
	case n == 0:
		out.Outcome = windowNoWindow
		out.Detail = "no window was open"
	case n == 1:
		out.Outcome = windowDetached
		out.Detail = "1 window detached"
	default:
		out.Outcome = windowDetached
		out.Detail = strconv.Itoa(n) + " windows detached"
	}
	return out
}
