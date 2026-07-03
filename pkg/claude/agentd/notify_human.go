package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// humanMsgNotify is the OS-notification seam for notify-human: a desktop
// banner companion to the dashboard Messages tab. Production routes it
// through notify.SendHumanMessage (which self-gates on config and no-ops
// when disabled); flow tests swap in a recorder via
// SetHumanMessageNotifierForTest. The handler fires it through
// goBackground so a slow platform send (WSL spawns PowerShell) never
// blocks the request.
var humanMsgNotify = notify.SendHumanMessage

// notifyHumanRequest is the POST /v1/notify-human body.
type notifyHumanRequest struct {
	Body    string `json:"body"`
	Subject string `json:"subject"`
}

// Size caps for a human notification. A notification is a short
// message, not a document — bounding it keeps one looping or
// misbehaving sender from bloating the human_messages table and the
// /api/snapshot payload (every message ships in every 2s snapshot).
const (
	maxNotifyHumanBodyLen    = 16 * 1024
	maxNotifyHumanSubjectLen = 256
)

// maxNotifyHumanRequestBytes bounds the raw POST body the daemon will
// buffer for /v1/notify-human, enforced by http.MaxBytesReader *before*
// the JSON decode. maxNotifyHumanBodyLen / maxNotifyHumanSubjectLen cap
// the *decoded* strings; this caps the *wire* bytes — so a malicious
// local agent cannot stream a multi-GB body into daemon memory before
// the decoded-length check ever runs (the actual DoS the size caps
// imply they address).
//
// JSON escaping inflates content — `"` and `\` double, and control or
// HTML-significant chars expand to a 6-byte \uXXXX — so the wire cap is
// the decoded caps times 6 plus headroom for the JSON envelope. That is
// loose enough that no legitimate body (even a maximally-escaped one) is
// rejected pre-decode, yet still orders of magnitude below the multi-GB
// range that is the real concern.
const maxNotifyHumanRequestBytes = 6*(maxNotifyHumanBodyLen+maxNotifyHumanSubjectLen) + 1024

// handleNotifyHuman serves POST /v1/notify-human — the daemon side of
// `tclaude agent notify-human`. It gates via requireNotifyHumanPermission,
// then persists the message to the human_messages table, where the
// dashboard Messages tab surfaces it.
//
// from_title / group_name are snapshotted at insert (notifyHumanCaller*)
// so a later rename or deletion of the sending agent cannot blank an
// already-delivered message.
func handleNotifyHuman(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	callerConv, ok := requireNotifyHumanPermission(w, r)
	if !ok {
		return
	}
	// Cap the buffered request body before decoding — see
	// maxNotifyHumanRequestBytes. An over-cap body fails the Decode below
	// with http.MaxBytesReader's error, handled as a 400 like any other
	// malformed request.
	r.Body = http.MaxBytesReader(w, r.Body, maxNotifyHumanRequestBytes)
	var body notifyHumanRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	body.Subject = strings.TrimSpace(body.Subject)
	if body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required")
		return
	}
	if len(body.Body) > maxNotifyHumanBodyLen {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("body too long: %d bytes, max %d", len(body.Body), maxNotifyHumanBodyLen))
		return
	}
	if len(body.Subject) > maxNotifyHumanSubjectLen {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("subject too long: %d bytes, max %d", len(body.Subject), maxNotifyHumanSubjectLen))
		return
	}
	// Snapshot the sender attribution once, reused for both the persisted
	// row and the OS notification below.
	fromTitle := notifyHumanCallerTitle(callerConv)
	groupName := notifyHumanCallerGroup(callerConv)
	id, err := db.InsertHumanMessage(&db.HumanMessage{
		FromConv:  callerConv,
		FromTitle: fromTitle,
		GroupName: groupName,
		Subject:   body.Subject,
		Body:      body.Body,
		CreatedAt: time.Now(),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"failed to record message: "+err.Error())
		return
	}
	// Also raise a desktop notification (off the request goroutine — a
	// platform send can spawn a subprocess). Self-gates on config, so this
	// is a no-op unless the human opted in. The per-agent / per-group
	// notification filters apply here too: a muted sender's ping still
	// lands in the Messages tab (with the unread badge), it just skips
	// the OS banner. Checked outside the seam so flow tests observe it.
	senderSession := notifyHumanSenderSessionID(callerConv)
	goBackground(func() {
		if !notify.AllowedForConv(callerConv) {
			return
		}
		humanMsgNotify(senderSession, fromTitle, groupName, body.Subject, body.Body)
	})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "delivered": true})
}

