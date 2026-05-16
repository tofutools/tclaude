package agentd

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Emergency-shutdown endpoint for the dashboard. One POST route,
// /api/emergency-shutdown, behind the same cookie + Origin pin as
// every other /api mutation. It stops a scope of running agents fast
// — and ONLY stops them: no conversation, enrollment, group or
// permission row is touched, so every shut-down agent is reinstatable
// by simply resuming its session.
//
// Two scopes, picked by the request body:
//   - {"scope":"group","group":"<name>"} — every alive member of one
//     group (the same membership set `tclaude agent groups stop`
//     walks; owner-only rows are not members and are left alone).
//   - {"scope":"all"} — every alive agent on the dashboard's active
//     roster (db.ListActiveAgents): grouped and ungrouped alike.
//     Retired/superseded convs are excluded by ListActiveAgents. The
//     request comes from the human's browser, so there is no "self"
//     to exclude.
//
// Per agent the shutdown escalates gracefully: inject /exit (soft),
// wait a grace window, and force-kill the tmux session only if the
// agent is still alive when the window closes. Every agent in scope
// is escalated in PARALLEL so one hung agent can't delay the rest.

// emergencyShutdownGrace is the soft→hard escalation window: how long
// an agent has to honour its injected /exit before it gets
// force-killed. A package var (not a const) so it stays overridable;
// the per-request `grace_ms` body field overrides it per call — the
// flow test passes a few milliseconds so it never sleeps for real
// seconds.
var emergencyShutdownGrace = 10 * time.Second

// emergencyPollInterval is the gap between liveness polls while
// waiting out the grace window. Small enough that a quick exiter is
// noticed promptly, large enough not to spin.
var emergencyPollInterval = 100 * time.Millisecond

// emergencyShutdownGraceCap bounds the per-request grace override so a
// browser bug (or a fat-fingered value) can't wedge the request for
// minutes.
const emergencyShutdownGraceCap = 2 * time.Minute

// Per-agent outcome strings. Stable wire values — the dashboard reads
// them back into the result toast.
const (
	emShutdownExited  = "exited_gracefully" // honoured /exit within the grace window
	emShutdownKilled  = "force_killed"      // still alive after grace → tmux kill-session
	emShutdownOffline = "already_offline"   // raced — exited between collection and escalation
	emShutdownFailed  = "failed"            // neither /exit nor kill-session succeeded
)

// emergencyAgentOutcome is the per-agent result of one escalation.
type emergencyAgentOutcome struct {
	ConvID  string `json:"conv_id"`
	Title   string `json:"title,omitempty"`
	Outcome string `json:"outcome"`          // exited_gracefully | force_killed | already_offline | failed
	Detail  string `json:"detail,omitempty"` // human-readable note (escalation reason / error)
}

// emergencyShutdownResp is the wire shape returned by
// /api/emergency-shutdown. Agents is always non-nil so the dashboard
// can iterate it unconditionally.
type emergencyShutdownResp struct {
	Scope            string                  `json:"scope"`
	Group            string                  `json:"group,omitempty"`
	GraceMs          int64                   `json:"grace_ms"`
	Targeted         int                     `json:"targeted"`
	ExitedGracefully int                     `json:"exited_gracefully"`
	ForceKilled      int                     `json:"force_killed"`
	AlreadyOffline   int                     `json:"already_offline"`
	Failed           int                     `json:"failed"`
	Agents           []emergencyAgentOutcome `json:"agents"`
}

