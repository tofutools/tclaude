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

// The dashboard's power buttons — a matched pair of bulk controls,
// each behind the same cookie + Origin pin as every other /api
// mutation:
//
//   - Shutdown   (POST /api/shutdown)  — stop a scope of running
//     agents fast. Stop-only: no conversation, enrollment, group or
//     permission row is touched, so every shut-down agent is
//     reinstatable by simply resuming its session.
//   - Power On   (POST /api/power-on)  — the inverse: resume every
//     OFFLINE agent in scope. Resume-only: it reuses the per-agent
//     session-resume primitive (resumeOneConv) and starts nothing
//     that wasn't already a recorded conversation.
//
// Both take the same two scopes, picked by the request body:
//   - {"scope":"group","group":"<name>"} — the members of one group
//     (the same membership set `tclaude agent groups stop` / `resume`
//     walk; owner-only rows are not members and are left alone).
//   - {"scope":"all"} — every agent on the dashboard's active roster
//     (db.ListActiveAgents): grouped and ungrouped alike.
//     Retired/superseded convs are excluded by ListActiveAgents. The
//     request comes from the human's browser, so there is no "self"
//     to exclude.
//
// The two are mirror images at collection time: shutdown collects the
// ALIVE convs in scope (aliveConvIDs) and skips the offline ones;
// power-on collects the OFFLINE convs (offlineConvIDs) and skips the
// alive ones. An agent already in the desired state is never in the
// target list and so gets no outcome row.
//
// Per agent, shutdown escalates gracefully: inject /exit (soft), wait
// a grace window, and force-kill the tmux session only if the agent
// is still alive when the window closes. Every agent in scope is
// escalated in PARALLEL so one hung agent can't delay the rest.
// Power-on needs no grace/escalation — resume either starts a session
// or fails — so it runs a plain sequential loop, the same shape as
// the bulk groups.resume path (handleGroupResume).

// shutdownGrace is the soft→hard escalation window: how long an agent
// has to honour its injected /exit before it gets force-killed. A
// package var (not a const) so it stays overridable; the per-request
// `grace_ms` body field overrides it per call — the flow test passes
// a few milliseconds so it never sleeps for real seconds.
var shutdownGrace = 10 * time.Second

// shutdownPollInterval is the gap between liveness polls while waiting
// out the grace window. Small enough that a quick exiter is noticed
// promptly, large enough not to spin.
var shutdownPollInterval = 100 * time.Millisecond

// shutdownGraceCap bounds the per-request grace override so a browser
// bug (or a fat-fingered value) can't wedge the request for minutes.
const shutdownGraceCap = 2 * time.Minute

// Per-agent outcome strings for a shutdown. Stable wire values — the
// dashboard reads them back into the result toast.
const (
	shutdownExited  = "exited_gracefully" // honoured /exit within the grace window
	shutdownKilled  = "force_killed"      // still alive after grace → tmux kill-session
	shutdownOffline = "already_offline"   // raced — exited between collection and escalation
	shutdownFailed  = "failed"            // neither /exit nor kill-session succeeded
)

// Per-agent outcome strings for a power-on. Stable wire values.
const (
	powerOnResumed       = "resumed"        // a fresh tmux session was spawned for the conv
	powerOnAlreadyOnline = "already_online" // raced — came online between collection and resume
	powerOnFailed        = "failed"         // the resume spawn failed
)

// powerAgentOutcome is the per-agent result of one power op — shared
// by the shutdown and power-on responses; the Outcome string says
// which op (and which result) it carries.
type powerAgentOutcome struct {
	// AgentID is the powered agent's stable actor key — the canonical ID
	// the dashboard/CLI leads with; ConvID is the live generation behind
	// it (kept as the snapshot/hover). "" when the conv is not a known
	// agent.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title,omitempty"`
	Outcome string `json:"outcome"`          // shutdown: exited_gracefully|force_killed|already_offline|failed — power-on: resumed|already_online|failed
	Detail  string `json:"detail,omitempty"` // human-readable note (escalation reason / error)
}

// shutdownResp is the wire shape returned by /api/shutdown. Agents is
// always non-nil so the dashboard can iterate it unconditionally.
type shutdownResp struct {
	Scope            string              `json:"scope"`
	Group            string              `json:"group,omitempty"`
	GraceMs          int64               `json:"grace_ms"`
	Targeted         int                 `json:"targeted"`
	ExitedGracefully int                 `json:"exited_gracefully"`
	ForceKilled      int                 `json:"force_killed"`
	AlreadyOffline   int                 `json:"already_offline"`
	Failed           int                 `json:"failed"`
	Agents           []powerAgentOutcome `json:"agents"`
}

