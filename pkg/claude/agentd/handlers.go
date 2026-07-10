package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- /v1/info ---

// handleInfo returns daemon-wide constants the CLI needs to discover
// at runtime — currently just the popup base URL so `tclaude agent
// dashboard` can open it without hard-coding the random port.
//
// Open to anyone: no identity required, no permission gate. Loopback
// URLs aren't sensitive on their own; the auth-gated endpoints
// (popup approve, dashboard /api) sit behind cookies.
func handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"popup_base_url": popupBaseURL,
	})
}

// --- /v1/whoami ---

type whoamiResp struct {
	IsHuman bool     `json:"is_human"`
	AgentID string   `json:"agent_id,omitempty"` // stable actor key — the canonical identity
	ConvID  string   `json:"conv_id,omitempty"`  // live generation behind it (rotates)
	Title   string   `json:"title,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	// Phases lists the advisory process phase (JOH-242) of each group the
	// caller is in that HAS a process — one "<group>: phase <n>/<m>: <name>"
	// line per such group. Omitted when no group the caller is in has a
	// process (absence = feature off, degrade everywhere).
	Phases []string `json:"phases,omitempty"`
}

func handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		writeJSON(w, http.StatusOK, whoamiResp{IsHuman: true})
		return
	case classAgent:
		// fall through and report the agent's conv-id
	default:
		// Neither a confirmed agent nor the human (unidentified /
		// unconfirmed / unidentifiable-agent). Report honestly rather
		// than the old fail-open is_human:true.
		writeJSON(w, http.StatusOK, whoamiResp{})
		return
	}
	// Resolve the agent's name through FreshTitle so it picks up the
	// spawn-time pending_name (agents.pending_name) when no custom title has
	// landed yet — the same priority (custom → pending → summary → first
	// prompt) the dashboard and conv-listing surfaces use. A bare
	// DisplayTitle would skip the pending name, which is the bug for a
	// freshly-spawned Codex agent (JOH-219): Codex persists its title
	// out-of-band AFTER the welcome is injected (JOH-216), so at the
	// moment the agent runs `whoami` there is no custom title in
	// conv_index yet and it would read itself as "(unnamed)" — and
	// self-describe that way — despite the welcome having named it.
	// Claude Code is unaffected (its /rename writes the .jsonl before the
	// welcome). UnknownTitle maps back to the "(unnamed)" placeholder
	// whoami has always shown.
	title := agent.FreshTitle(p.ConvID)
	if title == agent.UnknownTitle {
		title = "(unnamed)"
	}
	groups, _ := db.ListGroupsForConv(p.ConvID)
	gs := make([]string, 0, len(groups))
	var phases []string
	for _, g := range groups {
		gs = append(gs, g.Name)
		// Advisory process (JOH-242): surface the group's current phase when it
		// has a process. Best-effort — a lookup error just omits the line.
		if st, err := db.GetGroupProcessState(g.ID); err == nil && st != nil {
			phases = append(phases, g.Name+": "+phaseLabel(st))
		}
	}
	agentID, _ := db.AgentIDForConv(p.ConvID)
	writeJSON(w, http.StatusOK, whoamiResp{AgentID: agentID, ConvID: p.ConvID, Title: title, Groups: gs, Phases: phases})
}

// --- /v1/lookup ---

func handleLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// Open to humans as well as agents — selector resolution is a
	// read-only conv_index lookup with no PII unique to one caller,
	// and the CLI's `agent delete` uses this to preview matches
	// before prompting the human for confirmation.
	selector := r.URL.Query().Get("selector")
	if selector == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing selector")
		return
	}
	res, matches, err := agent.ResolveSelector(selector)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "selector matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	agentID, _ := db.AgentIDForConv(res.ConvID)
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID, "agent_id": agentID})
}

// --- /v1/peers ---

type peerEntry struct {
	// AgentID is the stable, rotation-immune actor key — the canonical
	// way to reference an agent. The agent CLI leads with it; ConvID is
	// the live generation behind it (which rotates on reincarnate/clone).
	// Empty only for a resolved candidate that isn't an agent. (The
	// dashboard still keys on conv_id — a separate follow-up.)
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Role    string `json:"role,omitempty"`
	Descr   string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
}

// handlePeers returns the conversations the caller can see.
//
// Two passes:
//
//  1. **Group members.** Agent caller → members of every group the
//     caller is in. Human caller → members of every known group
//     (humans aren't scoped by group membership — they see the full
//     picture and can reach anyone).
//  2. **Ungrouped agents.** Every active enrolled agent not already
//     surfaced by pass 1, online or offline. Caller (when known) is
//     excluded. Being an agent is an explicit, durable fact now, so
//     `tclaude agent ls` keeps showing an agent after its tmux pane
//     closes instead of dropping it the moment it goes offline.
//     Retired agents are excluded — they are no longer agents.
func handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// The human operator sees every group; an agent is scoped to its
	// own; unidentified / unconfirmed callers are refused fail-closed.
	myID, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}

	var groups []*db.AgentGroup
	var err error
	if isHuman {
		groups, err = db.ListAgentGroups()
	} else {
		groups, err = db.ListGroupsForConv(myID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// One tmux ls for the whole listing — every isConvOnlineIn below
	// is a map lookup against this snapshot, not a per-row subprocess.
	aliveSessions, _ := session.LiveTmuxSessions()

	byConv := map[string]*peerEntry{}
	// Pass 1: group members.
	for _, g := range groups {
		members, _ := db.ListAgentGroupMembers(g.ID)
		for _, m := range members {
			if m.ConvID == myID {
				continue
			}
			pe, exists := byConv[m.ConvID]
			if !exists {
				// FreshTitle / FreshBranch refresh the conv_index row
				// from the .jsonl first, so a renamed / freshly-spawned
				// member shows its real name and branch instead of stale
				// values.
				pe = &peerEntry{
					AgentID:           peerAgentID(m.ConvID),
					ConvID:            m.ConvID,
					Title:             agent.FreshTitle(m.ConvID),
					Role:              m.Role,
					Descr:             m.Descr,
					agentLocationView: locationView(m.ConvID),
					Online:            isConvOnlineIn(m.ConvID, aliveSessions),
				}
				byConv[m.ConvID] = pe
			}
			pe.Groups = append(pe.Groups, g.Name)
		}
	}
	// Pass 2: active enrolled agents that belong to NO group, online or
	// offline. Switched from "online sessions" to the enrollment roster
	// so an agent that is offline (tmux closed) still shows up —
	// agent-ness is an explicit, durable fact now, not a function of
	// whether a pane happens to be alive.
	//
	// Only UNGROUPED agents are surfaced here: a grouped agent is
	// either already in byConv (a group the caller shares, via pass 1)
	// or in a group the caller cannot see — and in the latter case it
	// must stay hidden, preserving the group-scoping an agent caller
	// relies on. ListActiveAgents excludes retired agents.
	if active, err := db.ListActiveAgents(); err == nil {
		for _, e := range active {
			conv := e.CurrentConvID
			if conv == "" || conv == myID {
				continue
			}
			if _, exists := byConv[conv]; exists {
				continue
			}
			if groups, gerr := db.ListGroupsForConv(conv); gerr != nil || len(groups) > 0 {
				continue
			}
			byConv[conv] = &peerEntry{
				AgentID:           e.AgentID,
				ConvID:            conv,
				Title:             agent.FreshTitle(conv),
				agentLocationView: locationView(conv),
				Online:            isConvOnlineIn(conv, aliveSessions),
			}
		}
	}
	out := make([]*peerEntry, 0, len(byConv))
	for _, pe := range byConv {
		out = append(out, pe)
	}
	writeJSON(w, http.StatusOK, out)
}

func peerEntriesFromResolved(rs []*agent.Resolved) []*peerEntry {
	out := make([]*peerEntry, 0, len(rs))
	for _, r := range rs {
		title := ""
		if r.Row != nil {
			title = agent.DisplayTitle(r.Row)
		}
		out = append(out, &peerEntry{
			// agent.ResolveSelector already stamped the stable agent_id on
			// every candidate, so read it off the resolved handle instead of
			// a second db.AgentIDForConv lookup.
			AgentID:           r.AgentID,
			ConvID:            r.ConvID,
			Title:             title,
			agentLocationView: locationView(r.ConvID),
		})
	}
	return out
}

// peerAgentID resolves a conv to its stable agent_id for display, or ""
// when the conv is not (yet) an agent. A miss is non-fatal — the row just
// shows no stable id.
func peerAgentID(conv string) string {
	id, _ := db.AgentIDForConv(conv)
	return id
}

// sameActor reports whether two conv-ids belong to the same stable agent.
// It is the rotation-immune form of `a == b` for actor-equality self-checks
// (JOH-323): each conv resolves to its agent_id and the agents are compared,
// so two generations of one agent (post-reincarnate / post-/clear) count as
// equal — the old conv-literal compare only matched the exact generation.
//
// An empty conv-id is "no actor" and is never the same actor as anything,
// including another empty — so it is fail-closed (returns false) regardless
// of the a == b short-circuit below. Callers already guard their empty cases
// (a human caller is ""; a placeholder member's conv is ""), but failing
// closed here keeps the primitive safe for any future caller.
//
// Semantics are otherwise preserved for the non-agent cases the old compare
// handled: the `a == b` short-circuit keeps the common self case cheap (no DB
// hit) and still matches two identical convs; and a conv with no actor row (a
// plain conversation) resolves to "" and is matched by conv-id alone, so two
// distinct non-agent convs never collide, and a non-agent conv never matches
// an agent. Mirrors the JOH-317 inbox/ownership matcher (callerIsRecipient).
func sameActor(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	aa := peerAgentID(a)
	return aa != "" && aa == peerAgentID(b)
}

// stampCallerAgentID adds the durable `caller_agent_id` companion to a
// response that already carries a `caller_conv` snapshot. The conv-id names
// only one generation, so after the caller reincarnates the snapshot points
// at a dead generation; the agent_id is the stable actor the attribution
// should lead with (JOH-329). No-op when the caller isn't an enrolled agent
// (older / un-enrolled callers keep only the conv snapshot, and the CLI falls
// back to it).
func stampCallerAgentID(resp map[string]any, caller string) {
	if aid := peerAgentID(caller); aid != "" {
		resp["caller_agent_id"] = aid
	}
}

// The message inbox/ownership surface keys on the STABLE actor, not the
// caller's current conv generation (JOH-317). A conv-id only names one
// generation, so an agent that reincarnated / ran /clear could neither read
// nor manage mail it received under a predecessor conv. These helpers resolve
// the caller's conv to its agent and address the message by current conv OR
// stable actor: the actor term spans generations; the conv term keeps a row
// whose agent companion is '' (a conv messaged before it enrolled) visible
// from its own current inbox. A conv with no actor row (a plain conversation
// that messaged directly) resolves its agent to "" and is matched by conv
// alone, so non-agent senders are unaffected. The delivery/flush queue is
// already actor-keyed the same way (JOH-310).

// inboxForCaller returns the caller's received mail across all of the actor's
// generations (plus its own current-conv mail).
func inboxForCaller(callerConv string, limit int) ([]*db.AgentMessage, error) {
	return db.ListInboxForActor(callerConv, peerAgentID(callerConv), limit)
}

// outboxForCaller is the sent-mail twin of inboxForCaller.
func outboxForCaller(callerConv string, limit int) ([]*db.AgentMessage, error) {
	return db.ListOutboxForActor(callerConv, peerAgentID(callerConv), limit)
}

// callerIsRecipient reports whether callerConv is the recipient of m (the
// read / reply gates: being the sender does not grant access). It matches by
// current conv OR stable actor — so any generation of the actor counts
// (JOH-317) — the rotation-immune form of the old `m.ToConv == callerConv`
// check.
func callerIsRecipient(callerConv string, m *db.AgentMessage) bool {
	if m.ToConv == callerConv {
		return true
	}
	if a := peerAgentID(callerConv); a != "" {
		return m.ToAgent == a
	}
	return false
}

// deleteMessageForCaller deletes message id when callerConv is a party,
// matching by current conv OR stable actor so it spans the actor's
// generations (JOH-317).
func deleteMessageForCaller(id int64, callerConv string) (bool, error) {
	return db.DeleteAgentMessageForActor(id, callerConv, peerAgentID(callerConv))
}

// pruneMessagesForCaller prunes the caller's mail by current conv OR stable
// actor, so it spans the actor's generations (JOH-317).
func pruneMessagesForCaller(callerConv string, olderThan time.Time, readOnly bool) (int64, error) {
	return db.PruneAgentMessagesForActor(callerConv, peerAgentID(callerConv), olderThan, readOnly)
}

// --- /v1/messages (POST), /v1/messages/{id} (GET) ---

type sendReq struct {
	To string `json:"to"`
	// Gen optionally pins delivery to a SPECIFIC previous generation of the
	// target agent (JOH-310 prev-gen targeting): a conv-id that must belong
	// to the same agent `To` resolves to. When set, the message is addressed
	// to that exact conv (pin_gen=1) instead of following the agent to its
	// current head generation — the deliberate, explicit way to reach a past
	// generation, never inferred from a bare conv-id in `To` (which the
	// resolver auto-redirects forward to the head). Direct (1:1) sends only;
	// an error with a group:/--cc target.
	Gen     string   `json:"gen,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Body    string   `json:"body"`
	// Role, when non-empty, restricts a multicast (To prefixed with
	// "group:") to members whose agent_group_members.role matches it
	// case-insensitively. It is an error on a 1:1 (non-group:) target.
	Role string `json:"role,omitempty"`
	// Members, when non-empty on a "group:" multicast, narrows the
	// fan-out to the listed members — addressed by stable agent_id (the
	// canonical key, JOH-27) or conv-id (back-compat). The dashboard's
	// group-scoped message modal sets it when the human ticks a subset
	// of a group's members. Like Role it is applied AFTER the live
	// roster is read, so it can only shrink the recipient set, never
	// widen it: a listed id that is not a current member of the target
	// group simply matches nothing. Empty → the multicast reaches every
	// member. It is an error on a 1:1 (non-group:) target.
	//
	// As part of the shared sendReq it is decoded on BOTH front doors —
	// the dashboard's POST /api/message and the agent-facing POST
	// /v1/messages — even though the `tclaude agent message` CLI
	// exposes no flag for it today. That is safe: handleMulticast still
	// gates the sender on group membership/ownership, and Members can
	// only shrink reach, so an agent gains no authority it lacked.
	Members []string `json:"members,omitempty"`
}

// sendResp carries the result of either a direct send or a group
// fan-out. For direct messages the top-level fields (ID, Delivered)
// are populated and Recipients is nil. For multicast (To prefixed
// with "group:") ID/Delivered are zero values and Recipients lists
// one entry per non-sender member.
type sendResp struct {
	ID int64 `json:"id,omitempty"`
	// Queued is true once the message row is persisted and handed to the
	// async per-agent delivery queue (JOH-310). Under the async model the
	// send call returns immediately, BEFORE the worker decides
	// delivered/held, so Queued — not Delivered/Held — is the success
	// signal on a direct send. Pending carries the queue depth.
	Queued bool `json:"queued,omitempty"`
	// Pending is the recipient's queue depth at send time — how many
	// undelivered nudges are now waiting for them, including this one. It is
	// the "queue length" the sender CLI surfaces. 0 is a valid value (e.g. a
	// non-actor target whose count we don't track), so it is informational
	// only, never a failure signal.
	Pending int `json:"pending,omitempty"`
	// Delivered/Held are retained for response back-compat but are no longer
	// set on the async send path (delivery + the JOH-308 human-input hold are
	// decided later by the worker, not at send time). They stay populated
	// only where a synchronous verdict still exists.
	Delivered bool `json:"delivered,omitempty"`
	// Held is true when the recipient has an alive pane but is currently
	// blocked on a human (awaiting_input / awaiting_permission). The
	// message is in their mailbox but deliberately NOT nudged in — a nudge
	// now could be captured by the open dialog as the human's answer. It is
	// delivered once the recipient is back to working/idle. Mutually
	// exclusive with Delivered. Only meaningful on a direct (non-group)
	// send; multicast / --cc carry per-recipient Held on each entry.
	Held       bool        `json:"held,omitempty"`
	ViaGroup   string      `json:"via_group"`
	Recipients []recipient `json:"recipients,omitempty"`
	// RedirectedFrom is non-empty when the addressed conv-id has been
	// superseded and the daemon re-routed to its live successor. The
	// sender CLI uses this to print a `→ delivered to <new> (you
	// addressed <old>, superseded)` notice. Only populated on direct
	// sends; per-recipient redirects on multicast / multi-recipient
	// surface in the per-row recipient struct.
	RedirectedFrom string `json:"redirected_from,omitempty"`
}

