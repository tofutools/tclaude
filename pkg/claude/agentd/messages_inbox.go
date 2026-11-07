package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

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

// systemMailSender names tclaude itself as the sender of its own mail.
const systemMailSender = "tclaude (system)"

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

// messageNudgeText builds the bracketed tmux nudge for a delivered agent
// message, naming the sender by stable identity (JOH-27 PR3b) — the
// agent_id is rotation-immune, so it is safe to surface in the receiver's
// transcript where a conv-id prefix would have gone stale. On a read miss
// it degrades to the terse, senderless form. The async delivery worker uses
// this one formatter for immediate queue drains and deferred retries.
func messageNudgeText(msgID int64) string {
	if m, err := db.GetAgentMessage(msgID); err == nil && m != nil {
		text, _ := messageNudgeTextFor(m)
		return text
	}
	return fmt.Sprintf(
		"[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]",
		msgID, msgID)
}

// messageNudgeTextFor chooses inline-vs-pointer delivery exactly once for a
// claimed row. The returned consumed flag must be committed with the same
// attempt after the text lands; recomputing it later could disagree if config
// changes while tmux is settling.
func messageNudgeTextFor(m *db.AgentMessage) (text string, consumed bool) {
	if inline, ok := messageInlineText(m); ok {
		return inline, true
	}
	if db.IsOperatorAgentMessage(m.ID) {
		return fmt.Sprintf("[system: new agent message #%d from the human operator. Read it with: tclaude agent inbox read %d.]", m.ID, m.ID), false
	}
	// The pointer is pasted too, so it names the sender only when the label
	// is paste-safe.
	if remote, _ := remoteSenderLabel(m.ID); remote != "" && pasteSafeLine(remote) {
		return fmt.Sprintf("[system: new agent message #%d from %s for you. fetch with: tclaude agent inbox read %d]", m.ID, remote, m.ID), false
	}
	if sender := agent.MessageSenderLabel(m.FromConv, m.FromAgent); sender != "" && pasteSafeLine(sender) {
		return fmt.Sprintf("[system: new agent message #%d from %s for you. fetch with: tclaude agent inbox read %d]", m.ID, sender, m.ID), false
	}
	if m.Subject == "Morph handoff requested" {
		return fmt.Sprintf("[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d; subject: Morph handoff requested]", m.ID, m.ID), false
	}
	return fmt.Sprintf("[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]", m.ID, m.ID), false
}

func messageInlineMaxChars() int {
	cfg, err := config.Load()
	if err != nil || cfg == nil || cfg.Agent == nil || cfg.Agent.MessageInlineMaxChars == nil {
		return config.DefaultMessageInlineMaxChars
	}
	return *cfg.Agent.MessageInlineMaxChars
}

// pasteSafe reports whether s may be pasted into an agent's pane as is. The
// inline nudge carries text another agent or a peer wrote into the
// recipient's harness through tmux bracketed paste, so it must hold nothing a
// terminal or that harness could interpret rather than show, and nothing that
// reads differently from what it is: invalid UTF-8 (a raw 0x9b byte is an
// 8-bit CSI), any control character except newline and tab (C0, DEL, C1), and
// any format character (Unicode Cf: the bidi embedding, override, isolate and
// mark controls, zero-width characters, the BOM). This is a strict superset of
// termtext's terminal rule. Text that fails is not altered; the recipient gets
// the inbox pointer and reads the message unchanged (awb tcl-dih161).
func pasteSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// pasteSafeLine is pasteSafe for a one-line field of the nudge header (the
// subject, the sender, an attachment name), which may not break the line:
// no newline or tab, and no Unicode line or paragraph separator, which some
// renderers also break on.
func pasteSafeLine(s string) bool {
	return !strings.ContainsAny(s, "\n\t\u2028\u2029") && pasteSafe(s)
}

