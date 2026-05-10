package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	ConvID  string   `json:"conv_id,omitempty"`
	Title   string   `json:"title,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}

func handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	p := peerFromContext(r.Context())
	if p.ConvID == "" {
		writeJSON(w, http.StatusOK, whoamiResp{IsHuman: true})
		return
	}
	row := agent.FreshConvRow(p.ConvID)
	title := "(unnamed)"
	if row != nil {
		if t := agent.DisplayTitle(row); t != "" {
			title = t
		}
	}
	groups, _ := db.ListGroupsForConv(p.ConvID)
	gs := make([]string, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, g.Name)
	}
	writeJSON(w, http.StatusOK, whoamiResp{ConvID: p.ConvID, Title: title, Groups: gs})
}

// --- /v1/lookup ---

func handleLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if _, ok := requireAgent(w, r); !ok {
		return
	}
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
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

// --- /v1/peers ---

type peerEntry struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title"`
	Alias  string   `json:"alias,omitempty"`
	Role   string   `json:"role,omitempty"`
	Descr  string   `json:"descr,omitempty"`
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
//  2. **Online ungrouped agents.** Conv-sessions whose tmux is alive
//     and which weren't already surfaced by pass 1. Caller (when
//     known) is excluded. This makes `tclaude agent ls` reflect
//     "what's running right now" rather than "what's been added to
//     a group", which was the user's frequent paper-cut.
func handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	p := peerFromContext(r.Context())
	myID := p.ConvID

	var groups []*db.AgentGroup
	var err error
	if myID == "" {
		groups, err = db.ListAgentGroups()
	} else {
		groups, err = db.ListGroupsForConv(myID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
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
				row, _ := db.GetConvIndex(m.ConvID)
				title := "(unknown)"
				if row != nil {
					if t := agent.DisplayTitle(row); t != "" {
						title = t
					}
				}
				pe = &peerEntry{
					ConvID: m.ConvID,
					Title:  title,
					Alias:  m.Alias,
					Role:   m.Role,
					Descr:  m.Descr,
					Online: isConvOnline(m.ConvID),
				}
				byConv[m.ConvID] = pe
			}
			pe.Groups = append(pe.Groups, g.Name)
		}
	}
	// Pass 2: online conv-sessions that aren't already represented.
	// We iterate the sessions table directly (cheaper than fanning
	// out isConvOnline per row) and check tmux liveness inline.
	if sessions, err := db.ListSessions(); err == nil {
		for _, s := range sessions {
			if s.ConvID == "" || s.ConvID == myID {
				continue
			}
			if _, exists := byConv[s.ConvID]; exists {
				continue
			}
			if s.TmuxSession == "" || !session.IsTmuxSessionAlive(s.TmuxSession) {
				continue
			}
			row, _ := db.GetConvIndex(s.ConvID)
			title := "(unknown)"
			if row != nil {
				if t := agent.DisplayTitle(row); t != "" {
					title = t
				}
			}
			byConv[s.ConvID] = &peerEntry{
				ConvID: s.ConvID,
				Title:  title,
				Online: true,
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
		out = append(out, &peerEntry{ConvID: r.ConvID, Title: title})
	}
	return out
}

// --- /v1/messages (POST), /v1/messages/{id} (GET) ---

type sendReq struct {
	To      string   `json:"to"`
	Cc      []string `json:"cc,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Body    string   `json:"body"`
}

// sendResp carries the result of either a direct send or a group
// fan-out. For direct messages the top-level fields (ID, Delivered)
// are populated and Recipients is nil. For multicast (To prefixed
// with "group:") ID/Delivered are zero values and Recipients lists
// one entry per non-sender member.
type sendResp struct {
	ID         int64       `json:"id,omitempty"`
	Delivered  bool        `json:"delivered,omitempty"`
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
	ConvID    string `json:"conv_id"`
	Alias     string `json:"alias,omitempty"`
	MessageID int64  `json:"message_id"`
	Delivered bool   `json:"delivered"`
	// RedirectedFrom mirrors sendResp.RedirectedFrom on a per-recipient
	// basis: when the entry's ConvID is the live successor of a
	// superseded id the sender originally addressed, the original id
	// goes here. Empty when the address was already canonical.
	RedirectedFrom string `json:"redirected_from,omitempty"`
}

// multicastPrefix marks a multicast target. `to: "group:reviewer-team"`
// fans out to every member of that group except the sender.
const multicastPrefix = "group:"

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
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is empty")
		return
	}
	if strings.HasPrefix(req.To, multicastPrefix) {
		handleMulticast(w, fromID, &req)
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
	// superseded conv-id and the resolver redirected it. Alias / prefix
	// inputs naturally skip this branch (they don't have chain rows
	// keyed on the literal alias text).
	finalConv := target.ConvID
	originalTo := ""
	rawInput := strings.TrimSpace(req.To)
	if rawInput != "" && rawInput != finalConv && db.ResolveLatestConv(rawInput) == finalConv {
		originalTo = rawInput
	}
	if finalConv == fromID {
		writeError(w, http.StatusBadRequest, "invalid_arg", "cannot message self")
		return
	}
	// Authorisation: shared-group OR sender owns a group containing
	// target. Owner-as-non-member lets a coordinator agent address its
	// teams without itself being a peer. Authority is checked against
	// the LIVE successor — the outdated id may have lost membership
	// by the time the successor took over, but the successor is who
	// actually receives the message.
	via, _, err := db.CanSenderReachTarget(fromID, finalConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if via == nil {
		writeError(w, http.StatusForbidden, "auth",
			"no shared group with target and you do not own a group containing the target")
		return
	}
	if !requireGroupActive(w, via) {
		return
	}

	// Multi-recipient (--cc) path: one row per (To + each CC), each with
	// the same to_recipients / cc_recipients audience. CCs that resolve
	// ambiguously / not at all / aren't reachable surface as a 4xx so
	// the sender can fix the typo before any rows are written.
	if len(req.Cc) > 0 {
		handleMultiRecipient(w, fromID, finalConv, originalTo, via, &req)
		return
	}

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:        via.ID,
		FromConv:       fromID,
		ToConv:         finalConv,
		OriginalToConv: originalTo,
		Subject:        req.Subject,
		Body:           req.Body,
		// Even single-recipient sends record the audience arrays now
		// so the recipient's `inbox read` can render a consistent
		// "To: ..." header. CC stays empty.
		ToRecipients: []string{finalConv},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	delivered := nudgeIfAlive(id, finalConv)
	writeJSON(w, http.StatusOK, sendResp{
		ID:             id,
		Delivered:      delivered,
		ViaGroup:       via.Name,
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
// audience. The primary's `via` group has already been validated by
// the caller; each CC is independently resolved and authorised.
//
// Pre-validation: if any CC fails (ambiguous, unknown, unreachable,
// duplicate of self/primary), the whole send is rejected before any
// rows are written. Half-broadcasts are confusing for the recipient
// who notices an extra "CC: <missing>" entry that wasn't actually
// delivered.
func handleMultiRecipient(w http.ResponseWriter, fromID, primaryConv, primaryOriginalTo string, primaryVia *db.AgentGroup, req *sendReq) {
	type resolvedCC struct {
		ConvID         string
		OriginalToConv string
		Alias          string
		Via            *db.AgentGroup
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
		via, _, err := db.CanSenderReachTarget(fromID, ccConv)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if via == nil {
			writeError(w, http.StatusForbidden, "auth",
				fmt.Sprintf("CC %q: no shared group and you do not own a group containing it", sel))
			return
		}
		if via.IsArchived() {
			writeError(w, http.StatusConflict, "archived",
				fmt.Sprintf("CC %q routes via archived group %q", sel, via.Name))
			return
		}
		alias := agent.AliasFor(via.ID, ccConv)
		resolved = append(resolved, resolvedCC{ConvID: ccConv, OriginalToConv: ccOriginal, Alias: alias, Via: via})
	}

	toRecipients := []string{primaryConv}
	ccRecipients := make([]string, 0, len(resolved))
	for _, r := range resolved {
		ccRecipients = append(ccRecipients, r.ConvID)
	}

	out := sendResp{ViaGroup: primaryVia.Name, Recipients: []recipient{}}

	// Insert + nudge primary first so the response order matches the
	// "To:, CC: ..." header order in inbox read.
	primaryAlias := agent.AliasFor(primaryVia.ID, primaryConv)
	primaryID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:        primaryVia.ID,
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
	primaryDelivered := nudgeIfAlive(primaryID, primaryConv)
	out.Recipients = append(out.Recipients, recipient{
		ConvID:         primaryConv,
		Alias:          primaryAlias,
		MessageID:      primaryID,
		Delivered:      primaryDelivered,
		RedirectedFrom: primaryOriginalTo,
	})

	for _, r := range resolved {
		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:        r.Via.ID,
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
				Alias:     r.Alias,
				MessageID: 0,
				Delivered: false,
			})
			continue
		}
		delivered := nudgeIfAlive(id, r.ConvID)
		out.Recipients = append(out.Recipients, recipient{
			ConvID:         r.ConvID,
			Alias:          r.Alias,
			MessageID:      id,
			Delivered:      delivered,
			RedirectedFrom: r.OriginalToConv,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMulticast fans out req.Body to every member of the named group
// except the sender. Auth: the sender must be a member of the target
// group (we don't allow strangers to broadcast in). Each recipient
// gets its own agent_messages row + tmux nudge if online; replies
// from any recipient go back to the sender as a normal direct
// message via the original group.
//
// Returns 200 with recipients=[] and via_group set (idempotent
// success) when the group is empty save for the sender.
func handleMulticast(w http.ResponseWriter, fromID string, req *sendReq) {
	groupName := strings.TrimPrefix(req.To, multicastPrefix)
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"multicast target is empty; expected 'group:<name>'")
		return
	}
	g, err := db.GetAgentGroupByName(groupName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no group named %q", groupName))
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
			fmt.Sprintf("you are not a member or owner of group %q", groupName))
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := sendResp{ViaGroup: g.Name, Recipients: []recipient{}}
	for _, m := range members {
		if m.ConvID == fromID {
			continue
		}
		// Defensive: membership migrations on reincarnate are atomic
		// today (the new conv-id is added before the old is removed),
		// so a member row should already point at the live successor.
		// But cross-machine sync, manual DB edits, or a future race
		// could leave a stale row. Walk the chain so the message
		// always lands on the live successor; cheap insurance.
		finalConv, originalTo := walkSuccession(m.ConvID)
		if finalConv == fromID {
			// Sender's own conv may be the live successor of a member
			// row (rare manager-pattern edge case); skip self-send.
			continue
		}
		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:        g.ID,
			FromConv:       fromID,
			ToConv:         finalConv,
			OriginalToConv: originalTo,
			Subject:        req.Subject,
			Body:           req.Body,
		})
		if err != nil {
			// Don't abort the whole broadcast on one DB error; record
			// it and continue. The sender sees per-recipient status
			// in the response and can retry the failures explicitly.
			slog.Warn("multicast: insert failed",
				"group", g.Name, "to", finalConv, "error", err)
			out.Recipients = append(out.Recipients, recipient{
				ConvID:         finalConv,
				Alias:          m.Alias,
				MessageID:      0,
				Delivered:      false,
				RedirectedFrom: originalTo,
			})
			continue
		}
		delivered := nudgeIfAlive(id, finalConv)
		out.Recipients = append(out.Recipients, recipient{
			ConvID:         finalConv,
			Alias:          m.Alias,
			MessageID:      id,
			Delivered:      delivered,
			RedirectedFrom: originalTo,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// nudgeIfAlive looks up the target's tmux session and, if alive, sends
// the bracketed system-style nudge. Returns true on successful delivery.
//
// This is the half that broke for sandboxed senders in v1: the daemon
// owns the tmux side here, so the sender's sandbox is irrelevant.
//
// The DB can hold multiple session rows for the same conv_id (auto-register
// creates new rows alongside stale ones from previous launches). We pick
// the first one whose tmux session is actually alive, most-recent first.
func nudgeIfAlive(msgID int64, toID string) bool {
	candidates, err := db.FindSessionsByConvID(toID)
	if err != nil {
		return false
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
		return false
	}
	// Minimal nudge: just announce the message. Sender, subject, group,
	// reply addressing — all of that lives in the message itself, fetched
	// via `tclaude agent inbox read <id>`. Keeping the bracket text terse
	// avoids leaking ephemeral details (short conv-id prefixes,
	// alias-of-the-moment) into the receiver's transcript.
	nudge := fmt.Sprintf(
		"[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]",
		msgID, msgID,
	)
	if err := injectTextAndSubmit(sess.TmuxSession+":0.0", nudge); err != nil {
		slog.Warn("nudge failed", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	// delivered_at is internal bookkeeping; the nudge itself already
	// landed, so log on failure rather than failing the whole call.
	if err := db.MarkAgentMessageDelivered(msgID); err != nil {
		slog.Warn("failed to record delivered_at", "error", err, "msg_id", msgID)
	}
	return true
}

// injectSlashCommand finds an alive tmux session for convID and types the
// given slash-command line into its CC pane, followed by a submit Enter.
// If followUp is non-empty, it is sent as a fresh prompt right after the
// slash submit. Returns true on successful delivery.
//
// Note: when used with /compact, the follow-up bytes queue in the pty
// until CC resumes reading after the slash command settles. We don't
// wait for the slash to complete — there's no clean way to detect it
// without a hook. The follow-up may land in a still-busy textarea on
// unlucky timing; agents that depend on tight ordering should poll
// context-info and submit the follow-up themselves once compact has
// resolved.
func injectSlashCommand(convID, line, followUp string) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	var sess *db.SessionRow
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			sess = c
			break
		}
	}
	if sess == nil {
		return false
	}
	target := sess.TmuxSession + ":0.0"
	if err := injectTextAndSubmit(target, line); err != nil {
		slog.Warn("slash-command inject failed", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	if followUp != "" {
		if err := injectTextAndSubmit(target, followUp); err != nil {
			slog.Warn("slash-command follow-up failed", "error", err, "tmux", sess.TmuxSession)
			return false
		}
	}
	return true
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
func injectTextAndSubmit(tmuxTarget, text string) error {
	if err := clcommon.TmuxCommand("send-keys", "-t", tmuxTarget, text).Run(); err != nil {
		return fmt.Errorf("send-keys text: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := clcommon.TmuxCommand("send-keys", "-t", tmuxTarget, "Enter").Run(); err != nil {
		return fmt.Errorf("send-keys submit: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = clcommon.TmuxCommand("send-keys", "-t", tmuxTarget, "Enter").Run()
	return nil
}

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
func runRenameOrchestration(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
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
	if !injectSlashCommand(target, "/rename "+body.Title, "") {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session to inject /rename into")
		return
	}
	resp := map[string]any{
		"conv_id": target,
		"title":   body.Title,
		"note":    "rename submitted via tmux send-keys; CC will write the new title on its next turn",
	}
	if caller != "" && caller != target {
		resp["caller_conv"] = caller
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWhoamiCompact injects `/compact` into the caller's own CC pane.
// Optional follow-up text is queued as a fresh prompt right after.
// Permission-gated on `self.compact`.
func handleWhoamiCompact(w http.ResponseWriter, r *http.Request) {
	handleSelfSlash(w, r, PermSelfCompact, "/compact", "compact")
}

// handleAgentCompact injects `/compact` into ANOTHER agent's CC pane.
// Routed via handleAgentByConv (the dispatcher resolves targetConv from
// the URL). Auth: agent.compact slug OR caller is owner of a group
// containing target. Same body shape as the self variant.
func handleAgentCompact(w http.ResponseWriter, r *http.Request, targetConv string) {
	handleAgentSlash(w, r, PermAgentCompact, targetConv, "/compact", "compact")
}

// handleSelfSlash factors out self-targeted slash-command handlers like
// /compact.
func handleSelfSlash(w http.ResponseWriter, r *http.Request, perm, slash, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, perm)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own conversation; humans should use Claude Code's "+slash+" directly, or use POST /v1/agent/{conv}/"+strings.TrimPrefix(slash, "/")+" to act on another agent")
		return
	}
	runSlashOrchestration(w, r, convID, convID, slash, label)
}

// handleAgentSlash is the cross-agent counterpart to handleSelfSlash.
// Auth via requireCrossAgentPermission (slug OR owner-of-group).
func handleAgentSlash(w http.ResponseWriter, r *http.Request, perm, targetConv, slash, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, perm, targetConv)
	if !ok {
		return
	}
	runSlashOrchestration(w, r, targetConv, caller, slash, label)
}

// runSlashOrchestration validates the optional follow_up body, injects
// the slash command into the target's pane, and writes the JSON
// response. caller is recorded in the response for cross-agent calls
// so the audit trail has both sides; for self the value is the same as
// target.
func runSlashOrchestration(w http.ResponseWriter, r *http.Request, target, caller, slash, label string) {
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
	if !injectSlashCommand(target, slash, body.FollowUp) {
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
	candidates, _ := db.FindSessionsByConvID(convID)
	var snap db.ContextSnapshot
	var sessionID string
	if sess := pickLiveOrLatest(candidates); sess != nil {
		sessionID = sess.ID
		if s, err := db.GetContextSnapshot(sess.ID); err == nil {
			snap = s
		}
	}
	resp := map[string]any{
		"conv_id":             convID,
		"session_id":          sessionID,
		"context_pct":         snap.ContextPct,
		"tokens_input":        snap.TokensInput,
		"tokens_output":       snap.TokensOutput,
		"context_window_size": snap.ContextWindowSize,
		"compact_pending":     snap.CompactPending,
	}
	writeJSON(w, http.StatusOK, resp)
}

// pickLiveOrLatest returns the session row whose tmux pane is alive,
// or — falling back — the row that comes first in the list (which
// FindSessionsByConvID orders by updated_at DESC). nil when the list
// is empty.
func pickLiveOrLatest(candidates []*db.SessionRow) *db.SessionRow {
	return pickWithLiveness(candidates, func(t string) bool {
		return t != "" && session.IsTmuxSessionAlive(t)
	})
}

// pickWithLiveness is the testable core of pickLiveOrLatest. The
// liveness predicate is injected so unit tests can stub it without
// reaching for tmux on the host.
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
	deleted, err := db.DeleteAgentMessageByID(id, myID)
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
	if orig.ToConv != myID {
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
	// Routing: keep the reply on the same group_id as the original,
	// so threads stay coherent on the recipient's side even when
	// shared membership has since dissolved.
	via, err := db.GetAgentGroupByID(orig.GroupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if via == nil {
		writeError(w, http.StatusInternalServerError, "io",
			"original message references a group that no longer exists")
		return
	}
	// Reply target is the original sender. If they've since
	// reincarnated, their old conv-id is still on the message row
	// (immutable audit trail). Walk the chain so the reply lands on
	// the live successor instead of the archived inbox.
	replyTarget, replyOriginalTo := walkSuccession(orig.FromConv)
	newID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:        via.ID,
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
	delivered := nudgeIfAlive(newID, replyTarget)
	writeJSON(w, http.StatusOK, sendResp{
		ID:             newID,
		Delivered:      delivered,
		ViaGroup:       via.Name,
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
	if m.ToConv != myID {
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
		"from_alias": agent.AliasFor(m.GroupID, m.FromConv),
		"to":         m.ToConv,
		"group":      groupName,
		"subject":    m.Subject,
		"body":       m.Body,
		"created_at": m.CreatedAt.Format(time.RFC3339),
		// Reply-To is the conv-id to address when replying. Same as
		// `from` today; broken out so clients have an obvious affordance
		// and so we can support distinct reply-to addresses later
		// (e.g. shared-inbox aliases) without breaking the wire format.
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
	// Decorated with aliases when known so the receiver sees friendly names
	// alongside the conv-ids.
	if len(m.ToRecipients) > 0 {
		resp["to_recipients"] = decorateRecipients(m.GroupID, m.ToRecipients)
	}
	if len(m.CcRecipients) > 0 {
		resp["cc_recipients"] = decorateRecipients(m.GroupID, m.CcRecipients)
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
		msgs, err = db.ListAgentMessagesFromConv(myID, limit)
	} else {
		msgs, err = db.ListAgentMessagesForConv(myID, limit)
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
			item.ToShort = agent.ShortID(m.ToConv)
			item.Delivered = !m.DeliveredAt.IsZero()
		} else {
			item.From = m.FromConv
			item.FromShort = agent.ShortID(m.FromConv)
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
	deleted, err := db.PruneAgentMessagesForConv(myID, cutoff, req.ReadOnly)
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
// (alias if known, else the conv-index title, else empty). Returned as
// part of /v1/messages/{id} so `inbox read` can render
// "To: alice <abcd1234>" without a second round-trip.
type recipientLine struct {
	ConvID string `json:"conv_id"`
	Alias  string `json:"alias,omitempty"`
}

// decorateRecipients turns a recipients array (conv-ids only, as stored
// in agent_messages) into a labelled list. Best-effort lookup: a conv
// without an alias / index row just gets ConvID set, so the renderer
// can fall back to the short prefix.
func decorateRecipients(groupID int64, ids []string) []recipientLine {
	out := make([]recipientLine, 0, len(ids))
	for _, id := range ids {
		out = append(out, recipientLine{
			ConvID: id,
			Alias:  agent.AliasFor(groupID, id),
		})
	}
	return out
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
	Name     string `json:"name"`
	Descr    string `json:"descr,omitempty"`
	Members  int    `json:"members"`
	Online   int    `json:"online"`
	Archived bool   `json:"archived,omitempty"`
}

// isConvOnline reports whether any tmux session registered for this conv-id
// is currently alive. Same alive-check `nudgeIfAlive` uses for delivery.
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
		out := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			if !showArchived && g.IsArchived() {
				continue
			}
			members, _ := db.ListAgentGroupMembers(g.ID)
			online := 0
			for _, m := range members {
				if isConvOnline(m.ConvID) {
					online++
				}
			}
			out = append(out, groupSummary{
				Name:     g.Name,
				Descr:    g.Descr,
				Members:  len(members),
				Online:   online,
				Archived: g.IsArchived(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		creator, ok := requirePermission(w, r, PermGroupsCreate)
		if !ok {
			return
		}
		var body struct {
			Name  string `json:"name"`
			Descr string `json:"descr,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if body.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "name is required")
			return
		}
		if existing, _ := db.GetAgentGroupByName(body.Name); existing != nil {
			writeError(w, http.StatusConflict, "exists", "group already exists")
			return
		}
		id, err := db.CreateAgentGroup(body.Name, body.Descr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Auto-grant ownership to the creator. Skipped for the human
		// path (creator == "") since humans don't have a conv-id; the
		// human is implicitly above the permission system anyway.
		// Failure here is logged but doesn't unwind the create — the
		// human can grant ownership manually if needed.
		if creator != "" {
			if err := db.AddAgentGroupOwner(id, creator, creator); err != nil {
				slog.Warn("groups create: auto-grant owner failed",
					"group", body.Name, "creator", creator, "error", err)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": body.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// --- /v1/groups/{name}* dispatcher ---

func handleGroupByName(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/groups/")
	parts := strings.SplitN(rest, "/", 3)
	name := parts[0]
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

	// /v1/groups/{name}/stop and /resume — bulk lifecycle ops over
	// the group's members. Both are POST-only since they have side
	// effects (tmux send-keys / kill-session / spawning subprocesses).
	if len(parts) >= 2 && parts[1] == "stop" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupStop(w, r, g)
		return
	}
	if len(parts) >= 2 && parts[1] == "resume" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupResume(w, r, g)
		return
	}
	if len(parts) >= 2 && parts[1] == "spawn" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupSpawn(w, r, g)
		return
	}
	if len(parts) >= 2 && parts[1] == "archive" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupArchive(w, r, g)
		return
	}
	if len(parts) >= 2 && parts[1] == "unarchive" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupUnarchive(w, r, g)
		return
	}
	if len(parts) >= 2 && parts[1] == "rename" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupRename(w, r, g)
		return
	}
	if len(parts) >= 2 && parts[1] == "clone" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		handleGroupClone(w, r, g)
		return
	}

	// /v1/groups/{name}/owners[*]
	if len(parts) >= 2 && parts[1] == "owners" {
		switch r.Method {
		case http.MethodGet:
			handleGroupOwnersList(w, r, g)
		case http.MethodPost:
			handleGroupOwnersAdd(w, r, g)
		case http.MethodDelete:
			if len(parts) < 3 || parts[2] == "" {
				writeError(w, http.StatusBadRequest, "invalid_arg", "missing owner conv-id")
				return
			}
			handleGroupOwnersRemove(w, r, g, parts[2])
		default:
			writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST, or DELETE")
		}
		return
	}

	// /v1/groups/{name}/members[*]
	if len(parts) >= 2 && parts[1] == "members" {
		switch r.Method {
		case http.MethodGet:
			handleGroupMembersList(w, r, g)
		case http.MethodPost:
			handleGroupMembersAdd(w, r, g)
		case http.MethodPatch:
			if len(parts) < 3 || parts[2] == "" {
				writeError(w, http.StatusBadRequest, "invalid_arg", "missing member id")
				return
			}
			handleGroupMembersUpdate(w, r, g, parts[2])
		case http.MethodDelete:
			if len(parts) < 3 || parts[2] == "" {
				writeError(w, http.StatusBadRequest, "invalid_arg", "missing member id")
				return
			}
			handleGroupMembersRemove(w, r, g, parts[2])
		default:
			writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST, PATCH, or DELETE")
		}
		return
	}

	// /v1/groups/{name}
	switch r.Method {
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermGroupsRm); !ok {
			return
		}
		if err := db.DeleteAgentGroup(name); err != nil {
			writeError(w, http.StatusConflict, "constraint", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE")
	}
}