type recipient struct {
	ConvID string `json:"conv_id"`
	// AgentID is the recipient's stable agent_id (JOH-27 PR3b-2), resolved
	// from ConvID at send time so the sender's receipt names each recipient
	// by the rotation-immune handle instead of a conv-id prefix. Empty when
	// the recipient is not (yet) an enrolled agent — the receipt then falls
	// back to the conv-id.
	AgentID   string `json:"agent_id,omitempty"`
	Title     string `json:"title,omitempty"`
	MessageID int64  `json:"message_id"`
	// Queued is true once this recipient's row is persisted and handed to the
	// async delivery queue (JOH-310) — the per-recipient analogue of
	// sendResp.Queued. Pending is this recipient's queue depth.
	Queued  bool `json:"queued,omitempty"`
	Pending int  `json:"pending,omitempty"`
	// Delivered/Held are retained for back-compat but no longer set on the
	// async path (the worker decides them after the send returns).
	Delivered bool `json:"delivered"`
	// Held mirrors sendResp.Held per-recipient: the recipient is alive but
	// blocked on a human, so their copy is in the mailbox, not yet nudged.
	Held bool `json:"held,omitempty"`
	// RedirectedFrom mirrors sendResp.RedirectedFrom on a per-recipient
	// basis: when the entry's ConvID is the live successor of a
	// superseded id the sender originally addressed, the original id
	// goes here. Empty when the address was already canonical.
	RedirectedFrom string `json:"redirected_from,omitempty"`
}

// multicastPrefix marks a multicast target. `to: "group:reviewer-team"`
// fans out to every member of that group except the sender.
const multicastPrefix = "group:"

// holdsPermission reports whether an agent conv holds permission slug
// through any non-interactive source: an active sudo grant, a per-conv
// grant override, or the config default-permissions list. A per-conv
// deny override reads as false. It is the boolean twin of
// requirePermission minus the X-Tclaude-Ask-Human popup — callers that
// only need the verdict (not the interactive escalation) use this.
func holdsPermission(convID, slug string) bool {
	return resolvePermission(convID, slug) == permAllow
}

// resolveMessageRouting authorises a 1:1 send fromID→targetID and
// returns the group_id to stamp on the agent_messages row. Two
// outcomes:
//
//   - A group-policy path exists (db.CanSenderReachTarget: shared
//     group, owner-of-group, or via-link) → the message routes through
//     that group; groupID is its id, viaName its name. This is the
//     default intra-group policy and needs no permission slug.
//   - No group-policy path → the send is "off-group" (to an ungrouped
//     agent, or across a group boundary) and requires the elevated
//     message.direct slug. When the sender holds it, the send is
//     allowed as a direct message: groupID 0, viaName "".
//
// On denial — no path and no slug, or the routing group is archived —
// the error response is already written and ok is false.
func resolveMessageRouting(w http.ResponseWriter, fromID, targetID string) (groupID int64, viaName string, ok bool) {
	via, _, err := db.CanSenderReachTarget(fromID, targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return 0, "", false
	}
	if via != nil {
		if !requireGroupActive(w, via) {
			return 0, "", false
		}
		return via.ID, via.Name, true
	}
	if !holdsPermission(fromID, PermMessageDirect) {
		writeError(w, http.StatusForbidden, "auth",
			fmt.Sprintf("no shared group with %s and you do not own a group containing it; "+
				"messaging an agent outside your group requires the %q permission "+
				"(ask the human to grant it, or get a time-bounded grant via `tclaude agent sudo`)",
				short8(targetID), PermMessageDirect))
		return 0, "", false
	}
	return 0, "", true
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	fromID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	dispatchSend(w, fromID, &req)
}

// dispatchSend validates and routes a send from fromID. It is the
// shared core behind two callers: POST /v1/messages (agent sender —
// identity is the connecting socket peer) and the dashboard's POST
// /api/message (human sender — identity is the From conv the human
// picked). Every authority check the send must clear lives below
// this point — the group member/owner gate inside handleMulticast,
// the shared-group / message.direct gate inside resolveMessageRouting
// — so neither caller can route around the gate.
func dispatchSend(w http.ResponseWriter, fromID string, req *sendReq) {
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is empty")
		return
	}
	// --role only filters a group: multicast's recipient set. On a 1:1
	// target there is no member set to filter, so the flag is
	// meaningless and almost certainly a mistake — reject it loudly.
	if strings.TrimSpace(req.Role) != "" && !strings.HasPrefix(req.To, multicastPrefix) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"--role is only valid with a 'group:' multicast target")
		return
	}
	// Members, like Role, only narrows a group: multicast's recipient
	// set. On a 1:1 target there is no roster to narrow, so the field
	// is meaningless and almost certainly a mistake — reject it loudly.
	if len(req.Members) > 0 && !strings.HasPrefix(req.To, multicastPrefix) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"members is only valid with a 'group:' multicast target")
		return
	}
	// gen pins a SPECIFIC generation of ONE agent; it is meaningless for a
	// group: fan-out or a --cc multi-send, where "the target" isn't a single
	// agent. Reject loudly before routing.
	if strings.TrimSpace(req.Gen) != "" && (strings.HasPrefix(req.To, multicastPrefix) || len(req.Cc) > 0) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"gen is only valid on a direct (non-group, non-cc) send")
		return
	}
	if strings.HasPrefix(req.To, multicastPrefix) {
		handleMulticast(w, fromID, req)
		return
	}
	target, matches, err := agent.ResolveSelector(req.To)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "target matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	// Succession-aware routing: ResolveSelector already auto-redirects
	// internally for known indexed convs (and the new succession-chain
	// fallback in tryResolve), so target.ConvID is the head of any
	// chain that walks. We just need to detect *whether* a redirect
	// happened, so the recipient can see Original-To: in their inbox
	// and the sender gets a redirect notice. Compare the raw input
	// string (after trim) to target.ConvID — when they differ AND the
	// input walks to target.ConvID via the chain, the input was a
	// superseded conv-id and the resolver redirected it. Title / prefix
	// inputs naturally skip this branch (they don't have chain rows
	// keyed on the literal title text).
	// headConv is the agent's live head generation (the resolver already
	// redirected a superseded input to it). Authorisation routes against the
	// head — reachability is an agent-level property — even when `gen` repoints
	// delivery to a past generation below.
	headConv := target.ConvID
	finalConv := headConv
	originalTo := ""
	rawInput := strings.TrimSpace(req.To)
	if rawInput != "" && rawInput != finalConv && db.ResolveLatestConv(rawInput) == finalConv {
		originalTo = rawInput
	}
	// Prev-gen targeting (JOH-310): an explicit `gen` overrides the
	// head-following default, pinning delivery to a SPECIFIC past generation
	// of the SAME agent `To` resolved to. It must be a conv of that agent —
	// validated here so a caller can't smuggle a cross-agent conv past the
	// agent-keyed routing — and it is the deliberate opt-out from the
	// resolver's auto-redirect-to-head, so no Original-To attribution.
	pinGen := false
	if genTrim := strings.TrimSpace(req.Gen); genTrim != "" {
		targetAgent, _ := db.AgentIDForConv(headConv)
		genAgent, _ := db.AgentIDForConv(genTrim)
		if targetAgent == "" || genAgent == "" || genAgent != targetAgent {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("gen %q is not a generation of the target agent", genTrim))
			return
		}
		finalConv = genTrim
		originalTo = ""
		pinGen = true
	}
	if finalConv == fromID {
		writeError(w, http.StatusBadRequest, "invalid_arg", "cannot message self")
		return
	}
	// Authorisation + routing. The default policy is intra-group: a
	// shared group (or owner-of-group / via-link) routes the message
	// through that group. Off-group sends — to an ungrouped agent, or
	// across a group boundary — require the elevated message.direct
	// slug and route as direct messages (group_id 0). Authority is
	// checked against the LIVE successor (headConv): the outdated id may
	// have lost membership by the time the successor took over, but the
	// successor is who actually receives the message. Routing against the
	// head (not a `gen`-pinned past conv) keeps the owner-of-group /
	// via-link reach paths — which compare the member's head conv — correct.
	groupID, viaName, ok := resolveMessageRouting(w, fromID, headConv)
	if !ok {
		return
	}

	// Multi-recipient (--cc) path: one row per (To + each CC), each with
	// the same to_recipients / cc_recipients audience. CCs that resolve
	// ambiguously / not at all / aren't reachable surface as a 4xx so
	// the sender can fix the typo before any rows are written.
	if len(req.Cc) > 0 {
		handleMultiRecipient(w, fromID, finalConv, originalTo, groupID, viaName, req)
		return
	}

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:        groupID,
		FromConv:       fromID,
		ToConv:         finalConv,
		OriginalToConv: originalTo,
		Subject:        req.Subject,
		Body:           req.Body,
		// Even single-recipient sends record the audience arrays now
		// so the recipient's `inbox read` can render a consistent
		// "To: ..." header. CC stays empty.
		ToRecipients: []string{finalConv},
		PinGen:       pinGen,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Async delivery (JOH-310): hand the recipient to the per-agent queue and
	// return immediately with the queue depth. The sender never blocks on the
	// tmux nudge; the worker decides delivered/held later.
	enqueueDeliveryForConv(finalConv)
	writeJSON(w, http.StatusOK, sendResp{
		ID:             id,
		Queued:         true,
		Pending:        queueDepthFor(finalConv, pinGen),
		ViaGroup:       viaName,
		RedirectedFrom: originalTo,
	})
}

// walkSuccession returns the live successor of convID and the
// original id when a redirect happened. When the chain has no
// successor, finalConv == convID and originalTo == "" — callers can
// rely on the empty originalTo to skip the redirect-rendering paths
// without comparing strings.
func walkSuccession(convID string) (finalConv, originalTo string) {
	if convID == "" {
		return convID, ""
	}
	latest := db.ResolveLatestConv(convID)
	if latest == convID {
		return convID, ""
	}
	return latest, convID
}

// handleMultiRecipient writes one row per (primary + each CC) of a
// `--cc`-flagged send, where every row carries the same to_recipients
// / cc_recipients arrays so each receiver's `inbox read` sees the full
// audience. The primary's routing has already been resolved by the
// caller (primaryGroupID / primaryViaName — 0 / "" for an off-group
// direct send); each CC is independently resolved and authorised via
// resolveMessageRouting, so a CC may route through its own group or
// off-group as a direct message, just like the primary.
//
// Pre-validation: if any CC fails (ambiguous, unknown, unreachable,
// duplicate of self/primary), the whole send is rejected before any
// rows are written. Half-broadcasts are confusing for the recipient
// who notices an extra "CC: <missing>" entry that wasn't actually
// delivered.
func handleMultiRecipient(w http.ResponseWriter, fromID, primaryConv, primaryOriginalTo string, primaryGroupID int64, primaryViaName string, req *sendReq) {
	type resolvedCC struct {
		ConvID         string
		OriginalToConv string
		Title          string
		GroupID        int64
	}
	resolved := make([]resolvedCC, 0, len(req.Cc))
	seen := map[string]bool{primaryConv: true, fromID: true}
	for _, sel := range req.Cc {
		sel = strings.TrimSpace(sel)
		if sel == "" {
			continue
		}
		t, matches, err := agent.ResolveSelector(sel)
		if errors.Is(err, agent.ErrAmbiguous) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      fmt.Sprintf("CC selector %q matches multiple conversations", sel),
				"code":       "ambiguous",
				"candidates": peerEntriesFromResolved(matches),
			})
			return
		}
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("CC %q: %v", sel, err))
			return
		}
		// Detect succession redirect on each CC so the per-row
		// original_to_conv reflects what the sender actually typed.
		// ResolveSelector already auto-redirected, so t.ConvID is
		// the head; we compare to the raw selector string to attribute
		// the original. Same shape as the primary path.
		ccConv := t.ConvID
		ccOriginal := ""
		ccRaw := strings.TrimSpace(sel)
		if ccRaw != "" && ccRaw != ccConv && db.ResolveLatestConv(ccRaw) == ccConv {
			ccOriginal = ccRaw
		}
		if seen[ccConv] {
			// Duplicate (CC == To, CC == self, CC repeated, OR a CC
			// that happens to redirect onto the primary's successor).
			// Skip silently — the sender's intent is "include this conv
			// once" either way.
			continue
		}
		seen[ccConv] = true
		ccGroupID, _, ok := resolveMessageRouting(w, fromID, ccConv)
		if !ok {
			// resolveMessageRouting already wrote the 4xx — no
			// group-policy path and no message.direct slug, or an
			// archived routing group. Pre-validation: abort the whole
			// send before any rows are written.
			return
		}
		title := agent.TitleFor(ccConv)
		resolved = append(resolved, resolvedCC{ConvID: ccConv, OriginalToConv: ccOriginal, Title: title, GroupID: ccGroupID})
	}

	toRecipients := []string{primaryConv}
	ccRecipients := make([]string, 0, len(resolved))
	for _, r := range resolved {
		ccRecipients = append(ccRecipients, r.ConvID)
	}

	out := sendResp{ViaGroup: primaryViaName, Recipients: []recipient{}}

	// Insert + nudge primary first so the response order matches the
	// "To:, CC: ..." header order in inbox read.
	primaryTitle := agent.TitleFor(primaryConv)
	primaryID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:        primaryGroupID,
		FromConv:       fromID,
		ToConv:         primaryConv,
		OriginalToConv: primaryOriginalTo,
		Subject:        req.Subject,
		Body:           req.Body,
		ToRecipients:   toRecipients,
		CcRecipients:   ccRecipients,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	enqueueDeliveryForConv(primaryConv)
	out.Recipients = append(out.Recipients, recipient{
		ConvID:         primaryConv,
		AgentID:        peerAgentID(primaryConv),
		Title:          primaryTitle,
		MessageID:      primaryID,
		Queued:         true,
		Pending:        queueDepthFor(primaryConv, false),
		RedirectedFrom: primaryOriginalTo,
	})

	for _, r := range resolved {
		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:        r.GroupID,
			FromConv:       fromID,
			ToConv:         r.ConvID,
			OriginalToConv: r.OriginalToConv,
			Subject:        req.Subject,
			Body:           req.Body,
			ToRecipients:   toRecipients,
			CcRecipients:   ccRecipients,
		})
		if err != nil {
			// Don't abort: the primary already landed. Surface the per-CC
			// failure so the sender can retry just that one.
			slog.Warn("multi-recipient: CC insert failed",
				"to", r.ConvID, "error", err)
			out.Recipients = append(out.Recipients, recipient{
				ConvID:    r.ConvID,
				AgentID:   peerAgentID(r.ConvID),
				Title:     r.Title,
				MessageID: 0,
				Delivered: false,
			})
			continue
		}
		enqueueDeliveryForConv(r.ConvID)
		out.Recipients = append(out.Recipients, recipient{
			ConvID:         r.ConvID,
			AgentID:        peerAgentID(r.ConvID),
			Title:          r.Title,
			MessageID:      id,
			Queued:         true,
			Pending:        queueDepthFor(r.ConvID, false),
			RedirectedFrom: r.OriginalToConv,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveMulticastGroup resolves the token after the "group:" prefix
// to a concrete group. The grammar:
//
//   - empty token        → the sender's own group (resolveOwnGroup).
//   - matches a name     → that group (the long-standing behaviour).
//   - all-digits, no name match → the group with that numeric id.
//   - otherwise          → 404.
//
// Name lookup is tried first, so a group a human chose to *name* "42"
// stays reachable; the numeric-id path is a strict fallback for tokens
// that match no name. On any failure the error response is already
// written and ok is false.
func resolveMulticastGroup(w http.ResponseWriter, fromID, token string) (g *db.AgentGroup, ok bool) {
	if token == "" {
		return resolveOwnGroup(w, fromID)
	}
	g, err := db.GetAgentGroupByName(token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil, false
	}
	if g != nil {
		return g, true
	}
	// No name match — fall back to a numeric group id, but only for an
	// all-digit token: strconv.ParseInt would otherwise accept signed
	// forms ("+7", "-7") that the documented grammar excludes.
	allDigits := token != ""
	for _, r := range token {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		if id, perr := strconv.ParseInt(token, 10, 64); perr == nil {
			g, err = db.GetAgentGroupByID(id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return nil, false
			}
			if g != nil {
				return g, true
			}
		}
	}
	writeError(w, http.StatusNotFound, "not_found",
		fmt.Sprintf("no group named or numbered %q", token))
	return nil, false
}

// resolveOwnGroup resolves an empty "group:" target to the sender's
// single group. "My own group" is only unambiguous when there is
// exactly one: it is a 400 when the sender is a member of 0 or >1
// active (non-archived) groups. Membership — not ownership — is what
// counts; a manager that owns groups but is a member of none should
// name the team explicitly.
func resolveOwnGroup(w http.ResponseWriter, fromID string) (*db.AgentGroup, bool) {
	groups, err := db.ListGroupsForConv(fromID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil, false
	}
	var active []*db.AgentGroup
	for _, g := range groups {
		if !g.IsArchived() {
			active = append(active, g)
		}
	}
	switch len(active) {
	case 1:
		return active[0], true
	case 0:
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"'group:' with no name resolves to your own group, but you are not a "+
				"member of any group; name a group explicitly, e.g. group:<name>")
		return nil, false
	default:
		names := make([]string, len(active))
		for i, g := range active {
			names[i] = g.Name
		}
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("'group:' is ambiguous — you are a member of %d groups (%s); "+
				"name one explicitly, e.g. group:%s",
				len(active), strings.Join(names, ", "), names[0]))
		return nil, false
	}
}