// notifyHumanSenderSessionID resolves the caller conv-id to its tclaude
// session ID so the desktop notification can click-to-focus the sending
// agent's terminal — the OS-notification twin of the dashboard's
// per-message Focus button. Empty for the human path (callerConv == "")
// or when the sender has no recorded session; the notification still
// fires, just non-clickable.
func notifyHumanSenderSessionID(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	if row, err := db.FindSessionByConvID(callerConv); err == nil && row != nil {
		return row.ID
	}
	return ""
}

// requireNotifyHumanPermission gates POST /v1/notify-human. The caller
// passes if ANY of:
//
//   - they are the human, or hold the human.notify slug (config default
//     / per-conv grant / sudo), or clear the X-Tclaude-Ask-Human popup;
//   - they own at least one group — a group owner is a trusted
//     coordinating role and gets human.notify by default, slug or not.
//
// The owner default is realised as a structural bypass at the
// permUndecided level (via requirePermissionEx), so the universal
// precedence holds: a permAllow grant passes, and an explicit deny
// override is authoritative and suppresses the owner default too — deny
// always wins, the same as every other gate. Returns (callerConvID, ok);
// callerConvID is "" for the human path. On failure the response is
// already written.
func requireNotifyHumanPermission(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requirePermissionEx(w, r, PermHumanNotify, func(convID string) bool {
		owned, err := db.ListGroupsOwnedBy(convID)
		return err == nil && len(owned) > 0
	})
}

// notifyHumanCallerTitle resolves a caller conv-id to its display title
// for the message's sender attribution. Empty for the human path
// (callerConv == "") or when the conv has no resolvable title.
func notifyHumanCallerTitle(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	if row := agent.FreshConvRowResolved(callerConv); row != nil {
		return agent.DisplayTitle(row)
	}
	return ""
}

// notifyHumanCallerGroup returns one group name the caller belongs to,
// for the message's "which project" context. Empty when the caller is
// ungrouped or is the human. When the caller is in several groups the
// first is used — the attribution is a hint, not an audit.
func notifyHumanCallerGroup(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	groups, err := db.ListGroupsForConv(callerConv)
	if err != nil || len(groups) == 0 {
		return ""
	}
	return groups[0].Name
}