type memberJSON struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Alias  string `json:"alias,omitempty"`
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	Online bool   `json:"online"`
	Owner  bool   `json:"owner,omitempty"`
}

func handleGroupMembersList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
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
		row, _ := db.GetConvIndex(m.ConvID)
		title := "(unknown)"
		if row != nil {
			if t := agent.DisplayTitle(row); t != "" {
				title = t
			}
		}
		out = append(out, memberJSON{
			ConvID: m.ConvID,
			Title:  title,
			Alias:  m.Alias,
			Role:   m.Role,
			Descr:  m.Descr,
			Online: isConvOnline(m.ConvID),
			Owner:  ownerSet[m.ConvID],
		})
	}
	// Surface owners who aren't members so the list is comprehensive.
	// They get an "owner" role tag and no alias/descr (those are
	// member-scoped fields).
	for ownerConv := range ownerSet {
		if memberSet[ownerConv] {
			continue
		}
		row, _ := db.GetConvIndex(ownerConv)
		title := "(unknown)"
		if row != nil {
			if t := agent.DisplayTitle(row); t != "" {
				title = t
			}
		}
		out = append(out, memberJSON{
			ConvID: ownerConv,
			Title:  title,
			Role:   "owner",
			Online: isConvOnline(ownerConv),
			Owner:  true,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type ownerJSON struct {
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
	out := make([]ownerJSON, 0, len(owners))
	for _, o := range owners {
		row, _ := db.GetConvIndex(o.ConvID)
		title := "(unknown)"
		if row != nil {
			if t := agent.DisplayTitle(row); t != "" {
				title = t
			}
		}
		entry := ownerJSON{
			ConvID: o.ConvID,
			Title:  title,
			Online: isConvOnline(o.ConvID),
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
	if err := db.AddAgentGroupOwner(g.ID, res.ConvID, grantedBy); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":   g.Name,
		"conv_id": res.ConvID,
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
		Alias string `json:"alias,omitempty"`
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
		Alias:   body.Alias,
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

// handleGroupMembersUpdate patches alias/role/descr on an existing member.
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
		Alias *string `json:"alias,omitempty"`
		Role  *string `json:"role,omitempty"`
		Descr *string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Alias == nil && body.Role == nil && body.Descr == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "at least one of alias/role/descr is required")
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	n, err := db.UpdateAgentGroupMember(g.ID, res.ConvID, body.Alias, body.Role, body.Descr)
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

