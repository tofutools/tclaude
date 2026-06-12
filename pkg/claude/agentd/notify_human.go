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
	// is a no-op unless the human opted in.
	senderSession := notifyHumanSenderSessionID(callerConv)
	goBackground(func() {
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
//   - they own at least one group — a group owner is a trusted
//     coordinating role and may always reach the human, slug or not;
//   - they are the human, hold the human.notify slug, or clear the
//     X-Tclaude-Ask-Human popup — all handled by requirePermission.
//
// The group-owner check runs first so it also covers an owner who was
// never granted the slug. Returns (callerConvID, ok); callerConvID is
// "" for the human path. On failure the response is already written.
func requireNotifyHumanPermission(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := peerFromContext(r.Context())
	if classify(p) == classAgent {
		if owned, err := db.ListGroupsOwnedBy(p.ConvID); err == nil && len(owned) > 0 {
			return p.ConvID, true
		}
	}
	// Everyone else: the standard slug gate — human bypass, the
	// human.notify slug (config default / per-conv grant / sudo), and the
	// X-Tclaude-Ask-Human popup escape hatch, with a 403 otherwise.
	return requirePermission(w, r, PermHumanNotify)
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
// — marks one message read ({"id": N}) or every message read
// ({"all": true}). Cookie-authed (dashboard-only).
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
		ID  int64 `json:"id"`
		All bool  `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.All {
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
	if _, err := db.MarkHumanMessageRead(body.ID); err != nil {
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