// handleMulticast fans out req.Body to every member of the target group
// except the sender. The target group is resolved by name, numeric id,
// or — for an empty "group:" — the sender's own group (see
// resolveMulticastGroup). Auth: the sender must be a member or owner of
// the group (we don't allow strangers to broadcast in). When req.Role
// is set, only members whose role matches (case-insensitively) receive
// the message — the filter narrows the recipient set *after* the auth
// gate, so it can never widen reach. Each recipient gets its own
// agent_messages row + tmux nudge if online; replies from any recipient
// go back to the sender as a normal direct message via the group.
//
// Returns 200 with recipients=[] and via_group set (idempotent
// success) when no member other than the sender matched.
func handleMulticast(w http.ResponseWriter, fromID string, req *sendReq) {
	token := strings.TrimSpace(strings.TrimPrefix(req.To, multicastPrefix))
	g, ok := resolveMulticastGroup(w, fromID, token)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	// Sender must be a member OR an owner of the group to broadcast.
	senderMember, err := db.FindMemberInGroup(g.ID, fromID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	isOwner := false
	if senderMember == nil {
		isOwner, err = db.IsAgentGroupOwner(g.ID, fromID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	if senderMember == nil && !isOwner {
		writeError(w, http.StatusForbidden, "auth",
			fmt.Sprintf("you are not a member or owner of group %q", g.Name))
		return
	}
	recipients, err := fanOutToGroup(g, fromID, req.Subject, req.Body, req.Role, req.Members)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sendResp{ViaGroup: g.Name, Recipients: recipients})
}

// fanOutToGroup delivers (subject, body) to every member of group g
// except fromConv. Membership is read at call time, so a recurring
// caller — a group-targeted cron job — always tracks the live roster
// as members join and leave. Each recipient gets its own agent_messages
// row stamped with g.ID plus a tmux nudge when alive; a per-row insert
// failure is recorded (MessageID 0, Delivered false) and does NOT abort
// the rest of the fan-out. roleFilter, when non-empty, narrows the
// recipient set case-insensitively AFTER membership is read — it can
// only shrink reach, never widen it. memberFilter, when non-empty,
// narrows it the same way: only members named in the list — by their
// stable agent_id (the canonical key) or conv-id (back-compat) — receive
// the message. The two filters compose (a member must
// clear both); an id in memberFilter that is not a current member
// matches nothing, so the filter can never widen reach.
//
// This is the shared fan-out core behind both the `group:` multicast
// send (handleMulticast) and group-targeted cron jobs (fireCronJob).
// Keeping the two on this one path means a delivery fix lands for both
// and they can never drift apart. Caller-supplied auth is the caller's
// responsibility — fanOutToGroup itself does no permission checking.
func fanOutToGroup(g *db.AgentGroup, fromConv, subject, body, roleFilter string, memberFilter []string) ([]recipient, error) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil, err
	}
	roleFilter = strings.TrimSpace(roleFilter)
	// memberFilter narrows the fan-out to the listed members. The
	// canonical key is the stable agent_id (JOH-27): an `agt_`-prefixed
	// entry matches a member by its rotation-immune id, so a subset that
	// named a member who reincarnated between the dashboard's snapshot and
	// the send still reaches the live agent. A conv-id entry stays
	// accepted (back-compat with the dashboard's current snapshot, and
	// 1:1 selector parity): it is resolved to its live successor up front
	// and matched against the likewise successor-walked roster id.
	//
	// hasMemberFilter — whether the caller passed any memberFilter at
	// all, NOT whether the want-sets ended up non-empty — is what arms the
	// filter. A caller that passed a non-empty list asked to narrow, so
	// a list whose entries all trim away ({"members":[" "]}) must match
	// NOBODY, never fall back to a full-group broadcast: the filter can
	// only ever shrink reach.
	hasMemberFilter := len(memberFilter) > 0
	wantAgents := make(map[string]bool, len(memberFilter))
	wantConvs := make(map[string]bool, len(memberFilter))
	for _, c := range memberFilter {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if strings.HasPrefix(c, db.AgentIDPrefix) {
			wantAgents[c] = true
		} else {
			wantConvs[db.ResolveLatestConv(c)] = true
		}
	}
	out := []recipient{}
	for _, m := range members {
		if m.ConvID == fromConv {
			continue
		}
		// Role filter: skip members whose role does not match. Roles are
		// free-form human-set strings, so the match is case-insensitive
		// (roleLabelMatches — the shared rule the process phase-role matcher
		// reuses).
		if roleFilter != "" && !roleLabelMatches(m.Role, roleFilter) {
			continue
		}
		// Defensive: membership migrations on reincarnate are atomic
		// today (the new conv-id is added before the old is removed),
		// so a member row should already point at the live successor.
		// But cross-machine sync, manual DB edits, or a future race
		// could leave a stale row. Walk the chain so the message
		// always lands on the live successor; cheap insurance.
		finalConv, originalTo := walkSuccession(m.ConvID)
		if finalConv == fromConv {
			// fromConv may be the live successor of a member row (rare
			// manager-pattern edge case); skip the self-send.
			continue
		}
		// Member filter: skip members the caller did not select. A member
		// is kept when its stable agent_id was listed (the canonical key),
		// or — back-compat — when its successor-walked conv-id matches a
		// likewise-resolved conv entry. Resolving both sides means neither
		// a stale roster row nor a stale id in the caller's list causes a
		// false miss.
		if hasMemberFilter {
			matched := wantConvs[finalConv]
			if !matched {
				if a := peerAgentID(finalConv); a != "" {
					matched = wantAgents[a]
				}
			}
			if !matched {
				continue
			}
		}
		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:        g.ID,
			FromConv:       fromConv,
			ToConv:         finalConv,
			OriginalToConv: originalTo,
			Subject:        subject,
			Body:           body,
		})
		if err != nil {
			// Don't abort the whole fan-out on one DB error; record it
			// and continue. The caller sees per-recipient status and can
			// retry the failures explicitly.
			slog.Warn("fan-out: insert failed",
				"group", g.Name, "to", finalConv, "error", err)
			out = append(out, recipient{
				ConvID:         finalConv,
				AgentID:        peerAgentID(finalConv),
				Title:          agent.TitleFor(finalConv),
				MessageID:      0,
				Delivered:      false,
				RedirectedFrom: originalTo,
			})
			continue
		}
		enqueueDeliveryForConv(finalConv)
		out = append(out, recipient{
			ConvID:         finalConv,
			AgentID:        peerAgentID(finalConv),
			Title:          agent.TitleFor(finalConv),
			MessageID:      id,
			Queued:         true,
			Pending:        queueDepthFor(finalConv, false),
			RedirectedFrom: originalTo,
		})
	}
	return out, nil
}

// messageNudgeText builds the bracketed tmux nudge for a delivered agent
// message, naming the sender by stable identity (JOH-27 PR3b) — the
// agent_id is rotation-immune, so it is safe to surface in the receiver's
// transcript where a conv-id prefix would have gone stale. On a read miss
// it degrades to the terse, senderless form. Shared by nudgeIfAlive (live
// delivery) and sendNudgeBracket (deferred flush) so the wording lives in
// one place.
func messageNudgeText(msgID int64) string {
	if m, err := db.GetAgentMessage(msgID); err == nil && m != nil {
		if sender := agent.MessageSenderLabel(m.FromConv, m.FromAgent); sender != "" {
			return fmt.Sprintf(
				"[system: new agent message #%d from %s for you. fetch with: tclaude agent inbox read %d]",
				msgID, sender, msgID)
		}
	}
	return fmt.Sprintf(
		"[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]",
		msgID, msgID)
}

// nudgeIfAlive looks up the target's tmux session and, if alive, sends
// the bracketed system-style nudge. The returned deliveryOutcome
// distinguishes delivered / queued-offline / held — see deliveryOutcome.
//
// This is the half that broke for sandboxed senders in v1: the daemon
// owns the tmux side here, so the sender's sandbox is irrelevant.
//
// The DB can hold multiple session rows for the same conv_id (auto-register
// creates new rows alongside stale ones from previous launches). We pick
// the first one whose tmux session is actually alive, most-recent first.
func nudgeIfAlive(msgID int64, toID string) deliveryOutcome {
	candidates, err := db.FindSessionsByConvID(toID)
	if err != nil {
		return outcomeQueued
	}
	var sess *db.SessionRow
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		if session.IsTmuxSessionAlive(c.TmuxSession) {
			sess = c
			break
		}
	}
	if sess == nil {
		return outcomeQueued
	}
	// Hold delivery if the pane is blocked on a human (awaiting_input /
	// awaiting_permission). Injecting a nudge now would be captured by the
	// open dialog as the human's answer, and the real notification lost.
	// Leave delivered_at empty so a later flush — once the agent is back to
	// working/idle — delivers it. See isAwaitingHumanInput.
	if isAwaitingHumanInput(sess.Status) {
		slog.Info("nudge held: recipient awaiting human input",
			"msg_id", msgID, "to", toID, "status", sess.Status)
		return outcomeHeld
	}
	// Announce the message, naming the sender by stable identity (JOH-27
	// PR3b). Subject, group and reply addressing still live in the message
	// itself (fetched via `tclaude agent inbox read <id>`) so the line stays
	// short. The earlier form was deliberately senderless to avoid leaking
	// *ephemeral* details — but the agent_id is rotation-immune, so unlike a
	// conv-id prefix it does not go stale on the sender's next reincarnate;
	// the title is truncated so it cannot dominate the line.
	nudge := messageNudgeText(msgID)
	if err := injectTextAndSubmit(sess.TmuxSession+":0.0", nudge); err != nil {
		slog.Warn("nudge failed", "error", err, "tmux", sess.TmuxSession)
		// Treat an inject failure like the offline case: delivered_at stays
		// empty so a later flush retries.
		return outcomeQueued
	}
	// delivered_at is internal bookkeeping; the nudge itself already
	// landed, so log on failure rather than failing the whole call.
	if err := db.MarkAgentMessageDelivered(msgID); err != nil {
		slog.Warn("failed to record delivered_at", "error", err, "msg_id", msgID)
	}
	return outcomeDelivered
}

// injectSlashCommand finds an alive tmux session for convID and types the
// given slash-command line into its CC pane, followed by a submit Enter.
// If followUp is non-empty, it is sent as a fresh prompt right after the
// slash submit. Returns true on successful delivery.
//
// reason is a short human-readable label of WHAT triggered this injection
// (e.g. "self-compact", "compact (caller=ab12cd34)", "soft-exit",
// "rename"). It is recorded in the success log line so that every
// slash command tclaude types into a pane — most importantly /compact —
// has an audit trail in ~/.tclaude/output.log. send-keys is the one
// channel through which tclaude can make a pane do something the agent
// did not type itself, so "where did that /compact come from?" must be
// answerable after the fact from the log alone.
//
// Note: when used with /compact, the follow-up bytes queue in the pty
// until CC resumes reading after the slash command settles. We don't
// wait for the slash to complete — there's no clean way to detect it
// without a hook. The follow-up may land in a still-busy textarea on
// unlucky timing; agents that depend on tight ordering should poll
// context-info and submit the follow-up themselves once compact has
// resolved.
//
// The per-pane lock (JOH-310) makes the slash submit and the follow-up
// each atomic, but NOT the pair: another injector can take the pane
// between them. That is consistent with the best-effort follow-up
// ordering described above and is not a regression.
func injectSlashCommand(convID, line, followUp, reason string) bool {
	sess := aliveSessionForConv(convID)
	if sess == nil {
		return false
	}
	target := sess.TmuxSession + ":0.0"
	if err := injectTextAndSubmit(target, line); err != nil {
		slog.Warn("slash-command inject failed", "error", err, "tmux", sess.TmuxSession, "line", line, "reason", reason)
		return false
	}
	slog.Info("slash-command injected via send-keys",
		"conv_id", convID,
		"line", line,
		"reason", reason,
		"tmux_session", sess.TmuxSession,
		"has_follow_up", followUp != "",
	)
	if followUp != "" {
		if err := injectTextAndSubmit(target, followUp); err != nil {
			slog.Warn("slash-command follow-up failed", "error", err, "tmux", sess.TmuxSession)
			return false
		}
	}
	return true
}

