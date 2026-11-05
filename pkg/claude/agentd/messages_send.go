package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

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
	// Attachments are files the CLI read on the sender's behalf, so a
	// sender can attach only what it can read. Group mail takes none.
	Attachments []proto.AttachmentPayload `json:"attachments,omitempty"`
}

// sendResp carries the result of either a direct send or a group
// fan-out. For direct messages the top-level fields (ID, Delivered)
// are populated and Recipients is nil. For multicast (To prefixed
// with "group:") ID/Delivered are zero values and Recipients lists
// one entry per non-sender member.
type sendResp struct {
	ID int64 `json:"id,omitempty"`
	// Queued is true once the durable inbox row is persisted. The send returns
	// before the best-effort nudge worker runs, so this does not promise a tmux
	// notification. Pending carries the regular-message backlog depth.
	Queued bool `json:"queued,omitempty"`
	// Pending is the recipient's unprocessed regular-message backlog at send
	// time, including this row. It is informational, never a failure signal.
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
	// EnvelopeID is set instead of MessageID for a remote (member@peer)
	// recipient: the message is queued in the federation outbox.
	EnvelopeID string `json:"envelope_id,omitempty"`
	// AgentID is the recipient's stable agent_id (JOH-27 PR3b-2), resolved
	// from ConvID at send time so the sender's receipt names each recipient
	// by the rotation-immune handle instead of a conv-id prefix. Empty when
	// the recipient is not (yet) an enrolled agent — the receipt then falls
	// back to the conv-id.
	AgentID   string `json:"agent_id,omitempty"`
	Title     string `json:"title,omitempty"`
	MessageID int64  `json:"message_id"`
	// Queued is true once this recipient's durable row is persisted — the
	// per-recipient analogue of sendResp.Queued. Pending is its unprocessed
	// regular-message backlog depth.
	Queued  bool `json:"queued,omitempty"`
	Pending int  `json:"pending,omitempty"`
	// QueueFull identifies a per-recipient backpressure rejection in a
	// multicast/CC response. Error carries the actionable retry hint and Limit
	// makes the overload threshold machine-readable to API callers.
	QueueFull bool   `json:"queue_full,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Error     string `json:"error,omitempty"`
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
func resolveMessageRouting(w http.ResponseWriter, r *http.Request, fromID, targetID string) (groupID int64, viaName string, ok bool) {
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
	if contextFreeResolution(resolvePermissionVerdictForRequest(r, fromID, PermMessageDirect)) != permAllow {
		if isLiveSteward(targetID) || db.IsExternalStewardConv(targetID) {
			writeError(w, http.StatusForbidden, "auth",
				fmt.Sprintf("you can answer the Steward with `tclaude agent reply <id>` but not write to it first; "+
					"that takes the %q permission or a shared group", PermMessageDirect))
			return 0, "", false
		}
		writeError(w, http.StatusForbidden, "auth",
			fmt.Sprintf("no shared group with %s and you do not own a group containing it; "+
				"messaging an agent outside your group requires the %q permission "+
				"(ask the human to grant it, or get a time-bounded grant via `tclaude agent sudo`)",
				short8(targetID), PermMessageDirect))
		return 0, "", false
	}
	if !spendSudoUse(w, r, fromID, PermMessageDirect, ActionContext{}) {
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
	if !resolveStewardAddresses(w, &req) {
		return
	}
	if group, peer, ok := splitFederatedGroup(req.To); ok {
		handleFederatedGroupSend(w, r, fromID, &req, group, peer)
		return
	}
	localCC, remoteCC := splitFederatedCC(req.Cc)
	if isFederatedAddress(strings.TrimSpace(req.To)) {
		req.To = strings.TrimSpace(req.To)
		if len(localCC) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "a remote message can only cc other remote (member@peer) recipients; message local agents separately")
			return
		}
		handleFederatedAgentSend(w, r, fromID, &req)
		return
	}
	if len(remoteCC) > 0 {
		handleSendWithRemoteCC(w, r, fromID, &req, localCC, remoteCC)
		return
	}
	dispatchSend(w, r, fromID, &req)
}

// dispatchSend validates and routes a send from fromID. It is the
// shared core behind two callers: POST /v1/messages (agent sender —
// identity is the connecting socket peer) and the dashboard's POST
// /api/message (human sender — identity is the From conv the human
// picked). Every authority check the send must clear lives below
// this point — the group member/owner gate inside handleMulticast,
// the shared-group / message.direct gate inside resolveMessageRouting
// — so neither caller can route around the gate.
func dispatchSend(w http.ResponseWriter, r *http.Request, fromID string, req *sendReq) {
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is empty")
		return
	}
	if err := validateFedAttachments(req.Attachments); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Group mail is text only, here as over federation.
	if len(req.Attachments) > 0 && strings.HasPrefix(req.To, multicastPrefix) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "attachments are not supported on group messages; message the agents directly")
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
	groupID, viaName, ok := resolveMessageRouting(w, r, fromID, headConv)
	if !ok {
		return
	}

	// Multi-recipient (--cc) path: one row per (To + each CC), each with
	// the same to_recipients / cc_recipients audience. CCs that resolve
	// ambiguously / not at all / aren't reachable surface as a 4xx so
	// the sender can fix the typo before any rows are written.
	if len(req.Cc) > 0 {
		handleMultiRecipient(w, r, fromID, finalConv, originalTo, groupID, viaName, req)
		return
	}

	id, pending, err := queueAgentMail(&db.AgentMessage{
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
	}, req.Attachments)
	if err != nil {
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, finalConv, full)
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Async delivery (JOH-310): queueAgentMessage handed the recipient to the
	// per-agent worker; the sender never blocks on the tmux nudge.
	writeJSON(w, http.StatusOK, sendResp{
		ID:             id,
		Queued:         true,
		Pending:        pending,
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
func handleMultiRecipient(w http.ResponseWriter, r *http.Request, fromID, primaryConv, primaryOriginalTo string, primaryGroupID int64, primaryViaName string, req *sendReq) {
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
		ccGroupID, _, ok := resolveMessageRouting(w, r, fromID, ccConv)
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
	primaryID, primaryPending, err := queueAgentMail(&db.AgentMessage{
		GroupID:        primaryGroupID,
		FromConv:       fromID,
		ToConv:         primaryConv,
		OriginalToConv: primaryOriginalTo,
		Subject:        req.Subject,
		Body:           req.Body,
		ToRecipients:   toRecipients,
		CcRecipients:   ccRecipients,
	}, req.Attachments)
	if err != nil {
		if full, ok := agentMessageQueueFull(err); ok {
			out.Recipients = append(out.Recipients, recipient{
				ConvID: primaryConv, AgentID: peerAgentID(primaryConv), Title: primaryTitle,
				Pending: full.Pending, QueueFull: true, Limit: full.Limit,
				Error: queueFullHint(full.Pending, full.Limit), RedirectedFrom: primaryOriginalTo,
			})
		} else {
			slog.Warn("multi-recipient: primary insert failed", "to", primaryConv, "error", err)
			out.Recipients = append(out.Recipients, recipient{
				ConvID: primaryConv, AgentID: peerAgentID(primaryConv), Title: primaryTitle,
				Error: "failed to queue message: " + err.Error(), RedirectedFrom: primaryOriginalTo,
			})
		}
	} else {
		out.Recipients = append(out.Recipients, recipient{
			ConvID:         primaryConv,
			AgentID:        peerAgentID(primaryConv),
			Title:          primaryTitle,
			MessageID:      primaryID,
			Queued:         true,
			Pending:        primaryPending,
			RedirectedFrom: primaryOriginalTo,
		})
	}

	for _, r := range resolved {
		id, pending, err := queueAgentMail(&db.AgentMessage{
			GroupID:        r.GroupID,
			FromConv:       fromID,
			ToConv:         r.ConvID,
			OriginalToConv: r.OriginalToConv,
			Subject:        req.Subject,
			Body:           req.Body,
			ToRecipients:   toRecipients,
			CcRecipients:   ccRecipients,
		}, req.Attachments)
		if err != nil {
			if full, ok := agentMessageQueueFull(err); ok {
				out.Recipients = append(out.Recipients, recipient{
					ConvID: r.ConvID, AgentID: peerAgentID(r.ConvID), Title: r.Title,
					Pending: full.Pending, QueueFull: true, Limit: full.Limit,
					Error: queueFullHint(full.Pending, full.Limit), RedirectedFrom: r.OriginalToConv,
				})
				continue
			}
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
				Error:     "failed to queue message: " + err.Error(),
			})
			continue
		}
		out.Recipients = append(out.Recipients, recipient{
			ConvID:         r.ConvID,
			AgentID:        peerAgentID(r.ConvID),
			Title:          r.Title,
			MessageID:      id,
			Queued:         true,
			Pending:        pending,
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
	recipients, err := fanOutToGroup(g, fromID, req.Subject, req.Body, req.Role, req.Members, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sendResp{ViaGroup: g.Name, Recipients: recipients})
}

// handleOperatorMulticast fans out req.Body to a group AS THE HUMAN OPERATOR —
// there is no sender agent, so no member/owner gate applies (the dashboard
// cookie + Origin pin in checkDashboardAuth is the human-consent layer, exactly
// as for the operator 1:1 mail and the operator-owned cron path). Every
// delivered row is stamped operator-authored: recipients see "the human
// operator" as the sender and the row is non-replyable. The target group must
// be named explicitly (an empty "group:" has no operator "own group" to fall
// back to). req.Role / req.Members narrow the recipient set exactly as they do
// for an agent multicast.
func handleOperatorMulticast(w http.ResponseWriter, req *sendReq) {
	token := strings.TrimSpace(strings.TrimPrefix(req.To, multicastPrefix))
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"a group name or id is required after 'group:' when sending as the operator")
		return
	}
	// fromID "" only resolves a named/numbered group here — resolveMulticastGroup
	// never reaches resolveOwnGroup for a non-empty token.
	g, ok := resolveMulticastGroup(w, "", token)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	recipients, err := fanOutToGroup(g, "", req.Subject, req.Body, req.Role, req.Members, true)
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
// responsibility — fanOutToGroup itself does no permission checking. Regular
// group sends are bounded; cron uses the filtered core with bounded=false.
//
// operatorAuthored stamps every delivered row as human-operator mail (fromConv
// is empty on that path); the recipient sees "the human operator" as the
// sender and the row is non-replyable. Agent sends pass false.
func fanOutToGroup(g *db.AgentGroup, fromConv, subject, body, roleFilter string, memberFilter []string, operatorAuthored bool) ([]recipient, error) {
	recipients, _, err := fanOutToGroupFiltered(
		g, fromConv, subject, body, roleFilter, memberFilter, nil, true, operatorAuthored, 0)
	return recipients, err
}

// fanOutToGroupFiltered optionally applies recipientFilter after all normal
// membership/role/subset/self filters. A rejected recipient is counted but no
// inbox row is inserted. Cron uses this to discard offline ticks by default;
// bounded=false keeps internal traffic exempt from regular-message
// backpressure. A non-zero cronJobID additionally coalesces repeated cron
// ticks to the latest undelivered row for each recipient.
func fanOutToGroupFiltered(g *db.AgentGroup, fromConv, subject, body, roleFilter string, memberFilter []string,
	recipientFilter func(string) bool, bounded, operatorAuthored bool, cronJobID int64,
) ([]recipient, int, error) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil, 0, err
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
	skipped := 0
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
		if recipientFilter != nil && !recipientFilter(finalConv) {
			skipped++
			continue
		}
		message := &db.AgentMessage{
			GroupID:          g.ID,
			FromConv:         fromConv,
			ToConv:           finalConv,
			OriginalToConv:   originalTo,
			Subject:          subject,
			Body:             body,
			OperatorAuthored: operatorAuthored,
		}
		var id int64
		var pending int
		var err error
		if bounded {
			id, pending, err = queueRegularAgentMessage(message)
		} else if cronJobID > 0 {
			id, err = queueCronAgentMessage(message, cronJobID)
			if err == nil {
				pending = queueDepthFor(finalConv, false)
			}
		} else {
			id, err = queueAgentMessage(message)
			if err == nil {
				pending = queueDepthFor(finalConv, false)
			}
		}
		if err != nil {
			if full, ok := agentMessageQueueFull(err); ok {
				out = append(out, recipient{
					ConvID: finalConv, AgentID: peerAgentID(finalConv), Title: agent.TitleFor(finalConv),
					Pending: full.Pending, QueueFull: true, Limit: full.Limit,
					Error: queueFullHint(full.Pending, full.Limit), RedirectedFrom: originalTo,
				})
				continue
			}
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
				Error:          "failed to queue message: " + err.Error(),
				RedirectedFrom: originalTo,
			})
			continue
		}
		out = append(out, recipient{
			ConvID:         finalConv,
			AgentID:        peerAgentID(finalConv),
			Title:          agent.TitleFor(finalConv),
			MessageID:      id,
			Queued:         true,
			Pending:        pending,
			RedirectedFrom: originalTo,
		})
	}
	return out, skipped, nil
}
