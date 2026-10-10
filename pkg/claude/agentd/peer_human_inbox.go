package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The human inbox across one operator's own nodes (tcl-99b5ir gap 2). An
// unrestricted peer — and only one: these slugs cannot be granted or
// requested — reads this node's human notifications and pending ask-human
// requests, replies to an agent, and answers a request once. The requesting
// node reaches these routes only through its dashboard peer proxy or the
// operator CLI, both human-only; no agent, away-cover or mail path calls them.
// Peer access requests (a peer asking this node for grants) and away-cover
// forwards stay local: only this node's own operator decides those.
const (
	PermHumanInboxRead   = "human.inbox.read"
	PermHumanInboxAnswer = "human.inbox.answer"
)

const (
	peerHumanInboxMessages = 50
	peerHumanInboxBodyMax  = 4 << 10
)

func addPeerHumanInboxRules(rules map[string]peerViewRule) {
	rules["GET /api/human-inbox"] = peerViewRule{feature: "human.inbox", requires: PermHumanInboxRead, unrestrictedOnly: true, serve: servePeerHumanInbox}
	for _, pattern := range []string{"POST /api/human-inbox/reply", "POST /api/human-inbox/read", "POST /api/human-inbox/access/{id}"} {
		rules[pattern] = peerViewRule{feature: "human.inbox.answer", requires: PermHumanInboxAnswer, unrestrictedOnly: true, write: servePeerHumanInboxAnswer}
	}
}

type peerHumanMessage struct {
	ID          int64    `json:"id"`
	FromAgent   string   `json:"from_agent,omitempty"`
	FromTitle   string   `json:"from_title,omitempty"`
	Group       string   `json:"group,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	Body        string   `json:"body"`
	Truncated   bool     `json:"truncated,omitempty"`
	CreatedAt   string   `json:"created_at"`
	Read        bool     `json:"read"`
	Replyable   bool     `json:"replyable"`
	Attachments []string `json:"attachments,omitempty"`
}

type peerAccessRequest struct {
	ID              string `json:"id"`
	Perm            string `json:"perm"`
	AgentID         string `json:"agent_id,omitempty"`
	Title           string `json:"title,omitempty"`
	Path            string `json:"path,omitempty"`
	Body            string `json:"body,omitempty"`
	TargetGroup     string `json:"target_group,omitempty"`
	TargetConvTitle string `json:"target_conv_title,omitempty"`
	CreatedAt       string `json:"created_at"`
	Deadline        string `json:"deadline,omitempty"`
}

func capText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// humanInboxReplyable mirrors the local reply rules a remote answer may use:
// an agent sender, and never a process obligation (resolving one is local).
func humanInboxReplyable(m *db.HumanMessage) bool {
	return m != nil && m.ProcessCommandID == "" && (m.FromAgent != "" || m.FromConv != "")
}

// localPendingAccessRequests are the ask-human requests of this node's own
// agents, without peer access requests.
func localPendingAccessRequests() []dashboardAccessRequest {
	var out []dashboardAccessRequest
	for _, ar := range approvals.dashboardSnapshot() {
		if ar.Status == "pending" && ar.OriginPeer == "" {
			out = append(out, ar)
		}
	}
	return out
}

func servePeerHumanInbox(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, 403, "permission", "requires unrestricted trust ("+rule.requires+")")
		return
	}
	all, err := db.ListHumanMessages()
	if err != nil {
		writeError(w, 500, "io", "list human messages")
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	if len(all) > peerHumanInboxMessages {
		all = all[:peerHumanInboxMessages]
	}
	messages := make([]peerHumanMessage, 0, len(all))
	for _, m := range all {
		body, cut := capText(m.Body, peerHumanInboxBodyMax)
		pm := peerHumanMessage{ID: m.ID, FromAgent: m.FromAgent, FromTitle: m.FromTitle, Group: m.GroupName, Subject: m.Subject,
			Body: body, Truncated: cut, CreatedAt: m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"), Read: !m.ReadAt.IsZero(), Replyable: humanInboxReplyable(m)}
		for _, a := range m.Attachments {
			pm.Attachments = append(pm.Attachments, a.Filename)
		}
		messages = append(messages, pm)
	}
	requests := []peerAccessRequest{}
	for _, ar := range localPendingAccessRequests() {
		body, _ := capText(ar.Body, peerHumanInboxBodyMax)
		requests = append(requests, peerAccessRequest{ID: ar.ID, Perm: ar.Perm, AgentID: ar.AgentID, Title: ar.ConvTitle, Path: ar.Path, Body: body,
			TargetGroup: ar.TargetGroup, TargetConvTitle: ar.TargetConvTitle, CreatedAt: ar.CreatedAt, Deadline: ar.Deadline})
	}
	writeJSON(w, 200, map[string]any{"messages": messages, "access_requests": requests})
}

func servePeerHumanInboxAnswer(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, 403, "permission", "requires unrestricted trust ("+rule.requires+")")
		return
	}
	switch {
	case r.PathValue("id") != "":
		var body struct {
			Decision string `json:"decision"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		// One-shot only: "always allow" would leave a lasting grant, and
		// extending a deadline is the local operator's call.
		if body.Decision != "approve" && body.Decision != "deny" {
			writeError(w, 400, "invalid_arg", "decision must be approve or deny (one-shot only)")
			return
		}
		found := false
		for _, ar := range localPendingAccessRequests() {
			if ar.ID == r.PathValue("id") {
				found = true
				break
			}
		}
		if !found {
			writeError(w, 404, "not_found", "no such pending access request")
			return
		}
		v.auditDetail = "decision=" + body.Decision
		r.Body = io.NopCloser(bytes.NewReader([]byte(`{"decision":"` + body.Decision + `"}`)))
		serveAccessRequestDecision(w, r)
	case r.URL.Path == "/api/human-inbox/read":
		var body struct {
			ID int64 `json:"id"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		if body.ID <= 0 {
			writeError(w, 400, "invalid_arg", "id is required")
			return
		}
		if _, err := db.MarkHumanMessageRead(body.ID); err != nil {
			writeError(w, 500, "io", "mark read")
			return
		}
		writeJSON(w, 200, map[string]any{"id": body.ID, "read": true})
	default:
		var body struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		m, err := db.GetHumanMessage(body.ID)
		if err != nil || m == nil {
			writeError(w, 404, "not_found", "no such human message")
			return
		}
		if !humanInboxReplyable(m) {
			writeError(w, 409, "not_replyable", "this notification cannot be answered from another node")
			return
		}
		v.auditDetail = "reply"
		raw, _ := json.Marshal(map[string]any{"id": body.ID, "body": body.Body})
		r.Body = io.NopCloser(bytes.NewReader(raw))
		serveHumanMessagesReply(w, r)
	}
}