// paneInjectMu guards paneInjectLocks. paneInjectLocks holds one mutex
// per tmux pane target, serializing send-keys injection into that pane
// (JOH-310).
//
// Every channel through which the daemon types into a CC pane — live
// inbox nudges (nudgeIfAlive), flushed/queued nudges (sendNudgeBracket),
// slash commands (/rename, /compact, soft-exit), the welcome prompt,
// export nudges, and the remote-access toggle — runs a MULTI-STEP
// send-keys sequence (text → settle → Enter → settle → Enter, or a full
// menu walk). Those steps are NOT atomic at the tmux layer, so two
// sequences racing on the same pane interleave: the text from one and
// the Enter from another land in the same input box, submitting a
// garbled or merged prompt (and a nudge can be lost).
//
// This still matters under the async per-agent delivery queue (JOH-310):
// inbox nudges no longer run on the sender's request, but the per-agent and
// per-conv drains can both target one pane (a head-following and a pinned
// message), and the unread-reminder sweep, slash/welcome/export injectors and
// cron all type into the same panes. The lock keeps any two such sequences
// single-file.
//
// SCOPE — daemon-side only. This is an in-process mutex, so it serializes
// the agentd injectors listed above against each other. send-keys into
// the same pane from OTHER processes (the CC hook's own /rename, CLI
// subprocesses) is not coordinated by it; those are rare and out of scope
// here — covering them would need a tmux-level / file lock.
//
// COST — the lock is held across injectTextAndSubmit's two ~500 ms settle
// sleeps (and injectMenuToggle's whole menu walk). Under the async model the
// waiters are BACKGROUND drain/reminder goroutines, not the sender's request
// (the send path enqueues and returns), so this latency is no longer on any
// caller's critical path — it only single-files the daemon's own injectors.
//
// The map gains one entry per distinct pane target the daemon ever
// injects into (bounded by total agents launched this daemon life) and
// is never pruned — a handful of bytes per ever-seen pane, negligible at
// personal scale and not worth the lock churn of cleanup.
var (
	paneInjectMu    sync.Mutex
	paneInjectLocks = map[string]*sync.Mutex{}
	// A nudge claim lives for two minutes. Keep lock acquisition well inside
	// that lease so a live worker cannot wait past claim expiry and later
	// duplicate an injection performed by the worker that reclaimed it.
	paneInjectLockTimeout = time.Minute
)

var errPaneInjectLockTimeout = errors.New("pane injection lock timeout")

// paneInjectLock returns the mutex serializing send-keys injection into
// tmuxTarget, creating it on first use. Hold it for the WHOLE injection
// sequence (text + both Enters, or a full menu walk) — never just a
// single send-keys call — so the sequence is atomic against other
// injectors hitting the same pane.
func paneInjectLock(tmuxTarget string) *sync.Mutex {
	paneInjectMu.Lock()
	defer paneInjectMu.Unlock()
	mu := paneInjectLocks[tmuxTarget]
	if mu == nil {
		mu = &sync.Mutex{}
		paneInjectLocks[tmuxTarget] = mu
	}
	return mu
}

func acquirePaneInjectLock(mu *sync.Mutex) error {
	if mu.TryLock() {
		return nil
	}
	timer := time.NewTimer(paneInjectLockTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		case <-timer.C:
			return fmt.Errorf("%w after %s", errPaneInjectLockTimeout, paneInjectLockTimeout)
		}
	}
}

// injectTextAndSubmit types `text` into a CC pane and submits it as a
// fresh prompt. Splits the text and the submit Enter into separate
// `send-keys` calls with a 500 ms gap so CC's bracketed-paste mode
// can't coalesce the trailing Enter into a paste-newline — when that
// happens, the text gets pasted into the input box but never submitted.
// (We learned this the hard way during reincarnate's handoff nudge:
// rename worked, the [system: new agent message ...] text appeared
// in the prompt, and neither Enter actually submitted because both
// arrived back-to-back during the same paste-mode window. 200 ms was
// enough in casual testing; 500 ms is the safety margin for slower
// terminals / heavier load.)
//
// The trailing Enter is sent twice (belt-and-suspenders); the second
// is a no-op if the first already submitted. Caller must have verified
// the tmux pane is alive.
//
// The whole sequence runs under the pane's injection lock so two
// injectors targeting the same pane single-file instead of interleaving
// their send-keys (JOH-310 — see paneInjectLock).
func injectTextAndSubmit(tmuxTarget, text string) error {
	mu := paneInjectLock(tmuxTarget)
	if err := acquirePaneInjectLock(mu); err != nil {
		return err
	}
	defer mu.Unlock()
	// Exact-match the session part of the target (clcommon.ExactTarget):
	// callers pass raw "name" / "name:0.0" targets, and a bare -t would
	// prefix-match a live namesake pane if the target dies between the
	// caller's liveness check and this send — landing the keystrokes in the
	// wrong agent's prompt. Lock keys stay on the raw target.
	target := clcommon.ExactTarget(tmuxTarget)
	if err := runTmuxCommand("send-keys", "-t", target, text); err != nil {
		return fmt.Errorf("send-keys text: %w", err)
	}
	time.Sleep(injectSettleDelay)
	if err := runTmuxCommand("send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("send-keys submit: %w", err)
	}
	time.Sleep(injectSettleDelay)
	_ = runTmuxCommand("send-keys", "-t", target, "Enter")
	return nil
}

// injectMenuToggle types a slash command that opens a confirm MENU, submits
// it EXACTLY ONCE, waits confirmDelay for the menu to render, then walks the
// menu with menuKeys — one key per send-keys, a stepDelay between each.
//
// It deliberately does NOT route through injectTextAndSubmit. That helper
// sends a belt-and-suspenders SECOND Enter after the submit (paste-coalescing
// insurance), which is harmless for a command that just submits a prompt — but
// fatal for one that opens a menu: the stray Enter lands ON the confirm menu
// and accepts its DEFAULT entry, tearing the menu down before menuKeys can
// move the highlight. That is exactly what left Remote Access stuck ON when
// the operator hit "disable" (the default entry is "keep connected", not
// "disconnect"). So here the toggle is submitted with a single Enter, after a
// settle gap so bracketed-paste mode can't coalesce that one Enter into the
// paste. That toggle→Enter gap deliberately reuses the package-global
// injectSettleDelay (it's the same paste-coalescing concern as
// injectTextAndSubmit, so the two stay in lockstep under
// SetInjectSettleDelayForTest); only the menu-render and per-key settles are
// parameters, since those are remote-control-specific. Caller must have
// verified the pane is alive and that the command opens a menu.
//
// Like injectTextAndSubmit, the whole toggle+menu-walk runs under the
// pane's injection lock so a concurrent nudge can't slip a send-keys
// into the middle of the menu navigation (JOH-310 — see paneInjectLock).
// It does NOT call injectTextAndSubmit, so there is no re-entrant lock.
func injectMenuToggle(tmuxTarget, toggle string, menuKeys []string, confirmDelay, stepDelay time.Duration) error {
	mu := paneInjectLock(tmuxTarget)
	if err := acquirePaneInjectLock(mu); err != nil {
		return err
	}
	defer mu.Unlock()
	// Exact-match the session part — same reasoning as injectTextAndSubmit.
	target := clcommon.ExactTarget(tmuxTarget)
	if err := runTmuxCommand("send-keys", "-t", target, toggle); err != nil {
		return fmt.Errorf("send-keys toggle: %w", err)
	}
	time.Sleep(injectSettleDelay)
	if err := runTmuxCommand("send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("send-keys submit: %w", err)
	}
	// Let the confirm menu render before moving its highlight.
	time.Sleep(confirmDelay)
	for i, key := range menuKeys {
		if i > 0 {
			time.Sleep(stepDelay)
		}
		if err := runTmuxCommand("send-keys", "-t", target, key); err != nil {
			return fmt.Errorf("send-keys menu key %q: %w", key, err)
		}
	}
	return nil
}

// injectSettleDelay is the gap injectTextAndSubmit leaves between its
// send-keys calls (see that function for why 500 ms in production). It
// is a package var, not a constant, so flow tests can shrink it to
// ~nothing: the simulator processes keystrokes synchronously and needs
// no settle window, yet a hardcoded 500 ms made every injection-driven
// flow test (soft /exit, /rename, welcome, nudge) sit on ~1 s of real
// sleeps. Overridden via SetInjectSettleDelayForTest in the flow
// harness setup; production keeps the 500 ms safety margin.
var injectSettleDelay = 500 * time.Millisecond

// handleWhoamiRename injects `/rename <title>` into the caller's own CC
// pane. Permission-gated on `self.rename`.
//
// Title is restricted to [A-Za-z0-9_-]+ (min 1, max 64 chars) to prevent
// keystroke-injection. Since the title becomes literal send-keys input,
// anything in it (newlines, slashes, control chars) lands in the input
// box; a permissive title would let a permitted agent execute arbitrary
// slash commands by sneaking a newline + another `/<cmd>` in.
func handleWhoamiRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfRename)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint renames the calling agent's own conversation; humans should use Claude Code's /rename directly, or use POST /v1/agent/{conv}/rename to rename another agent")
		return
	}
	runRenameOrchestration(w, r, convID, convID)
}

// handleAgentRename injects `/rename <title>` into ANOTHER agent's CC
// pane. Routed via handleAgentByConv. Auth: agent.rename slug OR caller
// is owner of a group containing target.
func handleAgentRename(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentRename, targetConv)
	if !ok {
		return
	}
	runRenameOrchestration(w, r, targetConv, caller)
}

// runRenameOrchestration validates the title charset, injects
// `/rename <title>` into the target's pane, and writes the JSON
// response. caller is recorded in the response when distinct from
// target so the audit trail has both sides.
//
// When body.Auto is true, the title is ignored and instead a
// bracketed `[system: …]` nudge is injected asking the agent to
// pick a title for itself via the agent-rename skill / CLI. Same
// auth, same tmux delivery mechanism — only the payload changes.
func runRenameOrchestration(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		Title string `json:"title"`
		Auto  bool   `json:"auto"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}

	if body.Auto {
		// Auto-rename: defer the title choice to the agent itself.
		// The nudge text uses the same bracketed [system: …] shape
		// as agent_messages so the recipient reads it as a system
		// prompt rather than user input. Spelling out the allowed
		// charset up front saves a back-and-forth when the agent
		// picks something the title validator would reject.
		nudge := "[system: please rename yourself to give this conversation a clearer title. " +
			"Run: tclaude agent rename \"<your-chosen-title>\". " +
			"Pick a 3-4-word kebab-case slug that captures what you've been working on or what " +
			"your role is — e.g. \"fix-bug-abc-123\", \"working-on-new-ui\", or \"worker-agent-a\". " +
			"Allowed: 1-64 characters from [A-Za-z0-9_-[]{}() ] only; single spaces ok, " +
			"no slashes / quotes / newlines / unicode.]"
		if !injectSlashCommand(target, nudge, "", "auto-rename-nudge") {
			writeError(w, http.StatusServiceUnavailable, "no_tmux",
				"target conv "+short8(target)+" has no live tmux session to inject auto-rename nudge into")
			return
		}
		resp := map[string]any{
			"conv_id": target,
			"auto":    true,
			"note":    "auto-rename nudge submitted via tmux send-keys; the target will pick its own title on its next turn",
		}
		if caller != "" && caller != target {
			resp["caller_conv"] = caller
			stampCallerAgentID(resp, caller)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	body.Title = strings.TrimSpace(body.Title)
	if !isValidRenameTitle(body.Title) {
		writeError(w, http.StatusBadRequest, "invalid_title",
			"REJECTED. Title must be 1-64 characters from [A-Za-z0-9_-[]{}() ]. "+
				"Single ASCII spaces are allowed; consecutive spaces, tabs, newlines, "+
				"slashes, quotes, and unicode are NOT allowed and will not be allowed. "+
				"This is a hard security gate against keystroke injection (the title becomes "+
				"literal tmux send-keys input) — it is not a style preference, not configurable, "+
				"and not bypassable. Do not retry with a similar title; pick one that uses only "+
				"the allowed characters.")
		return
	}
	if !deliverRename(target, body.Title) {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"could not deliver rename to conv "+short8(target)+" (no live tmux session to inject into, or the harness's title store rejected it)")
		return
	}
	resp := map[string]any{
		"conv_id": target,
		"title":   body.Title,
		"note":    "rename delivered; the harness will surface the new title on its next turn",
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// compactToken is the lifecycle-slash selector for compaction: the
// target harness's compact command (CC's `/compact`), or "" when the
// harness has no scriptable compaction. nil-safe so a bare descriptor
// can't panic the handler.
func compactToken(h *harness.Harness) string {
	if h == nil || h.Life == nil {
		return ""
	}
	return h.Life.CompactCommand()
}

// handleWhoamiCompact injects the caller harness's compact command into
// the caller's own pane. Optional follow-up text is queued as a fresh
// prompt right after. Permission-gated on `self.compact`.
func handleWhoamiCompact(w http.ResponseWriter, r *http.Request) {
	handleSelfSlash(w, r, PermSelfCompact, compactToken, "compact")
}

// handleAgentCompact injects the target harness's compact command into
// ANOTHER agent's pane. Routed via handleAgentByConv (the dispatcher
// resolves targetConv from the URL). Auth: agent.compact slug OR caller
// is owner of a group containing target. Same body shape as the self
// variant.
func handleAgentCompact(w http.ResponseWriter, r *http.Request, targetConv string) {
	handleAgentSlash(w, r, PermAgentCompact, targetConv, compactToken, "compact")
}

// slashToken selects a harness's lifecycle slash command (e.g. its
// compact command) for a given target harness, returning "" when the
// harness does not support that action.
type slashToken func(*harness.Harness) string

// handleSelfSlash factors out self-targeted lifecycle-slash handlers like
// /compact. The command itself is sourced from the target harness via
// token (resolved once the target conv — and thus its harness — is known).
func handleSelfSlash(w http.ResponseWriter, r *http.Request, perm string, token slashToken, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, perm)
	if !ok {
		return
	}
	if convID == "" {
		// No calling conv to resolve a harness from; name the default
		// harness's command in the guidance message.
		slash := token(harness.Default())
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own conversation; humans should use the harness's "+slash+" directly, or use POST /v1/agent/{conv}/"+label+" to act on another agent")
		return
	}
	runSlashOrchestration(w, r, convID, convID, token, label)
}

// handleAgentSlash is the cross-agent counterpart to handleSelfSlash.
// Auth via requireCrossAgentPermission (slug OR owner-of-group).
func handleAgentSlash(w http.ResponseWriter, r *http.Request, perm, targetConv string, token slashToken, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, perm, targetConv)
	if !ok {
		return
	}
	runSlashOrchestration(w, r, targetConv, caller, token, label)
}

// slashReason renders the audit "reason" recorded when a lifecycle slash
// (e.g. /compact) is injected into a pane, capturing WHO drove it so the
// output.log line answers "where did that /compact come from?":
//   - caller == ""      → a human via the dashboard / an owner over the
//     socket (cross-agent handlers return "" for the human peer class).
//   - caller == target  → an agent acting on its own conversation.
//   - otherwise         → a cross-agent call; the caller conv is named.
func slashReason(label, caller, target string) string {
	switch caller {
	case "":
		return label + " (human/dashboard)"
	case target:
		return "self-" + label
	default:
		return label + " (caller=" + short8(caller) + ")"
	}
}

// runSlashOrchestration validates the optional follow_up body, injects
// the slash command into the target's pane, and writes the JSON
// response. caller is recorded in the response for cross-agent calls
// so the audit trail has both sides; for self the value is the same as
// target.
func runSlashOrchestration(w http.ResponseWriter, r *http.Request, target, caller string, token slashToken, label string) {
	var body struct {
		FollowUp string `json:"follow_up"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	if body.FollowUp != "" && !isValidFollowUp(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			"REJECTED. Follow-up must be 1-4096 printable characters; tabs, newlines, "+
				"and other control characters are not allowed (each newline would be "+
				"treated as a submit by tmux send-keys, splitting the prompt). Strip "+
				"control chars and resubmit.")
		return
	}
	// Source the slash command from the target's harness so a pane is
	// never typed a command it can't parse. "" = the harness has no such
	// command (e.g. a harness without scriptable compaction).
	h := harnessForConv(target)
	slash := token(h)
	if slash == "" {
		writeError(w, http.StatusBadRequest, "unsupported",
			"harness "+h.Name+" does not support "+label)
		return
	}
	if !injectSlashCommand(target, slash, body.FollowUp, slashReason(label, caller, target)) {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session to inject "+slash+" into")
		return
	}
	resp := map[string]any{
		"conv_id": target,
		"action":  label,
		"note":    slash + " submitted via tmux send-keys; CC will process it on its next turn",
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	if body.FollowUp != "" {
		resp["follow_up"] = body.FollowUp
		resp["note"] = slash + " + follow-up submitted via tmux send-keys; the follow-up bytes queue in the pty until CC resumes reading after " + slash + " settles"
	}
	writeJSON(w, http.StatusOK, resp)
}