// handleEmergencyShutdown is the cookie-auth endpoint behind the
// dashboard's group-level and whole-dashboard emergency-shutdown
// buttons. Body:
//
//	POST /api/emergency-shutdown
//	  {"scope":"group","group":"<name>"}   — one group's alive members
//	  {"scope":"all"}                      — every alive agent
//	  optional: "grace_ms": <int>          — override the escalation
//	                                         window (default 10s; the
//	                                         flow test passes a tiny
//	                                         value so it never really
//	                                         sleeps)
//
// The dashboard cookie + Origin pin is the human-consent layer, same
// as every other /api mutation. The op is stop-only — it never
// deletes a conversation, enrollment, group membership or permission
// — so it needs no confirm beyond the modal the dashboard already
// shows.
func handleEmergencyShutdown(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Scope   string `json:"scope"`
		Group   string `json:"group"`
		GraceMs *int64 `json:"grace_ms"` // nil → emergencyShutdownGrace default
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve the grace window: the body override wins, clamped to
	// [0, cap]. 0 is legitimate ("no grace — escalate immediately").
	grace := emergencyShutdownGrace
	if body.GraceMs != nil {
		ms := *body.GraceMs
		if ms < 0 {
			ms = 0
		}
		grace = time.Duration(ms) * time.Millisecond
		if grace > emergencyShutdownGraceCap {
			grace = emergencyShutdownGraceCap
		}
	}

	scope := strings.TrimSpace(body.Scope)
	groupName := ""
	var targets []string
	switch scope {
	case "group":
		groupName = strings.TrimSpace(body.Group)
		if groupName == "" {
			http.Error(w, "scope \"group\" requires a non-empty \"group\" name", http.StatusBadRequest)
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
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ConvID)
		}
		targets = aliveConvIDs(ids)
	case "all":
		agents, err := db.ListActiveAgents()
		if err != nil {
			http.Error(w, "list agents: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ids := make([]string, 0, len(agents))
		for _, a := range agents {
			ids = append(ids, a.ConvID)
		}
		targets = aliveConvIDs(ids)
	default:
		http.Error(w, "invalid scope "+strconv.Quote(scope)+" (expected \"group\" or \"all\")", http.StatusBadRequest)
		return
	}

	outcomes := runEmergencyShutdown(targets, grace)
	// Deterministic order so the dashboard list (and the flow test)
	// don't depend on goroutine scheduling.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].ConvID < outcomes[j].ConvID })

	resp := emergencyShutdownResp{
		Scope:    scope,
		Group:    groupName,
		GraceMs:  grace.Milliseconds(),
		Targeted: len(outcomes),
		Agents:   outcomes,
	}
	for _, o := range outcomes {
		switch o.Outcome {
		case emShutdownExited:
			resp.ExitedGracefully++
		case emShutdownKilled:
			resp.ForceKilled++
		case emShutdownOffline:
			resp.AlreadyOffline++
		default:
			resp.Failed++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// aliveConvIDs filters convIDs down to the ones with a live tmux
// session right now, de-duplicating along the way. Collection-time
// liveness is only a snapshot — the escalation re-checks per agent,
// so a conv that dies between here and there still resolves cleanly
// (already_offline).
func aliveConvIDs(convIDs []string) []string {
	seen := make(map[string]bool, len(convIDs))
	out := []string{}
	for _, id := range convIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if pickAliveSession(id) != nil {
			out = append(out, id)
		}
	}
	return out
}

// runEmergencyShutdown escalates every target in parallel and returns
// one outcome per target, in input order. Each agent gets its own
// goroutine; the caller (the HTTP handler) blocks on the WaitGroup so
// the response carries the full summary. A hung agent only delays its
// own slot — never the others.
func runEmergencyShutdown(targets []string, grace time.Duration) []emergencyAgentOutcome {
	outcomes := make([]emergencyAgentOutcome, len(targets))
	var wg sync.WaitGroup
	for i, convID := range targets {
		wg.Add(1)
		go func(i int, convID string) {
			defer wg.Done()
			outcomes[i] = escalateShutdown(convID, grace)
		}(i, convID)
	}
	wg.Wait()
	return outcomes
}

// escalateShutdown runs the soft→grace→hard escalation for one agent:
//
//  1. Inject /exit (stopOneConv force=false) — the soft stop.
//  2. Poll the agent's tmux liveness for up to `grace`. An agent that
//     honours /exit within the window exits gracefully and is never
//     force-killed.
//  3. Still alive when the window closes (or the /exit injection
//     itself failed) → force-kill the tmux session (stopOneConv
//     force=true).
//
// It reuses stopOneConv unchanged, so it touches nothing but the tmux
// session — no DB row, no .jsonl. Idempotent: an already-offline conv
// comes back as already_offline.
func escalateShutdown(convID string, grace time.Duration) emergencyAgentOutcome {
	out := emergencyAgentOutcome{ConvID: convID, Title: agent.FreshTitle(convID)}

	// Step 1: soft exit — inject /exit, exactly as the per-agent
	// "soft exit" shutdown button does.
	soft := stopOneConv(convID, false)
	switch soft.Action {
	case "skipped:already_offline":
		// Raced — the agent exited between collection and now.
		out.Outcome = emShutdownOffline
		return out
	case "soft_stopped":
		// Step 2: give the agent the grace window to honour /exit. A
		// quick exiter is noticed on the first poll and never killed.
		if waitForConvOffline(convID, grace) {
			out.Outcome = emShutdownExited
			return out
		}
	default:
		// The /exit injection itself failed (send-keys error). Waiting
		// would accomplish nothing — escalate straight to a force-kill.
		out.Detail = "soft exit failed (" + soft.Detail + "); escalated to force-kill"
	}

	// Step 3: still alive (or the soft exit never landed) — force-kill.
	hard := stopOneConv(convID, true)
	switch hard.Action {
	case "killed":
		out.Outcome = emShutdownKilled
	case "skipped:already_offline":
		// It exited during or just after the grace window — the kill
		// was a no-op, so count it as a graceful exit.
		out.Outcome = emShutdownExited
	default:
		out.Outcome = emShutdownFailed
		detail := "force-kill failed: " + hard.Detail
		if out.Detail != "" {
			detail = out.Detail + "; " + detail
		}
		out.Detail = detail
	}
	return out
}

// waitForConvOffline polls convID's tmux liveness until it goes
// offline or the grace window closes. Returns true if the agent
// exited within the window. A grace of 0 means "check once, don't
// wait" — the escalation then proceeds straight to the force-kill.
func waitForConvOffline(convID string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if pickAliveSession(convID) == nil {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		sleep := emergencyPollInterval
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}