// messageInlineText returns a complete server-authored nudge only for bounded,
// pasteSafe content. Newlines and tabs are safe in the body because
// injectTextAndSubmit uses tmux bracketed paste for either one; everything
// else pasteSafe rejects stays out of the pane injection path, and the
// recipient gets the inbox pointer instead.
func messageInlineText(m *db.AgentMessage) (string, bool) {
	limit := messageInlineMaxChars()
	// Startup briefing delivery is owned by the launch seed/welcome path. Its
	// archival row may briefly be undelivered while the new pane comes online;
	// treating it as ordinary mail here would race the welcome and duplicate
	// the briefing as a second turn.
	if limit <= 0 || m.Subject == db.StartupContextSubject || strings.TrimSpace(m.Body) == "" {
		return "", false
	}
	if !pasteSafeLine(m.Subject) || !pasteSafe(m.Body) {
		return "", false
	}
	attachments, err := db.ListAgentMessageAttachments(m.ID)
	if err != nil {
		return "", false
	}
	for _, a := range attachments {
		if !pasteSafeLine(a.Filename) {
			return "", false
		}
	}
	var b strings.Builder
	operatorAuthored := db.IsOperatorAgentMessage(m.ID)
	sender := agent.MessageSenderLabel(m.FromConv, m.FromAgent)
	if remote, _ := remoteSenderLabel(m.ID); remote != "" {
		sender = remote
	}
	if !pasteSafeLine(sender) {
		return "", false
	}
	fmt.Fprintf(&b, "[system: new agent message #%d", m.ID)
	switch {
	case operatorAuthored:
		b.WriteString(" from the human operator")
	case sender != "":
		fmt.Fprintf(&b, " from %s", sender)
	}
	b.WriteString("; delivery: inline")
	if m.ParentID > 0 {
		fmt.Fprintf(&b, "; in reply to original message #%d", m.ParentID)
	}
	if m.Subject != "" {
		fmt.Fprintf(&b, "; subject: %s", m.Subject)
	}
	fmt.Fprintf(&b, "] %s", m.Body)
	if len(attachments) > 0 {
		fmt.Fprintf(&b, " Attachments (save with `tclaude agent inbox attachment %d`):", m.ID)
		for _, a := range attachments {
			fmt.Fprintf(&b, " %s;", a.Filename)
		}
	}
	text := b.String()
	if len([]rune(text)) > limit {
		return "", false
	}
	return text, true
}

// --- /v1/messages/{id} (GET) and /v1/messages/{id}/reply (POST) ---