// isValidFollowUp enforces follow-up prompt sanitization.
//
// Unlike rename titles (which need a hard charset gate against
// keystroke-injection across agents), the follow-up is a free-form
// prompt the agent submits to *itself* — there's no privilege
// escalation surface, since the agent already runs in its own pane.
//
// We only reject control characters (newlines, tabs, NUL, etc.)
// because each newline in tmux send-keys would land as a prompt-submit,
// fragmenting the follow-up into multiple turns. Length is capped at
// 4096 bytes to keep tmux invocations reasonable.
func isValidFollowUp(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r == ' ' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// isValidInitialMessage validates a spawn's initial-context brief.
//
// Unlike isValidFollowUp — which guards text typed into a tmux pane,
// where a raw newline would land as a premature prompt-submit — the
// initial message is delivered to the new agent's inbox as an
// agent_messages row and rendered by `inbox read`. So newlines and
// tabs are allowed (and wanted: a multi-line brief keeps its
// paragraph structure). We still reject other control characters
// (NUL, escape, carriage return, …) that would corrupt a terminal
// render, and cap the length at agent.MaxInitialMessageBytes.
//
// An empty string is valid — it simply means "no initial message".
func isValidInitialMessage(s string) bool {
	if len(s) > agent.MaxInitialMessageBytes {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// isValidSpawnName enforces the agent-name charset at the spawn boundary
// (handleGroupSpawn): ASCII letters, digits, '_' and '-' only — no
// spaces, punctuation, or unicode — and a 1..agent.MaxSpawnNameLen length
// when non-empty. An empty name is valid (the name is optional; the agent
// gets an auto-generated label). The client-side mirror is
// agent.isValidSpawnName, and both share agent.MaxSpawnNameLen so the caps
// stay identical.
//
// This is intentionally stricter than isValidRenameTitle (which allows
// spaces / brackets / parens): a spawn name doubles as a git worktree
// branch name (the dashboard's name→branch sync), so it must be a safe
// branch token, and the strict set is a clean subset of the rename
// charset, so a non-empty name that passes here always clears the
// downstream isValidRenameTitle gate that decides the post-spawn /rename.
func isValidSpawnName(name string) bool {
	if len(name) > agent.MaxSpawnNameLen {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// maxSpawnAttachments caps how many attachment paths a single spawn may carry.
// The dashboard upload endpoint enforces its own per-batch caps; this is the
// daemon-side backstop on the spawn request itself so a hand-rolled caller
// can't balloon the briefing.
const maxSpawnAttachments = 50

// sanitizeSpawnAttachments cleans the spawn request's attachment paths for the
// startup briefing: trims each, drops blanks, and rejects any path containing a
// control character (newline included — each path is a single briefing line) or
// exceeding a sane length, since the list is rendered by `inbox read` and may
// ride inline in the launch prompt. The paths are NOT access-checked: the
// daemon never opens the files — the spawned agent reads them itself, as the
// same user — so an arbitrary path only ever tells the new agent "look here".
// Returns the cleaned list, or a non-empty error message describing the first
// rejection.
func sanitizeSpawnAttachments(paths []string) ([]string, string) {
	if len(paths) > maxSpawnAttachments {
		return nil, fmt.Sprintf("too many attachments: %d (max %d)", len(paths), maxSpawnAttachments)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > 4096 {
			return nil, fmt.Sprintf("attachment path too long: %d bytes (max 4096)", len(p))
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return nil, "attachment path contains a control character"
			}
		}
		out = append(out, p)
	}
	return out, ""
}

// handleWhoamiContext returns the caller's own context-window state.
// Read-only and self-targeted, so no permission gate — any agent can
// introspect its own session. Returns 0/0 if the row hasn't been
// populated yet (statusbar hook hasn't fired this session).
//
// Note: context_pct is keyed in SQLite by tclaude's session ID (the
// label, not the conv-id) because the statusbar hook only knows
// TCLAUDE_SESSION_ID at write time. So we resolve conv-id → session
// row first, preferring an alive tmux session when multiple historical
// rows share the same conv-id.
func handleWhoamiContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	writeContextInfo(w, convID, "")
}

// handleAgentContext returns ANOTHER agent's context-window state — the
// manager-pattern read reached via /v1/agent/{selector}/context. Unlike
// the directory read (ungated, see handleAgentDir), context usage is
// gated like the other cross-agent verbs: the caller passes with the
// agent.context-info slug, or by owning a group containing the target
// (the owner bypass — a lead never needs a slug to watch its own team).
//
// An explicit deny override is ALWAYS authoritative and suppresses the
// owner bypass — the universal precedence resolvePermission /
// requireCrossAgentPermission apply to every cross-agent verb. The owner
// bypass only fills the "undecided" gap (no grant, no deny). Read-only,
// so GET only; the X-Tclaude-Ask-Human popup escape hatch still applies.
func handleAgentContext(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentContextInfo, targetConv)
	if !ok {
		return
	}
	writeContextInfo(w, targetConv, caller)
}

// writeContextInfo resolves convID to its most-relevant session row and
// writes that row's context snapshot. caller is the requesting agent's
// conv-id on the cross-agent path (echoed for the audit trail) and ""
// for self / human reads. Shared by the self and cross-agent handlers.
func writeContextInfo(w http.ResponseWriter, convID, caller string) {
	aliveSessions, _ := session.LiveTmuxSessions()
	snap, sessionID, _ := contextSnapshotForConvIn(convID, aliveSessions)
	resp := map[string]any{
		"conv_id":             convID,
		"session_id":          sessionID,
		"context_pct":         snap.ContextPct,
		"tokens_input":        snap.TokensInput,
		"tokens_output":       snap.TokensOutput,
		"context_window_size": snap.ContextWindowSize,
		"model":               snap.Model,
	}
	if caller != "" && caller != convID {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// contextSnapshotForConvIn resolves convID to the session row whose
// context snapshot best represents it — a live tmux pane preferred, else
// the most-recent historical row — and reads that row's snapshot. The
// alive set is passed in (fetched once per request) so a group-wide
// listing does one tmux ls, not one per member. hasSession is false when
// no session row exists for the conv at all (never launched under
// tclaude); the snapshot is then the zero value.
func contextSnapshotForConvIn(convID string, aliveSet map[string]struct{}) (snap db.ContextSnapshot, sessionID string, hasSession bool) {
	candidates, _ := db.FindSessionsByConvID(convID)
	sess := pickWithLiveness(candidates, func(t string) bool {
		if t == "" {
			return false
		}
		_, ok := aliveSet[t]
		return ok
	})
	if sess == nil {
		return db.ContextSnapshot{}, "", false
	}
	refreshCodexContextSnapshotOnRead(sess, sessionRowAliveIn(sess, aliveSet))
	if s, err := db.GetContextSnapshot(sess.ID); err == nil {
		snap = s
	}
	return snap, sess.ID, true
}

// pickWithLiveness returns the session row whose tmux pane is alive
// (per the injected liveness predicate), or — falling back — the row
// that comes first in the list (which FindSessionsByConvID orders by
// updated_at DESC). nil when the list is empty. The predicate is
// injected so callers can supply a pre-fetched alive set (a single tmux
// ls for a whole listing) and unit tests can stub it without reaching
// for tmux on the host.
func pickWithLiveness(candidates []*db.SessionRow, isAlive func(string) bool) *db.SessionRow {
	for _, c := range candidates {
		if isAlive(c.TmuxSession) {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return nil
}

// isValidRenameTitle enforces the rename title charset. Hard cap at 64
// chars (CC display titles get truncated anyway, and longer is just
// asking for keystroke-injection edge cases).
//
// Allowed: [A-Za-z0-9_\-\[\]{}() ]. Single ASCII spaces are allowed
// for readability ("code reviewer"), but consecutive spaces and any
// other whitespace (tabs, newlines, NBSP, etc.) are rejected. Caller
// should TrimSpace before calling so leading/trailing spaces don't
// sneak past either.
//
// Anything that could let `tmux send-keys` interpret a control
// sequence — newlines, slashes, quotes, tabs — stays out.
func isValidRenameTitle(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	if strings.Contains(t, "  ") {
		return false
	}
	return renameTitleCharsetOK(t)
}

// renameTitleCharsetOK reports whether every rune in t is in the rename
// title's allowed charset — no length or spacing checks. It is the
// injection-safety core shared by isValidRenameTitle (which layers on a
// 64-char cap + no-double-space readability rule) and isValidRenameSink
// (the length-exempt send-keys gate). Allowed: [A-Za-z0-9_\-\[\]{}() ].
// Anything `tmux send-keys` could interpret as a control sequence —
// newlines, tabs, slashes, quotes, NUL, unicode — is rejected, so a
// title that passes can never carry a premature Enter into a pane.
func renameTitleCharsetOK(t string) bool {
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		case r == '[' || r == ']' || r == '{' || r == '}':
		case r == '(' || r == ')':
		case r == ' ':
		default:
			return false
		}
	}
	return true
}

// isValidRenameSink is the charset-only rename gate used on the
// send-keys injection path for titles that may legitimately exceed
// isValidRenameTitle's 64-char cap. A clone carry title is
// `<original-title>-c-<N>`, and a reincarnation archive-renames its
// predecessor to `<predecessor-title>-x` (or `-x-<N>`) — a title already
// at the 64-char display max would push the suffixed form past the cap,
// so reusing isValidRenameTitle here would reject a perfectly legitimate
// title (and, pre-JOH-177, the reincarnate path injected with NO gate at
// all rather than over-reject). The injection-relevant
// property is the CHARSET — reject any rune `tmux send-keys` would treat
// as an early submit / control sequence — not the length, so this keeps
// the charset rule and drops the cap. An empty title is rejected
// (nothing to inject; callers fall back to leaving the pane unnamed).
func isValidRenameSink(t string) bool {
	if t == "" {
		return false
	}
	return renameTitleCharsetOK(t)
}

// --- /v1/messages/{id} (GET) and /v1/messages/{id}/reply (POST) ---

// handleMessageByIDOrReply dispatches between the message-fetch,
// reply, and delete endpoints based on path suffix and HTTP method.
// GET  /v1/messages/{id}        -> handleMessageByID
// POST /v1/messages/{id}/reply  -> handleMessageReply
// DELETE /v1/messages/{id}      -> handleMessageDelete
func handleMessageByIDOrReply(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/messages/")
	if rest == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing message id")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 2 && parts[1] == "reply" {
		handleMessageReply(w, r, parts[0])
		return
	}
	if r.Method == http.MethodDelete {
		handleMessageDelete(w, r, parts[0])
		return
	}
	handleMessageByID(w, r)
}

// handleMessageDelete removes a single agent_messages row when the
// caller is a party to it (sender or recipient). Mirrors the auth
// model of `inbox prune` (which already lets parties wipe rows by
// time-cutoff) — this just narrows the cutoff to one ID for use by
// the inbox-watch TUI.
func handleMessageDelete(w http.ResponseWriter, r *http.Request, idStr string) {
	myID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid id")
		return
	}
	deleted, err := deleteMessageForCaller(id, myID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if !deleted {
		// Two-step check so the caller gets a useful error: 404 when
		// the row never existed, 403 when it exists but they're not a
		// party. Probing only on the cold path keeps the happy path
		// at one DB write.
		m, err := db.GetAgentMessage(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if m == nil {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no message #%d", id))
			return
		}
		writeError(w, http.StatusForbidden, "auth",
			"you are not a party to this message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// handleMessageReply lets the recipient of a message reply without
// having to look up the sender's conv-id themselves. The daemon resolves
// it from the original message row, validates that the caller is the
// recipient, and routes the reply through the same send path as
// /v1/messages.
func handleMessageReply(w http.ResponseWriter, r *http.Request, idStr string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	myID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid id")
		return
	}
	orig, err := db.GetAgentMessage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if orig == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no message #%d", id))
		return
	}
	if !callerIsRecipient(myID, orig) {
		writeError(w, http.StatusForbidden, "auth", "you are not the recipient of this message")
		return
	}
	var body struct {
		Subject string `json:"subject,omitempty"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is empty")
		return
	}
	subject := body.Subject
	if subject == "" && orig.Subject != "" {
		subject = "Re: " + orig.Subject
	}
	// Reply path is open: if you received a message, you can reply
	// to it regardless of current group membership. This lets a
	// group owner address a member without being a peer themselves
	// — the member can still reply. The shared-group rule still
	// applies to *spontaneous* messages (handleMessages).
	//
	// Routing: keep the reply on the same group_id as the original, so
	// threads stay coherent on the recipient's side even when shared
	// membership has since dissolved. A group_id of 0 means the
	// original was a direct message (the universal-inbox transport for
	// off-group / solo sends) — the reply is direct too.
	var replyGroupID int64
	var replyViaName string
	if orig.GroupID != 0 {
		via, err := db.GetAgentGroupByID(orig.GroupID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// via == nil: the routing group was deleted since the original
		// was sent. DeleteAgentGroup now rewrites such rows to
		// group_id 0, so this is unreachable in practice — but treat a
		// missing group as "direct" rather than erroring, defensively.
		if via != nil {
			replyGroupID = via.ID
			replyViaName = via.Name
		}
	}
	// Reply target is the original sender. If they've since
	// reincarnated, their old conv-id is still on the message row
	// (immutable audit trail). Walk the chain so the reply lands on
	// the live successor instead of the archived inbox.
	replyTarget, replyOriginalTo := walkSuccession(orig.FromConv)
	if replyTarget == "" {
		// The original message has no sender to reply to — it was
		// injected by the system or a human-initiated spawn (the
		// "Startup context" brief is the canonical case: a human
		// spawner has no conv-id). Without this guard the reply would
		// be inserted with to_conv="" — an orphan row no inbox query
		// ever matches — and the CLI would still print "queued", a
		// status that never resolves. Reject observably so the caller
		// sends a fresh message instead.
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("cannot reply to message #%d: it has no sender (injected by the "+
				"system or a human-initiated spawn); send a fresh message with "+
				"`tclaude agent message <target> ...` instead", id))
		return
	}
	if replyTarget == myID {
		// The chain-walk has identified the recipient (us) as the
		// live successor of the original sender — i.e. we received
		// the original from our own predecessor before reincarnating.
		// Replying to ourselves is nonsensical; reject observably so
		// the caller can choose to write a fresh message instead of
		// looping into their own inbox. (Caught in production by the
		// system replying to a `reincarnation handoff` message from
		// its own predecessor.)
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"cannot reply: original sender has been superseded by you; the predecessor's chain points back to your own conv")
		return
	}
	newID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:        replyGroupID,
		FromConv:       myID,
		ToConv:         replyTarget,
		OriginalToConv: replyOriginalTo,
		Subject:        subject,
		Body:           body.Body,
		ParentID:       orig.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	enqueueDeliveryForConv(replyTarget)
	writeJSON(w, http.StatusOK, sendResp{
		ID:             newID,
		Queued:         true,
		Pending:        queueDepthFor(replyTarget, false),
		ViaGroup:       replyViaName,
		RedirectedFrom: replyOriginalTo,
	})
}

func handleMessageByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// Operator view: same header as /v1/inbox. When the operator reads
	// someone else's message, we force keep-unread (below) so the read
	// marker reflects the recipient's actual interaction, not the
	// operator's drive-by.
	myID, isOperator, ok := requireInboxAccess(w, r)
	if !ok {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/messages/")
	if idStr == "" || strings.Contains(idStr, "/") {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing id")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid id")
		return
	}
	m, err := db.GetAgentMessage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no message #%d", id))
		return
	}
	if !callerIsRecipient(myID, m) {
		writeError(w, http.StatusForbidden, "auth", "message is not addressed to you")
		return
	}
	if !isOperator && r.URL.Query().Get("keep-unread") != "1" && m.ReadAt.IsZero() {
		if err := db.MarkAgentMessageRead(id); err != nil {
			// User asked us to mark read; if we can't, that's a real
			// failure they should see — surface it instead of silently
			// returning success and leaving the inbox in a confusing
			// state. The body has already been computed; it's fine to
			// fail before writing it.
			writeError(w, http.StatusInternalServerError, "io",
				fmt.Sprintf("failed to mark message %d as read: %v", id, err))
			return
		}
	}
	groupName := ""
	if g, _ := groupByID(m.GroupID); g != nil {
		groupName = g.Name
	}
	resp := map[string]any{
		"id":         m.ID,
		"from":       m.FromConv,
		"from_agent": m.FromAgent,
		"from_title": agent.TitleFor(m.FromConv),
		"to":         m.ToConv,
		"to_agent":   m.ToAgent,
		"group":      groupName,
		"subject":    m.Subject,
		"body":       m.Body,
		"created_at": m.CreatedAt.Format(time.RFC3339),
		// Reply-To is the conv-id to address when replying. Same as
		// `from` today; broken out so clients have an obvious affordance
		// and so we can support distinct reply-to addresses later
		// without breaking the wire format.
		"reply_to": m.FromConv,
		// Reply-Cmd is a ready-to-paste shell command for the human-friendly
		// case. Agents in skills should prefer the `agent reply` command,
		// which figures this out from the message ID.
		"reply_cmd": fmt.Sprintf("tclaude agent reply %d \"<your reply body>\"", m.ID),
	}
	// Original-To: non-empty when this message was redirected by the
	// succession-aware send path — the sender addressed a superseded
	// conv-id and the daemon walked the chain to the live successor
	// (this row's to_conv). Surface in the response so `inbox read`
	// can render an `Original-To:` header line.
	if m.OriginalToConv != "" {
		resp["original_to_conv"] = m.OriginalToConv
	}
	// Email-style audience (schema v18). Each recipient row carries the
	// same arrays so any reader can render "To: ...; CC: ..." identically.
	// Decorated with conv titles when known so the receiver sees friendly
	// names alongside the conv-ids.
	if len(m.ToRecipients) > 0 {
		resp["to_recipients"] = decorateRecipients(m.ToRecipients, m.ToRecipientAgents)
	}
	if len(m.CcRecipients) > 0 {
		resp["cc_recipients"] = decorateRecipients(m.CcRecipients, m.CcRecipientAgents)
	}
	// In-Reply-To: only set on threaded messages so the renderer can
	// hide the header for top-of-thread messages.
	if m.ParentID > 0 {
		resp["in_reply_to"] = m.ParentID
		// Walk one step up so the reader can see the subject of the
		// parent without an extra round-trip. Best-effort: a parent
		// that's been pruned just yields no parent_subject.
		if parent, err := db.GetAgentMessage(m.ParentID); err == nil && parent != nil {
			resp["parent_subject"] = parent.Subject
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- /v1/inbox ---

// inboxItem is the row shape returned by /v1/inbox. From/FromShort
// are populated for received messages (the inbox view); To/ToShort
// + Delivered are populated for sent messages (the outbox view, when
// ?outbox=1 is set). Unused fields omit themselves via omitempty.
type inboxItem struct {
	ID        int64  `json:"id"`
	From      string `json:"from,omitempty"`
	FromShort string `json:"from_short,omitempty"`
	To        string `json:"to,omitempty"`
	ToShort   string `json:"to_short,omitempty"`
	Group     string `json:"group"`
	Subject   string `json:"subject,omitempty"`
	Preview   string `json:"preview,omitempty"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
	Delivered bool   `json:"delivered,omitempty"`
	ParentID  int64  `json:"parent_id,omitempty"`
}

func handleInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// Operator view: when X-Tclaude-Target-Conv is set, the caller
	// reads someone else's inbox (gated by agent.inbox-watch slug or
	// group ownership). Without the header, returns the caller's own
	// inbox just as before.
	myID, _, ok := requireInboxAccess(w, r)
	if !ok {
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	unreadOnly := r.URL.Query().Get("unread") == "1" || r.URL.Query().Get("unread") == "true"
	outbox := r.URL.Query().Get("outbox") == "1" || r.URL.Query().Get("outbox") == "true"

	var msgs []*db.AgentMessage
	var err error
	if outbox {
		msgs, err = outboxForCaller(myID, limit)
	} else {
		msgs, err = inboxForCaller(myID, limit)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	groupNames := map[int64]string{}
	if gs, err := db.ListAgentGroups(); err == nil {
		for _, g := range gs {
			groupNames[g.ID] = g.Name
		}
	}
	out := make([]inboxItem, 0, len(msgs))
	for _, m := range msgs {
		if unreadOnly && !m.ReadAt.IsZero() {
			continue
		}
		item := inboxItem{
			ID:        m.ID,
			Group:     groupNames[m.GroupID],
			Subject:   m.Subject,
			Preview:   bodyPreview(m.Body),
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
			Read:      !m.ReadAt.IsZero(),
			ParentID:  m.ParentID,
		}
		if outbox {
			item.To = m.ToConv
			item.ToShort = agent.ShortAgentID(m.ToAgent, m.ToConv)
			item.Delivered = !m.DeliveredAt.IsZero()
		} else {
			item.From = m.FromConv
			item.FromShort = agent.ShortAgentID(m.FromAgent, m.FromConv)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- /v1/inbox/prune ---
//
// POST { "older_than_seconds": <int>, "read_only": <bool> } returns
// { "deleted": <int> } and removes agent_messages rows older than
// that the caller is the sender or recipient of. read_only restricts
// to messages the recipient has read.
//
// We take the cutoff as a number of seconds from the daemon's "now"
// rather than an absolute timestamp so the CLI can stay simple
// (parse the duration locally, send the result) and the daemon
// never has to deal with parsing day/week suffixes.
func handleInboxPrune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	myID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	var req struct {
		OlderThanSeconds int64 `json:"older_than_seconds"`
		ReadOnly         bool  `json:"read_only"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if req.OlderThanSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "older_than_seconds must be positive")
		return
	}
	cutoff := time.Now().Add(-time.Duration(req.OlderThanSeconds) * time.Second)
	deleted, err := pruneMessagesForCaller(myID, cutoff, req.ReadOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

func bodyPreview(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// recipientLine pairs a conv-id with the friendly label resolved for it
// (the conv-index title, else empty) and the recipient's stable agent_id
// (JOH-27 PR3b-2; empty when the conv is not an enrolled agent). Returned
// as part of /v1/messages/{id} so `inbox read` can render the To:/CC:
// audience as "alice (agt_xxxxxxxx)" without a second round-trip.
type recipientLine struct {
	ConvID  string `json:"conv_id"`
	AgentID string `json:"agent_id,omitempty"`
	Title   string `json:"title,omitempty"`
}

// decorateRecipients turns the stored audience arrays into a labelled list.
// convs is the conv-id array (to_recipients / cc_recipients); agents is the
// 1:1 stable-agent_id companion (to_recipient_agents / cc_recipient_agents,
// JOH-284). Each line prefers the STORED agent_id — which survives the
// recipient's conv generation being pruned — and falls back to a live conv→agent
// lookup only where the stored id is empty (a legacy pre-v79 row the backfill
// missed, or a genuine non-actor recipient). Title is still resolved from the
// conv (best-effort display); a conv without an index row just leaves it blank.
func decorateRecipients(convs, agents []string) []recipientLine {
	out := make([]recipientLine, 0, len(convs))
	for i, id := range convs {
		out = append(out, recipientLine{
			ConvID:  id,
			AgentID: storedAgentOrResolve(agents, i, id),
			Title:   agent.TitleFor(id),
		})
	}
	return out
}

// storedAgentOrResolve returns the persisted agent_id at index i of the
// companion array when present and non-empty, otherwise resolves convID→agent
// live. This is the read-path fallback that keeps legacy rows (and non-actor
// recipients) working while preferring the stored, pruning-immune id.
func storedAgentOrResolve(agents []string, i int, convID string) string {
	if i < len(agents) && agents[i] != "" {
		return agents[i]
	}
	return peerAgentID(convID)
}

func groupByID(id int64) (*db.AgentGroup, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, nil
}

// --- /v1/groups (GET = anyone, POST = human only) ---

type groupSummary struct {
	Name    string `json:"name"`
	Descr   string `json:"descr,omitempty"`
	Members int    `json:"members"`
	Online  int    `json:"online"`
	// MaxMembers is the group's hard member cap (agent_groups.max_members);
	// 0 = unlimited. A spawn that would exceed it is refused.
	MaxMembers int `json:"max_members,omitempty"`
	// DefaultProfile is the name of the spawn profile whose launch fields fill
	// blank spawn fields for this group's agents (JOH-210); "" = none. It is
	// the spawn default's single source — the vestigial per-group
	// default_model was dropped (JOH-220).
	DefaultProfile string `json:"default_profile,omitempty"`
	Archived       bool   `json:"archived,omitempty"`
	// NotifyMuted flags a group whose OS notifications are switched
	// off (agent_groups.notify_enabled = false). omitempty: only the
	// exceptional muted state is serialized.
	NotifyMuted bool `json:"notify_muted,omitempty"`
	// RemoteControlPolicy is the group's remote-control policy (JOH-262):
	// "inherit" (defer to the spawn profile), "optin" (force Remote Access on)
	// or "deny" (force it off). Always serialized (the canonical token) so a
	// consumer never has to guess between "absent" and "inherit".
	RemoteControlPolicy string `json:"remote_control_policy"`
	// Mission and SourceTemplate are the deploy provenance (JOH-245) carried
	// on the group row. They are what tells a plain group apart from a deployed
	// task force: `tclaude agent task-force ls` (JOH-346) filters on a
	// non-empty SourceTemplate and shows the truncated mission. omitempty — a
	// hand-built group carries neither, and plain `groups ls` ignores them.
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
}

// isConvOnline reports whether any tmux session registered for this conv-id
// is currently alive. Same alive-check `nudgeIfAlive` uses for delivery.
//
// Single-target callers (delivery, lifecycle, reaper) use this — one
// conv at a time, one `tmux has-session` subprocess at most. For
// snapshot-shaped callers iterating many convs, use isConvOnlineIn
// with a pre-fetched alive set: O(N) subprocesses collapses to one.
func isConvOnline(convID string) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return true
		}
	}
	return false
}

// isConvOnlineIn is the snapshot-shaped variant of isConvOnline. It
// takes a pre-fetched alive set (e.g. from clcommon.Default.ListSessions
// at the top of an HTTP handler) and does the per-row check via map
// lookup instead of per-row subprocess. Callers MUST fetch the set
// once per request and pass the SAME map to every call — passing nil
// (or fetching afresh per call) defeats the purpose and is the worst
// of both worlds.
func isConvOnlineIn(convID string, alive map[string]struct{}) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		if _, ok := alive[c.TmuxSession]; ok {
			return true
		}
	}
	return false
}

func handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Anyone (token or not) can list groups. By default archived
		// groups are filtered out — they're soft-deleted and rarely
		// belong in a default listing. Pass `?archived=1` (any non-empty
		// truthy value) to include them; the CLI surfaces this via
		// `groups ls --archived`.
		groups, err := db.ListAgentGroups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		showArchived := isTruthy(r.URL.Query().Get("archived"))
		// One tmux ls for the listing — per-member online checks below
		// are map lookups against this snapshot.
		aliveSessions, _ := session.LiveTmuxSessions()
		out := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			if !showArchived && g.IsArchived() {
				continue
			}
			members, _ := db.ListAgentGroupMembers(g.ID)
			online := 0
			for _, m := range members {
				if isConvOnlineIn(m.ConvID, aliveSessions) {
					online++
				}
			}
			out = append(out, groupSummary{
				Name:                g.Name,
				Descr:               g.Descr,
				Members:             len(members),
				Online:              online,
				MaxMembers:          g.MaxMembers,
				DefaultProfile:      g.DefaultProfile,
				Archived:            g.IsArchived(),
				NotifyMuted:         !g.NotifyEnabled,
				RemoteControlPolicy: remoteControlPolicyToWire(g.RemoteControl),
				Mission:             g.Mission,
				SourceTemplate:      g.SourceTemplate,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		creator, ok := requirePermission(w, r, PermGroupsCreate)
		if !ok {
			return
		}
		var body struct {
			Name           string `json:"name"`
			Descr          string `json:"descr,omitempty"`
			DefaultCwd     string `json:"default_cwd,omitempty"`
			DefaultContext string `json:"default_context,omitempty"`
			// DefaultProfile names the spawn profile (JOH-210) whose launch
			// fields fill blank spawn fields for this group's agents. "" = none.
			DefaultProfile string `json:"default_profile,omitempty"`
			// MaxMembers is the group's hard member cap; 0 = unlimited.
			// A negative value is clamped to 0 by db.SetAgentGroupMaxMembers.
			MaxMembers int `json:"max_members,omitempty"`
			// RemoteControlPolicy is the group's remote-control policy that
			// overrides a spawn profile's remote-control default (JOH-262):
			// "" / "inherit" (defer to the profile), "optin" (force on), "deny"
			// (force off).
			RemoteControlPolicy string `json:"remote_control_policy,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		// validateGroupName is the same guard rename and clone already
		// apply — it rejects empty names, embedded slashes (which would
		// break the /v1/groups/{name}/... and /api/groups/{name}/...
		// route dispatch), control characters, and stray whitespace.
		// Create historically skipped it, which let a slash-named group
		// through and made every later sub-route on that group
		// unroutable.
		if err := validateGroupName(body.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		// Validate the optional default cwd + startup context up front,
		// before any DB write, so a bad value fails cleanly without
		// leaving a half-configured group behind.
		groupCwd, err := resolveGroupDefaultCwd(body.DefaultCwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
		groupContext, err := normalizeGroupContext(body.DefaultContext)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		// The default profile (JOH-210) replaces the Claude-only default_model
		// gate: validate only that the referenced profile exists — its launch
		// fields were already validated against their own harness at save, so a
		// group can default its team onto a Codex profile without the
		// Claude-only model validator rejecting it (#343). "" = no default.
		groupProfile, err := resolveGroupDefaultProfileName(body.DefaultProfile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
			return
		}
		// Validate the optional remote-control policy up front (JOH-262); a bad
		// token fails cleanly before any DB write. "" / "inherit" → nil (no
		// override, the column default).
		groupRemoteControl, err := parseRemoteControlPolicy(body.RemoteControlPolicy)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control_policy", err.Error())
			return
		}
		// Fold newlines out of the description on the create path too,
		// not just update — the one-line header invariant must hold
		// however the descr first arrives (see normalizeGroupDescr).
		groupDescr := normalizeGroupDescr(body.Descr)
		if existing, _ := db.GetAgentGroupByName(body.Name); existing != nil {
			writeError(w, http.StatusConflict, "exists", "group already exists")
			return
		}
		id, err := db.CreateAgentGroup(body.Name, groupDescr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Apply the default cwd + startup context as post-create updates
		// — keeps CreateAgentGroup's signature untouched (it's shared with
		// the clone path and flow-test helpers). A failure here is logged
		// but doesn't unwind the create; the human can set it later via
		// `groups set-context` or the dashboard.
		if groupCwd != "" {
			if _, err := db.SetAgentGroupDefaultCwd(body.Name, groupCwd); err != nil {
				slog.Warn("groups create: failed to set default cwd",
					"group", body.Name, "error", err)
			}
		}
		if groupContext != "" {
			if _, err := db.SetAgentGroupDefaultContext(body.Name, groupContext); err != nil {
				slog.Warn("groups create: failed to set default context",
					"group", body.Name, "error", err)
			}
		}
		if groupProfile != "" {
			if _, err := db.SetAgentGroupDefaultProfile(body.Name, groupProfile); err != nil {
				slog.Warn("groups create: failed to set default profile",
					"group", body.Name, "error", err)
			}
		}
		if body.MaxMembers != 0 {
			if _, err := db.SetAgentGroupMaxMembers(body.Name, body.MaxMembers); err != nil {
				slog.Warn("groups create: failed to set max members",
					"group", body.Name, "error", err)
			}
		}
		// Only write a non-inherit policy; "inherit" (nil) is the column default,
		// so a create that didn't ask for a policy leaves the group deferring to
		// the profile (JOH-262).
		if groupRemoteControl != nil {
			if _, err := db.SetAgentGroupRemoteControl(body.Name, groupRemoteControl); err != nil {
				slog.Warn("groups create: failed to set remote-control policy",
					"group", body.Name, "error", err)
			}
		}
		// Auto-grant ownership to the creator. Skipped for the human
		// path (creator == "") since humans don't have a conv-id; the
		// human is implicitly above the permission system anyway.
		// Failure here is logged but doesn't unwind the create — the
		// human can grant ownership manually if needed.
		if creator != "" {
			if err := db.AddAgentGroupOwner(id, creator, auditedCaller(creator, PermGroupsCreate)); err != nil {
				slog.Warn("groups create: auto-grant owner failed",
					"group", body.Name, "creator", creator, "error", err)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": body.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// --- /v1/groups/{name}* routes ---

// registerV1GroupRoutes wires the SO_PEERCRED-authed /v1/groups/{name}
// endpoints onto the daemon mux as Go 1.22 method+pattern routes:
//
//	POST   /v1/groups/{name}/stop            → stop every member
//	POST   /v1/groups/{name}/resume          → resume every member
//	POST   /v1/groups/{name}/retire          → retire every other member
//	POST   /v1/groups/{name}/spawn           → spawn a session into the group
//	POST   /v1/groups/{name}/archive         → soft-delete (archive)
//	POST   /v1/groups/{name}/unarchive       → restore an archived group
//	PUT    /v1/groups/{name}/parent          → nest under another group (body: {parent}); "" clears
//	POST   /v1/groups/{name}/rename          → rename (body: {new_name})
//	POST   /v1/groups/{name}/clone           → clone the group
//	GET    /v1/groups/{name}/owners          → list owners
//	POST   /v1/groups/{name}/owners          → grant owner
//	DELETE /v1/groups/{name}/owners/{conv}   → revoke owner
//	GET    /v1/groups/{name}/context         → list members' context-window state
//	GET    /v1/groups/{name}/members         → list members
//	POST   /v1/groups/{name}/members         → add member
//	PATCH  /v1/groups/{name}/members/{conv}  → update role/descr
//	DELETE /v1/groups/{name}/members/{conv}  → remove member
//	       /v1/groups/{name}/links[/{id}]    → link CRUD (own method dispatch)
//	PATCH  /v1/groups/{name}                 → update settings
//	DELETE /v1/groups/{name}                 → delete group
//
// The {name} / {conv} / {id} wildcards are matched and percent-decoded
// by the mux (read via r.PathValue), replacing the old hand-rolled
// TrimPrefix + SplitN dispatch. That manual parse split on r.URL.Path
// — already percent-decoded — so a group name with an embedded slash
// was re-split into bogus path segments and the route was lost. A
// {name} wildcard matches one segment of the *escaped* path, so the
// slash survives intact.
func registerV1GroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups/{name}/stop", v1GroupRoute(handleGroupStop))
	mux.HandleFunc("POST /v1/groups/{name}/resume", v1GroupRoute(handleGroupResume))
	mux.HandleFunc("POST /v1/groups/{name}/retire", v1GroupRoute(handleGroupRetire))
	mux.HandleFunc("POST /v1/groups/{name}/spawn", v1GroupRoute(handleGroupSpawn))
	mux.HandleFunc("POST /v1/groups/{name}/archive", v1GroupRoute(handleGroupArchive))
	mux.HandleFunc("POST /v1/groups/{name}/unarchive", v1GroupRoute(handleGroupUnarchive))
	mux.HandleFunc("PUT /v1/groups/{name}/parent", v1GroupRoute(handleGroupParent))
	mux.HandleFunc("POST /v1/groups/{name}/rename", v1GroupRoute(handleGroupRename))
	mux.HandleFunc("POST /v1/groups/{name}/clone", v1GroupRoute(handleGroupClone))
	mux.HandleFunc("GET /v1/groups/{name}/export", v1GroupRoute(handleGroupExport))

	mux.HandleFunc("GET /v1/groups/{name}/owners", v1GroupRoute(handleGroupOwnersList))
	mux.HandleFunc("POST /v1/groups/{name}/owners", v1GroupRoute(handleGroupOwnersAdd))
	mux.HandleFunc("DELETE /v1/groups/{name}/owners/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupOwnersRemove(w, r, g, r.PathValue("conv"))
	}))

	mux.HandleFunc("GET /v1/groups/{name}/context", v1GroupRoute(handleGroupContext))

	// Advisory process runtime (JOH-242): read the current phase (open),
	// advance to the next / a named phase (gated on process.advance, owner-pass).
	mux.HandleFunc("GET /v1/groups/{name}/process", v1GroupRoute(handleGroupProcessGet))
	mux.HandleFunc("POST /v1/groups/{name}/process/advance", v1GroupRoute(handleGroupProcessAdvance))

	// Staged-spawn choreography status (JOH-244): read the group's pending
	// waves (open, read-only). 404 when the deploy has no pending waves.
	mux.HandleFunc("GET /v1/groups/{name}/waves", v1GroupRoute(handleGroupWavesGet))

	// Re-brief a deployed force (JOH-247): re-deliver the source template's
	// work pattern to the live roster with the group's mission interpolated.
	// Gated on templates.instantiate + owner-pass (see handleGroupRebrief).
	mux.HandleFunc("POST /v1/groups/{name}/rebrief", v1GroupRoute(handleGroupRebrief))

	// Stand down a task force (JOH-345): retire the roster + sweep the
	// deploy-seeded rhythms and pending waves, keeping the group as a dormant
	// record. The mirror of `task-force deploy`. Gated on groups.retire +
	// owner-pass (see handleGroupStandDown).
	mux.HandleFunc("POST /v1/groups/{name}/stand-down", v1GroupRoute(handleGroupStandDown))

	mux.HandleFunc("GET /v1/groups/{name}/members", v1GroupRoute(handleGroupMembersList))
	mux.HandleFunc("POST /v1/groups/{name}/members", v1GroupRoute(handleGroupMembersAdd))
	mux.HandleFunc("PATCH /v1/groups/{name}/members/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersUpdate(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("DELETE /v1/groups/{name}/members/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersRemove(w, r, g, r.PathValue("conv"))
	}))

	// handleGroupLinks dispatches its own methods, so the two link
	// patterns are registered without a method in the pattern.
	mux.HandleFunc("/v1/groups/{name}/links", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupLinks(w, r, g, nil)
	}))
	mux.HandleFunc("/v1/groups/{name}/links/{id}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupLinks(w, r, g, []string{r.PathValue("id")})
	}))

	mux.HandleFunc("PATCH /v1/groups/{name}", v1GroupRoute(handleGroupUpdate))
	mux.HandleFunc("DELETE /v1/groups/{name}", v1GroupRoute(handleGroupDelete))
}

// v1GroupRoute adapts a group-scoped /v1 handler into an
// http.HandlerFunc. It resolves the {name} path wildcard to a group row
// and replies with a 404 error envelope when the group is missing; the
// per-handler permission gate (SO_PEERCRED identity → requirePermission)
// still runs inside fn, exactly as before.
func v1GroupRoute(fn func(http.ResponseWriter, *http.Request, *db.AgentGroup)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "missing group name")
			return
		}
		g, err := db.GetAgentGroupByName(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if g == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		fn(w, r, g)
	}
}

// handleGroupDelete deletes a group. Permission slug: groups.rm
// (default human-only). db.DeleteAgentGroup fails with a constraint
// error if the group still has blocking references.
func handleGroupDelete(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsRm); !ok {
		return
	}
	if err := db.DeleteAgentGroup(g.Name); err != nil {
		writeError(w, http.StatusConflict, "constraint", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxGroupContextBytes caps a group's startup context. The context is
// folded into the startup briefing delivered to each spawned agent's
// inbox; 16 KiB is comfortably more than any reasonable block of
// startup guidance while keeping the briefing message bounded.
const maxGroupContextBytes = 16 * 1024

// normalizeGroupContext prepares a group startup context for storage:
// CRLF / lone-CR line endings are folded to LF so the briefing
// renders consistently regardless of where the human typed it, and
// the result is rejected when it exceeds maxGroupContextBytes.
func normalizeGroupContext(s string) (string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if len(s) > maxGroupContextBytes {
		return "", fmt.Errorf("group context too long: %d bytes (max %d)", len(s), maxGroupContextBytes)
	}
	return s, nil
}

// resolveGroupDefaultProfileName validates a requested group default-profile
// reference (JOH-210): it trims the name and, when non-empty, requires a spawn
// profile by that name to exist — there is no DB-level foreign key, so the
// referential check lives here. Empty is valid and clears the group's default.
// Returns the trimmed name to store.
func resolveGroupDefaultProfileName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	p, err := db.GetSpawnProfile(name)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("no spawn profile named %q", name)
	}
	return name, nil
}

// normalizeGroupDescr prepares a group description for storage. The
// descr is a one-line label rendered inline in the dashboard's group
// header, so any embedded CR / LF is folded to a single space and the
// result is trimmed — a raw API caller can no longer wedge a newline
// into a header that has nowhere to put it. Empty stays empty.
//
// Applied on every write path — create, update, and clone — so the
// one-line invariant holds regardless of how the descr was set, not
// just when it was last edited.
func normalizeGroupDescr(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// handleGroupUpdate patches mutable group-level settings:
//
//   - descr — the group's own one-line description, shown next to the
//     group name on the dashboard. Distinct from a per-member descr.
//   - default_cwd — the working directory pre-filled into the spawn
//     form (and substituted server-side by handleGroupSpawn when a
//     spawn request leaves cwd blank).
//   - default_context — a block of shared startup guidance delivered
//     to the inbox of agents spawned into the group (see
//     handleGroupSpawn).
//   - default_profile — the spawn profile (JOH-210) whose launch fields
//     fill blank spawn fields server-side (see executeSpawn /
//     applyDefaultProfile), so a group can default its whole team onto a
//     given harness+model+… without the Claude-only default_model gate.
//     Validated only for existence; the profile carries its own validated
//     launch fields. Replaces the retired default_model setting (#343).
//   - max_members — the group's hard member cap (0 = unlimited); a
//     spawn that would exceed it is refused by the spawn-guardrail
//     layer. See checkSpawnGuardrails.
//
// Partial-update contract, matching handleGroupMembersUpdate: only
// fields present (non-nil) in the body are touched. descr / default_cwd
// / default_context / default_profile are *string so a caller can clear
// any of them by sending "" — distinct from omitting it; max_members is
// *int and clears to "unlimited" with 0. An empty body (no field) is a
// 400.
//
// Permission: groups.rename. Setting a group's description / default
// cwd / context / profile / member cap is the same class of human-curated
// group config as renaming it (the blast radius is a dashboard label /
// UI prefill / spawn-time injection / spawn refusal, strictly lower
// than a rename), so it rides the existing slug rather than minting a
// new one. Default human-only.
func handleGroupUpdate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsRename); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Descr          *string `json:"descr,omitempty"`
		DefaultCwd     *string `json:"default_cwd,omitempty"`
		DefaultContext *string `json:"default_context,omitempty"`
		// DefaultProfile names the spawn profile (JOH-210) whose launch fields
		// fill blank spawn fields for this group's agents; "" clears it. *string
		// so a caller can clear it by sending "" — distinct from omitting it.
		DefaultProfile *string `json:"default_profile,omitempty"`
		// MaxMembers is the group's hard member cap; 0 = unlimited. A
		// negative value is clamped to 0 by db.SetAgentGroupMaxMembers.
		MaxMembers *int `json:"max_members,omitempty"`
		// NotifyEnabled is the group's OS-notification switch; false
		// mutes state-transition notifications for every member agent
		// (a per-agent 'on' pref still overrides).
		NotifyEnabled *bool `json:"notify_enabled,omitempty"`
		// RemoteControlPolicy is the group's remote-control policy that overrides
		// a spawn profile's remote-control default (JOH-262): "inherit" (defer to
		// the profile), "optin" (force on) or "deny" (force off). *string so a
		// caller can change it without touching the other fields; omitting it
		// leaves the policy unchanged.
		RemoteControlPolicy *string `json:"remote_control_policy,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Descr == nil && body.DefaultCwd == nil && body.DefaultContext == nil && body.DefaultProfile == nil && body.MaxMembers == nil && body.NotifyEnabled == nil && body.RemoteControlPolicy == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"nothing to update (expected descr, default_cwd, default_context, default_profile, max_members, notify_enabled and/or remote_control_policy)")
		return
	}
	resp := map[string]any{"group": g.Name}

	if body.Descr != nil {
		// The group descr is a one-line label. Fold any embedded
		// newline (an API caller could send one — the CLI positional
		// and the dashboard's <input type=text> cannot) to a space so
		// it never breaks the single-line dashboard header. Empty
		// stays empty — that clears the description.
		descr := normalizeGroupDescr(*body.Descr)
		n, err := db.SetAgentGroupDescr(g.Name, descr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Zero rows: the group was renamed or deleted between the
		// dispatcher's lookup and this update. Report not_found rather
		// than a misleading 200.
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["descr"] = descr
	}

	if body.DefaultCwd != nil {
		// Normalise + validate: expand "~", require an absolute path (a
		// relative default would resolve against the daemon's cwd at
		// spawn time, which is meaningless). Empty stays empty — clears it.
		cwd, err := resolveGroupDefaultCwd(*body.DefaultCwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
		n, err := db.SetAgentGroupDefaultCwd(g.Name, cwd)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Zero rows: the group was renamed or deleted between the
		// dispatcher's lookup and this update. Report not_found rather
		// than a misleading 200.
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_cwd"] = cwd
	}

	if body.DefaultContext != nil {
		// Fold CRLF → LF and enforce the size cap. Empty stays empty —
		// that clears the group context.
		ctx, err := normalizeGroupContext(*body.DefaultContext)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		n, err := db.SetAgentGroupDefaultContext(g.Name, ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_context"] = ctx
	}

	if body.DefaultProfile != nil {
		// Validate only that the referenced profile exists (its launch fields
		// were validated against their own harness at save) — the harness-correct
		// replacement for the retired Claude-only default_model gate (#343).
		// Empty stays empty — that clears the group default.
		profile, err := resolveGroupDefaultProfileName(*body.DefaultProfile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
			return
		}
		n, err := db.SetAgentGroupDefaultProfile(g.Name, profile)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_profile"] = profile
	}

	if body.MaxMembers != nil {
		// db.SetAgentGroupMaxMembers clamps a negative value to 0
		// (unlimited) rather than rejecting it, so a careless caller
		// can never wedge a group with an impossible cap.
		n, err := db.SetAgentGroupMaxMembers(g.Name, *body.MaxMembers)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		stored := *body.MaxMembers
		if stored < 0 {
			stored = 0
		}
		resp["max_members"] = stored
	}

	if body.NotifyEnabled != nil {
		n, err := db.SetAgentGroupNotifyEnabled(g.Name, *body.NotifyEnabled)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["notify_enabled"] = *body.NotifyEnabled
	}

	if body.RemoteControlPolicy != nil {
		policy, err := parseRemoteControlPolicy(*body.RemoteControlPolicy)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control_policy", err.Error())
			return
		}
		n, err := db.SetAgentGroupRemoteControl(g.Name, policy)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["remote_control_policy"] = remoteControlPolicyToWire(policy)
	}

	writeJSON(w, http.StatusOK, resp)
}

// parseRemoteControlPolicy maps a group remote-control policy wire token to the
// tri-state *bool stored on the group (JOH-262): "" / "inherit" → nil (defer to
// the spawn profile's default), "optin" / "on" → true (force Remote Access on),
// "deny" / "off" → false (force it off). Any other token is an error so a typo
// can't silently land as "inherit". Tolerant of a few synonyms so the CLI and
// dashboard can speak the operator's words.
func parseRemoteControlPolicy(s string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "inherit", "unset", "default":
		return nil, nil
	case "optin", "opt-in", "on", "enable", "enabled", "true":
		v := true
		return &v, nil
	case "deny", "off", "disable", "disabled", "false":
		v := false
		return &v, nil
	default:
		return nil, fmt.Errorf("invalid remote_control_policy %q (want inherit, optin, or deny)", s)
	}
}

// remoteControlPolicyToWire is the inverse of parseRemoteControlPolicy: it maps
// the stored tri-state *bool back to its canonical wire token for read surfaces
// (group GET, dashboard payload). nil → "inherit", true → "optin", false → "deny".
func remoteControlPolicyToWire(p *bool) string {
	switch {
	case p == nil:
		return "inherit"
	case *p:
		return "optin"
	default:
		return "deny"
	}
}

type memberJSON struct {
	// AgentID is the member's stable actor key — the canonical ID the
	// agent CLI leads with; ConvID is the live generation behind it. (The
	// dashboard still keys on conv_id — a separate follow-up.)
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// CreatedAt is the conversation's creation timestamp (RFC3339 — the
	// first .jsonl event's time), empty when unknown. The dashboard
	// renders it as a relative "Age", and it is the default sort key
	// (newest first).
	CreatedAt string `json:"created_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Descr     string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	Online bool `json:"online"`
	Owner  bool `json:"owner,omitempty"`
}

// sortMembersByAge orders a group-member listing newest-first by
// conversation creation time (RFC3339 strings, which sort lexically =
// chronologically) — the default ordering for every group listing,
// shared by the CLI (`tclaude agent groups members`, which renders the
// JSON below) and the browser dashboard, whose Age column shows this and
// whose client-side column sort treats it as the "natural" order it
// falls back to when no column is active. Blank/unknown creation times
// sort last so a freshly-spawned, not-yet-indexed agent never crowds the
// top; conv_id breaks ties so the order is deterministic — the previous
// joined_at order left owner-only rows, appended from a map, in random
// iteration order.
func sortMembersByAge[T any](items []T, created func(T) string, convID func(T) string) {
	sort.SliceStable(items, func(i, j int) bool {
		ci, cj := created(items[i]), created(items[j])
		if (ci == "") != (cj == "") {
			return ci != "" // known creation times before unknown
		}
		if ci != cj {
			return ci > cj // newest first
		}
		return convID(items[i]) < convID(items[j])
	})
}

func handleGroupMembersList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// One tmux ls for the listing — per-member online checks below are
	// map lookups against this snapshot.
	aliveSessions, _ := session.LiveTmuxSessions()
	// Pre-load the owner set so we can tag any members who are also
	// owners. Distinct-from-members owners are emitted as their own
	// rows below so the list stays comprehensive.
	ownerSet := map[string]bool{}
	if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
		for _, o := range owners {
			ownerSet[o.ConvID] = true
		}
	}
	memberSet := map[string]bool{}
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		memberSet[m.ConvID] = true
		out = append(out, memberJSON{
			AgentID:           peerAgentID(m.ConvID),
			ConvID:            m.ConvID,
			Title:             agent.FreshTitle(m.ConvID),
			CreatedAt:         agent.FreshCreated(m.ConvID),
			Role:              m.Role,
			Descr:             m.Descr,
			agentLocationView: locationView(m.ConvID),
			Online:            isConvOnlineIn(m.ConvID, aliveSessions),
			Owner:             ownerSet[m.ConvID],
		})
	}
	// Surface owners who aren't members so the list is comprehensive.
	// They get an "owner" role tag and no descr (that is a
	// member-scoped field).
	for ownerConv := range ownerSet {
		if memberSet[ownerConv] {
			continue
		}
		out = append(out, memberJSON{
			AgentID:           peerAgentID(ownerConv),
			ConvID:            ownerConv,
			Title:             agent.FreshTitle(ownerConv),
			CreatedAt:         agent.FreshCreated(ownerConv),
			Role:              "owner",
			agentLocationView: locationView(ownerConv),
			Online:            isConvOnlineIn(ownerConv, aliveSessions),
			Owner:             true,
		})
	}
	sortMembersByAge(out,
		func(m memberJSON) string { return m.CreatedAt },
		func(m memberJSON) string { return m.ConvID })
	writeJSON(w, http.StatusOK, out)
}