// dashboardHumanMessage is the wire shape of one Messages-tab row in the
// dashboard snapshot.
type dashboardHumanMessage struct {
	ID        int64  `json:"id"`
	FromConv  string `json:"from_conv"`
	FromAgent string `json:"from_agent"`
	FromTitle string `json:"from_title"`
	Group     string `json:"group"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
}

// buildHumanMessagesSnapshot loads the human_messages rows for the
// dashboard snapshot, newest first, plus the unread count that drives
// the Messages tab badge.
func buildHumanMessagesSnapshot() ([]dashboardHumanMessage, int) {
	rows, err := db.ListHumanMessages()
	if err != nil {
		slog.Warn("dashboard: list human messages failed", "error", err)
		// Empty (not nil) slice so the snapshot serializes [] — the
		// dashboard JS calls .map() on it directly.
		return []dashboardHumanMessage{}, 0
	}
	out := make([]dashboardHumanMessage, 0, len(rows))
	unread := 0
	for _, m := range rows {
		if !m.IsRead() {
			unread++
		}
		out = append(out, dashboardHumanMessage{
			ID:        m.ID,
			FromConv:  m.FromConv,
			FromAgent: m.FromAgent,
			FromTitle: m.FromTitle,
			Group:     m.GroupName,
			Subject:   m.Subject,
			Body:      m.Body,
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
			Read:      m.IsRead(),
		})
	}
	return out, unread
}

// handleDashboardHumanMessagesRead serves POST /api/human-messages/read
// — sets read-state on one message ({"id": N}, optionally
// {"read": false} to mark it unread) or marks every message read
// ({"all": true}). The "read" field defaults to true when omitted, so
// existing {"id": N} callers keep marking read; {"id": N, "read": false}
// is the reader's "mark unread" opt-out. Cookie-authed (dashboard-only).
func handleDashboardHumanMessagesRead(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// The body is a tiny {"id":N} / {"all":true} envelope; cap it well
	// below anything legitimate so a stray huge POST cannot be buffered.
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var body struct {
		ID   int64 `json:"id"`
		All  bool  `json:"all"`
		Read *bool `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.All {
		// "all" is the "mark all read" control; there's no "mark all
		// unread" affordance, so it ignores the read field.
		n, err := db.MarkAllHumanMessagesRead()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"marked": n})
		return
	}
	if body.ID <= 0 {
		http.Error(w, "id is required (or pass {\"all\":true})", http.StatusBadRequest)
		return
	}
	// read defaults to true when omitted, so {"id": N} keeps marking read;
	// {"id": N, "read": false} marks the message unread.
	read := body.Read == nil || *body.Read
	var err error
	if read {
		_, err = db.MarkHumanMessageRead(body.ID)
	} else {
		_, err = db.MarkHumanMessageUnread(body.ID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": 1})
}

// handleDashboardHumanMessagesClear serves POST /api/human-messages/clear
// — hard-deletes every message that has been marked read (the manual
// "clear read" control). Unread messages survive. Cookie-authed.
func handleDashboardHumanMessagesClear(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	n, err := db.DeleteReadHumanMessages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// handleDashboardHumanMessagesDelete serves POST /api/human-messages/delete
// — hard-deletes one message ({"id": N}) or several ({"ids": [...]}),
// read or unread. The per-message and multi-select delete controls on
// the tab, distinct from the bulk "clear read" sweep. Cookie-authed
// (dashboard-only).
func handleDashboardHumanMessagesDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// A {"id":N} or {"ids":[...]} envelope — cap the body generously
	// above a "select all then delete" list but well below anything that
	// could blow up memory.
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var body struct {
		ID  int64   `json:"id"`
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.IDs) > 0 {
		n, err := db.DeleteHumanMessages(body.IDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
		return
	}
	if body.ID <= 0 {
		http.Error(w, "id or ids is required", http.StatusBadRequest)
		return
	}
	deleted, err := db.DeleteHumanMessage(body.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n := 0
	if deleted {
		n = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// replySubjectFor derives the subject of an operator reply from the
// subject of the notification being answered. A notify-human ping often
// carries no subject, so we fall back to a fixed line that still tells
// the agent WHO is speaking; when there is one we prefix "Re: " (bounded
// so a long original can't blow past the inbox subject cap).
func replySubjectFor(orig string) string {
	orig = strings.TrimSpace(orig)
	if orig == "" {
		return "Reply from the human operator"
	}
	// Bound the echoed subject. Truncate on a RUNE boundary — a byte slice
	// could split a multi-byte character and leave invalid UTF-8 in the
	// subject (the original is capped in bytes, so a long unicode subject
	// can reach here).
	const maxRe = 200
	if r := []rune(orig); len(r) > maxRe {
		orig = string(r[:maxRe]) + "…"
	}
	return "Re: " + orig
}

// handleDashboardHumanMessagesReply serves POST /api/human-messages/reply
// — the operator's answer to a `notify-human` ping, sent back to the
// agent that raised it. Body: {"id": N, "body": "..."} where id is the
// human_messages row being replied to.
//
// The reply target is resolved AUTHORITATIVELY from the stored row (the
// browser passes only the message id + text), so a reply can only route
// to the notification's real sender. It is delivered as a sender-less
// operator message — the same universal-inbox transport the dashboard's
// self-reincarnate request and the spawn brief use (FromConv ""): a live
// target is nudged immediately; the mail UI renders a sender-less row as
// the human/operator, which is exactly what this is.
//
// The operator asked that a reply be BLOCKED when the agent is offline —
// an offline agent has no live session, and answering a question into the
// void reads as delivered when it isn't. So this gates on a live tmux
// session and rejects (409) when the target is offline; the dashboard
// disables Send and shows the same reason, but the gate is enforced here
// too so a stale snapshot (agent went offline between poll and click)
// still can't slip a reply through. Cookie-authed (dashboard-only).
func handleDashboardHumanMessagesReply(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// The reply body is capped exactly like a notify-human message — same
	// inbox, same reason to bound it. Cap the wire bytes before decode.
	r.Body = http.MaxBytesReader(w, r.Body, maxNotifyHumanRequestBytes)
	var body struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	if body.ID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id is required")
		return
	}
	if body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required (the reply text)")
		return
	}
	if len(body.Body) > maxNotifyHumanBodyLen {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("body too long: %d bytes, max %d", len(body.Body), maxNotifyHumanBodyLen))
		return
	}
	orig, err := db.GetHumanMessage(body.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "load message: "+err.Error())
		return
	}
	if orig == nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no human message #%d to reply to", body.ID))
		return
	}
	// Resolve who to reply to. Lead with the stable agent_id (rotation-immune
	// across reincarnation), falling back to the raw from_conv for old rows /
	// a sender that never became an actor. ResolveSelector then walks any
	// succession chain forward, so the reply reaches the live generation.
	selector := orig.FromAgent
	if selector == "" {
		selector = orig.FromConv
	}
	if selector == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this notification has no sender to reply to",
			"code":  "no_sender",
		})
		return
	}
	res, _, err := agent.ResolveSelector(selector)
	if err != nil || res == nil || res.ConvID == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "cannot resolve the sending agent — it may have been deleted",
			"code":  "unresolved",
		})
		return
	}
	target := res.ConvID
	// Online gate — the reply is blocked when the target has no live tmux
	// session (see the doc comment). One tmux ls; a map lookup against it.
	aliveSessions, _ := session.LiveTmuxSessions()
	if !isConvOnlineIn(target, aliveSessions) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the agent is offline — it has no live session to receive a reply",
			"code":  "offline",
		})
		return
	}
	// Deliver as a sender-less operator message on the universal inbox
	// (FromConv "", group_id 0 = a direct message), then nudge if the pane
	// is ready. nudgeIfAlive may HOLD delivery when the agent is mid-prompt
	// (awaiting human input) — the row still lands in its inbox and flushes
	// when it resumes; we surface that as "held" so the toast can say so.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:      0,
		FromConv:     "",
		ToConv:       target,
		Subject:      replySubjectFor(orig.Subject),
		Body:         body.Body,
		ToRecipients: []string{target},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "queue reply: "+err.Error())
		return
	}
	outcome := nudgeIfAlive(id, target)
	// Replying means the operator has handled this notification — mark the
	// original read (idempotent; opening it in the reader usually already
	// did). Best-effort: a failure here must not fail the delivered reply.
	if _, err := db.MarkHumanMessageRead(body.ID); err != nil {
		slog.Warn("reply: mark original human message read failed", "id", body.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message_id": id,
		"conv_id":    target,
		"delivered":  outcome.delivered(),
		"held":       outcome.held(),
	})
}