// powerOnResp is the wire shape returned by /api/power-on. Agents is
// always non-nil so the dashboard can iterate it unconditionally.
type powerOnResp struct {
	Scope         string              `json:"scope"`
	Group         string              `json:"group,omitempty"`
	Targeted      int                 `json:"targeted"`
	Resumed       int                 `json:"resumed"`
	AlreadyOnline int                 `json:"already_online"`
	Failed        int                 `json:"failed"`
	Agents        []powerAgentOutcome `json:"agents"`
}

// handleShutdown is the cookie-auth endpoint behind the dashboard's
// group-level and whole-dashboard Shutdown buttons. Body:
//
//	POST /api/shutdown
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
func handleShutdown(w http.ResponseWriter, r *http.Request) {
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
		GraceMs *int64 `json:"grace_ms"` // nil → shutdownGrace default
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve the grace window: the body override wins, clamped to
	// [0, cap]. 0 is legitimate ("no grace — escalate immediately").
	grace := shutdownGrace
	if body.GraceMs != nil {
		ms := *body.GraceMs
		if ms < 0 {
			ms = 0
		}
		grace = time.Duration(ms) * time.Millisecond
		if grace > shutdownGraceCap {
			grace = shutdownGraceCap
		}
	}

	scope := strings.TrimSpace(body.Scope)
	groupName, memberIDs, ok := resolvePowerScope(w, scope, body.Group)
	if !ok {
		return
	}
	targets := aliveConvIDs(memberIDs)

	outcomes := runShutdown(targets, grace)
	// Deterministic order so the dashboard list (and the flow test)
	// don't depend on goroutine scheduling.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].ConvID < outcomes[j].ConvID })

	resp := shutdownResp{
		Scope:    scope,
		Group:    groupName,
		GraceMs:  grace.Milliseconds(),
		Targeted: len(outcomes),
		Agents:   outcomes,
	}
	for _, o := range outcomes {
		switch o.Outcome {
		case shutdownExited:
			resp.ExitedGracefully++
		case shutdownKilled:
			resp.ForceKilled++
		case shutdownOffline:
			resp.AlreadyOffline++
		default:
			resp.Failed++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePowerOn is the cookie-auth endpoint behind the dashboard's
// group-level and whole-dashboard Power On buttons — the inverse of
// handleShutdown. Body:
//
//	POST /api/power-on
//	  {"scope":"group","group":"<name>"}   — one group's offline members
//	  {"scope":"all"}                      — every offline agent
//
// For each OFFLINE agent in scope it spawns a fresh detached tmux
// session for the conv (resumeOneConv — the same primitive the
// per-agent "wake" button and `tclaude agent groups resume` use).
// Agents already online are skipped at collection, mirroring how
// shutdown skips already-offline ones. Resume-only: nothing is
// created that wasn't already a recorded conversation, so the
// dashboard cookie + Origin pin is consent enough — no extra gating.
func handlePowerOn(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Scope string `json:"scope"`
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	scope := strings.TrimSpace(body.Scope)
	groupName, memberIDs, ok := resolvePowerScope(w, scope, body.Group)
	if !ok {
		return
	}
	targets := offlineConvIDs(memberIDs)

	outcomes := runPowerOn(targets)
	// Deterministic order so the dashboard list (and the flow test)
	// don't depend on map iteration.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].ConvID < outcomes[j].ConvID })

	resp := powerOnResp{
		Scope:    scope,
		Group:    groupName,
		Targeted: len(outcomes),
		Agents:   outcomes,
	}
	for _, o := range outcomes {
		switch o.Outcome {
		case powerOnResumed:
			resp.Resumed++
		case powerOnAlreadyOnline:
			resp.AlreadyOnline++
		default:
			resp.Failed++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolvePowerScope resolves a power-button scope — "group" (with a
// non-empty name) or "all" — to the set of member conv-ids in scope,
// and the canonical group name ("" for the "all" scope, echoed back
// in the response). It writes the HTTP error and returns ok=false on
// a bad scope, a missing group, or a DB error.
//
// Liveness filtering is the caller's job: shutdown narrows to the
// alive convs, power-on to the offline ones. Both ops resolve scope
// the same way, so this is the single place "what is in scope" is
// decided.
func resolvePowerScope(w http.ResponseWriter, scope, group string) (resolvedGroup string, convIDs []string, ok bool) {
	switch scope {
	case "group":
		groupName := strings.TrimSpace(group)
		if groupName == "" {
			http.Error(w, "scope \"group\" requires a non-empty \"group\" name", http.StatusBadRequest)
			return "", nil, false
		}
		g, err := db.GetAgentGroupByName(groupName)
		if err != nil {
			http.Error(w, "group lookup: "+err.Error(), http.StatusInternalServerError)
			return "", nil, false
		}
		if g == nil {
			http.Error(w, "no such group "+groupName, http.StatusNotFound)
			return "", nil, false
		}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			http.Error(w, "list members: "+err.Error(), http.StatusInternalServerError)
			return "", nil, false
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ConvID)
		}
		return groupName, ids, true
	case "all":
		agents, err := db.ListActiveAgents()
		if err != nil {
			http.Error(w, "list agents: "+err.Error(), http.StatusInternalServerError)
			return "", nil, false
		}
		ids := make([]string, 0, len(agents))
		for _, a := range agents {
			ids = append(ids, a.CurrentConvID)
		}
		return "", ids, true
	default:
		http.Error(w, "invalid scope "+strconv.Quote(scope)+" (expected \"group\" or \"all\")", http.StatusBadRequest)
		return "", nil, false
	}
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

// offlineConvIDs is the mirror of aliveConvIDs: it filters convIDs
// down to the ones with NO live tmux session, de-duplicating along
// the way. Empty conv-ids — placeholder members with no conversation
// yet — are dropped here: they have no .jsonl to resume from.
// Collection-time liveness is a snapshot; resumeOneConv re-checks per
// agent, so a conv that comes online between here and there still
// resolves cleanly (already_online).
func offlineConvIDs(convIDs []string) []string {
	seen := make(map[string]bool, len(convIDs))
	out := []string{}
	for _, id := range convIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if pickAliveSession(id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// runShutdown escalates every target in parallel and returns one
// outcome per target, in input order. Each agent gets its own
// goroutine; the caller (the HTTP handler) blocks on the WaitGroup so
// the response carries the full summary. A hung agent only delays its
// own slot — never the others.
func runShutdown(targets []string, grace time.Duration) []powerAgentOutcome {
	outcomes := make([]powerAgentOutcome, len(targets))
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

// runPowerOn resumes every offline target and returns one outcome per
// target, in input order. resumeOneConv only spawns a detached
// subprocess (no blocking wait), so a plain sequential loop is enough
// — the same shape as the bulk groups.resume path. There is no grace
// window to parallelise around the way runShutdown has.
func runPowerOn(targets []string) []powerAgentOutcome {
	outcomes := make([]powerAgentOutcome, 0, len(targets))
	for _, convID := range targets {
		out := powerAgentOutcome{AgentID: peerAgentID(convID), ConvID: convID, Title: agent.FreshTitle(convID)}
		res := resumeOneConv(convID)
		switch res.Action {
		case "resumed":
			out.Outcome = powerOnResumed
		case "skipped:already_online":
			// Raced — the agent came online between collection and now.
			out.Outcome = powerOnAlreadyOnline
		default:
			// "error" — the resume spawn failed — or "error:missing_cwd", the
			// agent's recorded launch dir was deleted (Detail carries the path).
			// A bulk power-on can't recreate dirs interactively, so it surfaces
			// the failure; the human recreates via `agent resume --recreate-dir`
			// or the dashboard wake confirm. ("skipped:no_conv_id" can't reach
			// here: offlineConvIDs already drops empty ids.)
			out.Outcome = powerOnFailed
			out.Detail = res.Detail
			if out.Detail == "" {
				out.Detail = "resume failed (" + res.Action + ")"
			}
		}
		outcomes = append(outcomes, out)
	}
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
func escalateShutdown(convID string, grace time.Duration) powerAgentOutcome {
	out := powerAgentOutcome{AgentID: peerAgentID(convID), ConvID: convID, Title: agent.FreshTitle(convID)}

	// Step 1: soft exit — inject /exit, exactly as the per-agent
	// "soft exit" shutdown button does.
	soft := stopOneConv(convID, false)
	switch soft.Action {
	case "skipped:already_offline":
		// Raced — the agent exited between collection and now.
		out.Outcome = shutdownOffline
		return out
	case "soft_stopped":
		// Step 2: give the agent the grace window to honour /exit. A
		// quick exiter is noticed on the first poll and never killed.
		if waitForConvOffline(convID, grace) {
			out.Outcome = shutdownExited
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
		out.Outcome = shutdownKilled
	case "skipped:already_offline":
		// It exited during or just after the grace window — the kill
		// was a no-op, so count it as a graceful exit.
		out.Outcome = shutdownExited
	default:
		out.Outcome = shutdownFailed
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
		sleep := shutdownPollInterval
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}