// groupContextEntry is one member's context-window state in the
// group-wide listing (GET /v1/groups/{name}/context). HasSnapshot is
// false for a member whose statusline hook has never fired (a
// freshly-spawned agent pre-first-response, or one that never ran under
// tclaude) — the caller renders that as "—" rather than a misleading 0%.
// Model can be non-empty even when HasSnapshot is false: it's written on
// every statusline render (UpdateSessionModel), including the empty
// pre-first-response ones that carry no context figures.
type groupContextEntry struct {
	// AgentID is the member's stable actor key — the canonical ID the CLI
	// leads with; ConvID is the live generation behind it (kept as the
	// snapshot/hover). "" when the conv is not a known agent.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Role    string `json:"role,omitempty"`
	Online  bool   `json:"online"`
	// Status is the member's settled hook status (idle / working /
	// awaiting_* / exited …) — the SAME value stateForConvIn feeds the
	// dashboard, so a caller classifying force liveness (JOH-346's
	// task-force status) agrees with the dashboard force block's rollup:
	// offline → dead, online+idle → idle, anything else in flight →
	// working. Empty when no session row exists for the conv.
	Status            string  `json:"status,omitempty"`
	HasSnapshot       bool    `json:"has_snapshot"`
	ContextPct        float64 `json:"context_pct"`
	TokensInput       int64   `json:"tokens_input"`
	TokensOutput      int64   `json:"tokens_output"`
	ContextWindowSize int64   `json:"context_window_size"`
	Model             string  `json:"model,omitempty"`
}