// handleMessageByIDOrReply dispatches between the message-fetch,
// reply, and delete endpoints based on path suffix and HTTP method.
// GET  /v1/messages/{id}        -> handleMessageByID
// POST /v1/messages/{id}/reply  -> handleMessageReply
// GET  /v1/messages/{id}/attachments[/{attachment}] -> the message's files
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
	if len(parts) == 2 && parts[1] == "attachments" {
		handleMessageAttachmentList(w, r, parts[0])
		return
	}
	if len(parts) == 2 && strings.HasPrefix(parts[1], "attachments/") {
		handleMessageAttachment(w, r, parts[0], strings.TrimPrefix(parts[1], "attachments/"))
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
		Subject     string                    `json:"subject,omitempty"`
		Body        string                    `json:"body"`
		Attachments []proto.AttachmentPayload `json:"attachments,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is empty")
		return
	}
	if err := validateFedAttachments(body.Attachments); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	subject := body.Subject
	if subject == "" && orig.Subject != "" {
		subject = "Re: " + orig.Subject
	}
	// A reply to mail received from a remote instance goes back over
	// federation; reply authority comes from having received it.
	if in, err := db.FederationInboundForMessage(orig.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	} else if in != nil {
		if len(body.Attachments) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "attachments are not supported on a reply to a remote message; send it with `tclaude agent message` instead")
			return
		}
		handleFederatedReply(w, r, myID, in, subject, body.Body)
		return
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
	newID, pending, err := queueAgentMail(&db.AgentMessage{
		GroupID:        replyGroupID,
		FromConv:       myID,
		ToConv:         replyTarget,
		OriginalToConv: replyOriginalTo,
		Subject:        subject,
		Body:           body.Body,
		ParentID:       orig.ID,
	}, body.Attachments)
	if err != nil {
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, replyTarget, full)
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sendResp{
		ID:             newID,
		Queued:         true,
		Pending:        pending,
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
	if !isOperator && r.URL.Query().Get("keep-unread") != "1" {
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
	}
	operatorAuthored := db.IsOperatorAgentMessage(m.ID)
	replyable := !operatorAuthored && m.FromConv != ""
	remoteLabel, remoteAddr := remoteSenderLabel(m.ID)
	if remoteLabel != "" {
		resp["from_title"] = remoteLabel
		resp["from_agent"] = remoteAddr
		resp["remote"] = true
		replyable = strings.Contains(remoteAddr, "@")
		if replyable {
			resp["reply_to"] = remoteAddr
			resp["reply_cmd"] = fmt.Sprintf("tclaude agent reply %d \"<your reply body>\"", m.ID)
		}
	}
	resp["replyable"] = replyable
	if operatorAuthored {
		resp["from_title"] = "human operator"
	} else if m.FromConv == "" && remoteLabel == "" {
		// Senderless mail the operator did not write is tclaude's own (a
		// startup context, a task board notice). Without a title the CLI
		// would render the empty sender as the operator.
		resp["from_title"] = systemMailSender
	}
	if replyable && remoteLabel == "" {
		// Operator-authored mail has no mailbox return address and advertises
		// replyable=false above. Senderless system mail is likewise not
		// replyable. Agent-authored mail keeps the existing reply affordances.
		resp["reply_to"] = m.FromConv
		resp["reply_cmd"] = fmt.Sprintf("tclaude agent reply %d \"<your reply body>\"", m.ID)
	}
	if attachments, err := db.ListAgentMessageAttachments(m.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	} else if len(attachments) > 0 {
		resp["attachments"] = attachments
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
	ID               int64  `json:"id"`
	From             string `json:"from,omitempty"`
	FromShort        string `json:"from_short,omitempty"`
	To               string `json:"to,omitempty"`
	ToShort          string `json:"to_short,omitempty"`
	Group            string `json:"group"`
	Subject          string `json:"subject,omitempty"`
	Preview          string `json:"preview,omitempty"`
	CreatedAt        string `json:"created_at"`
	Read             bool   `json:"read"`
	ForwardedTo      string `json:"forwarded_to,omitempty"`
	ForwardState     string `json:"forward_state,omitempty"`
	Delivered        bool   `json:"delivered,omitempty"`
	NudgeDiscardedAt string `json:"nudge_discarded_at,omitempty"`
	ParentID         int64  `json:"parent_id,omitempty"`
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
	switch {
	case outbox:
		msgs, err = outboxForCaller(myID, limit)
	case unreadOnly:
		msgs, err = db.ListUnreadInboxForActor(myID, peerAgentID(myID), limit)
	default:
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
			item.ForwardedTo, item.ForwardState = localHomeMailStatus(m.ID)
			item.To = m.ToConv
			item.ToShort = agent.ShortAgentID(m.ToAgent, m.ToConv)
			item.Delivered = !m.DeliveredAt.IsZero() && m.NudgeDiscardedAt.IsZero()
			if !m.NudgeDiscardedAt.IsZero() {
				item.NudgeDiscardedAt = m.NudgeDiscardedAt.Format(time.RFC3339)
			}
		} else {
			item.From = m.FromConv
			item.FromShort = agent.ShortAgentID(m.FromAgent, m.FromConv)
			if db.IsOperatorAgentMessage(m.ID) {
				item.FromShort = "human"
			}
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