// handleGroupContext returns the context-window state of every member of
// a group in one read — the lead-watching-workers view, so a manager can
// spot anyone approaching their context limit at a glance. Read-only
// (GET only). Gated like the per-target read: the human always passes,
// an agent passes with the agent.context-info slug or by owning this
// group (owner bypass — the lead is normally the owner). All members'
// snapshots ride on the same already-persisted sessions rows the
// dashboard reads; no new data source.
func handleGroupContext(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if _, ok := requireGroupContextAccess(w, r, g); !ok {
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// One tmux ls for the whole listing; the per-member snapshot read
	// resolves liveness against this set, not a per-row subprocess.
	aliveSessions, _ := session.LiveTmuxSessions()
	out := make([]groupContextEntry, 0, len(members))
	for _, m := range members {
		// One read per member through stateForConvIn — the SAME reader the
		// dashboard snapshot uses, so this endpoint's context figures AND its
		// settled status (idle / working / awaiting_* / exited, incl. the
		// subagent-idle settle) agree with the dashboard force block that
		// classifies liveness on them (JOH-346). A populated snapshot implies
		// a session row, so has_snapshot collapses to "any real context figure"
		// — the separate hasSession gate was redundant.
		st := stateForConvIn(m.ConvID, aliveSessions)
		out = append(out, groupContextEntry{
			AgentID:           peerAgentID(m.ConvID),
			ConvID:            m.ConvID,
			Title:             agent.FreshTitle(m.ConvID),
			Role:              m.Role,
			Online:            isConvOnlineIn(m.ConvID, aliveSessions),
			Status:            st.Status,
			HasSnapshot:       agentStateHasSnapshot(st),
			ContextPct:        st.ContextPct,
			TokensInput:       st.TokensInput,
			TokensOutput:      st.TokensOutput,
			ContextWindowSize: st.ContextWindowSize,
			Model:             st.Model,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// snapshotPopulated reports whether a context snapshot carries any real
// figure — i.e. the statusline hook has fired at least once. An all-zero
// snapshot is the "not reported yet" sentinel (the same shape a missing
// row produces); distinguishing it lets the caller mark "hook never
// fired" separately from a genuine 0%.
func snapshotPopulated(s db.ContextSnapshot) bool {
	return s.ContextPct != 0 || s.TokensInput != 0 || s.TokensOutput != 0 || s.ContextWindowSize != 0
}

// agentStateHasSnapshot applies snapshotPopulated to the context figures an
// stateForConvIn read carries — true once the statusline hook has reported at
// least one real figure. A populated snapshot implies a live-or-past session
// row, so this needs no separate hasSession gate. Delegates to snapshotPopulated
// so the "is this real data" rule lives in exactly one place.
func agentStateHasSnapshot(s agentState) bool {
	return snapshotPopulated(db.ContextSnapshot{
		ContextPct:        s.ContextPct,
		TokensInput:       s.TokensInput,
		TokensOutput:      s.TokensOutput,
		ContextWindowSize: s.ContextWindowSize,
	})
}

// requireGroupContextAccess gates the group-wide context read. The human
// operator always passes. An agent passes if it holds the
// agent.context-info slug, or if it owns this group (the owner bypass) —
// but an explicit deny override is ALWAYS authoritative and suppresses
// the owner bypass, mirroring the universal precedence in
// resolvePermission / requireCrossAgentPermission (the owner bypass only
// fills the "undecided" gap). Unidentified / unconfirmed / unidentifiable
// callers are refused fail-closed. Returns the caller's conv-id ("" for
// humans) on success; on failure the error response is already written.
func requireGroupContextAccess(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) (string, bool) {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classUnidentified:
		writeUnidentified(w)
		return "", false
	case classHuman:
		return "", true
	case classAgentUnknown:
		writeAgentUnknown(w)
		return "", false
	case classUnconfirmed:
		writeUnconfirmed(w)
		return "", false
	case classAgent:
		// Confirmed agent — evaluate slug + owner bypass below.
	}
	switch resolvePermission(p.ConvID, PermAgentContextInfo) {
	case permAllow:
		return p.ConvID, true
	case permUndecided:
		// No grant/deny source — the owner bypass applies.
		if owns, err := db.IsAgentGroupOwner(g.ID, p.ConvID); err == nil && owns {
			return p.ConvID, true
		}
	case permDeny:
		// Explicit deny is authoritative — it suppresses the owner bypass.
	}
	writeError(w, http.StatusForbidden, "permission",
		fmt.Sprintf("caller is not granted %q and is not an owner of group %q "+
			"(be added as a group owner, or grant via `tclaude agent permissions grant %s %s`)",
			PermAgentContextInfo, g.Name, PermAgentContextInfo, short8(p.ConvID)))
	return "", false
}

type ownerJSON struct {
	// AgentID is the owner's stable actor key — the canonical ID the CLI
	// leads with; ConvID is the live generation behind it.
	AgentID   string `json:"agent_id,omitempty"`
	ConvID    string `json:"conv_id"`
	Title     string `json:"title"`
	Online    bool   `json:"online"`
	GrantedAt string `json:"granted_at,omitempty"`
	GrantedBy string `json:"granted_by,omitempty"`
}

// handleGroupOwnersList returns the owner set for the group. Owners
// can message members (and multicast) without being members of the
// group themselves.
func handleGroupOwnersList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	owners, err := db.ListAgentGroupOwners(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// One tmux ls for the listing — per-owner online checks below are
	// map lookups against this snapshot.
	aliveSessions, _ := session.LiveTmuxSessions()
	out := make([]ownerJSON, 0, len(owners))
	for _, o := range owners {
		entry := ownerJSON{
			AgentID: o.AgentID,
			ConvID:  o.ConvID,
			Title:   agent.FreshTitle(o.ConvID),
			Online:  isConvOnlineIn(o.ConvID, aliveSessions),
		}
		if !o.GrantedAt.IsZero() {
			entry.GrantedAt = o.GrantedAt.Format(time.RFC3339)
		}
		entry.GrantedBy = o.GrantedBy
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGroupOwnersAdd grants ownership of g to a conv. Permission-
// gated on groups.own (default human-only). The granted_by column
// records "" for human-issued grants and the agent's conv-id for
// agent-issued ones.
func handleGroupOwnersAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	grantedBy, ok := requirePermission(w, r, PermGroupsOwn)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Conv string `json:"conv"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Conv == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "conv is required")
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.AddAgentGroupOwner(g.ID, res.ConvID, auditedCaller(grantedBy, PermGroupsOwn)); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":    g.Name,
		"agent_id": peerAgentID(res.ConvID),
		"conv_id":  res.ConvID,
	})
}

// handleGroupOwnersRemove revokes ownership. 404 when convID wasn't
// an owner — distinct from "no such group" (which the dispatcher
// catches). Permission-gated on groups.own.
func handleGroupOwnersRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requirePermission(w, r, PermGroupsOwn); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	n, err := db.RemoveAgentGroupOwner(g.ID, res.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("conv %s is not an owner of %q", res.ConvID, g.Name))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleGroupMembersAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermMemberAdd); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Conv  string `json:"conv"`
		Role  string `json:"role,omitempty"`
		Descr string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Conv == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "conv is required")
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  res.ConvID,
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

// handleGroupMembersUpdate patches role/descr on an existing member.
// Only fields explicitly present in the request body are touched — pass
// `null` (or omit) to leave a field unchanged. Gated on member.redesignate.
func handleGroupMembersUpdate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requirePermission(w, r, PermMemberRedesignate); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Role  *string `json:"role,omitempty"`
		Descr *string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Role == nil && body.Descr == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "at least one of role/descr is required")
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	n, err := db.UpdateAgentGroupMember(g.ID, res.ConvID, body.Role, body.Descr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such member in group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

func handleGroupMembersRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requirePermission(w, r, PermMemberRemove); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.RemoveAgentGroupMember(g.ID, res.ConvID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
