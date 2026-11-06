package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The human inbox across nodes (tcl-99b5ir gap 2). An unrestricted peer
// reads this node's human notifications and pending ask-human requests,
// replies to an agent, and answers a request once. A restricted peer does the
// same within the groups its human.inbox grants name: it reads the notices an
// agent it sees sent in such a group (never a senderless one) and the
// requests of an agent all of whose groups it holds the grant in. It replies
// only to a notice it reads and denies only a request it reads; approving
// also takes the requested slug, as the action is scoped, within the peer
// ceiling, checked when it answers. The requesting node reaches these routes
// only through its dashboard peer proxy or the operator CLI, both human-only;
// no agent, away-cover or mail path calls them. Peer access requests (a peer
// asking this node for grants) and away-cover forwards stay local: only this
// node's own operator decides those.
const (
	PermHumanInboxRead   = "human.inbox.read"
	PermHumanInboxAnswer = "human.inbox.answer"
)

const (
	peerHumanInboxMessages = 50
	// peerHumanInboxScan bounds how many unread notices a restricted peer's
	// listing reads past the ones it may not see.
	peerHumanInboxScan = 2000
	peerHumanInboxBodyMax  = 4 << 10
)

func addPeerHumanInboxRules(rules map[string]peerViewRule) {
	rules["GET /api/human-inbox"] = peerViewRule{feature: "human.inbox", requires: PermHumanInboxRead, group: true, serve: servePeerHumanInbox}
	for _, pattern := range []string{"POST /api/human-inbox/reply", "POST /api/human-inbox/access/{id}"} {
		rules[pattern] = peerViewRule{feature: "human.inbox.answer", requires: PermHumanInboxAnswer, group: true, write: servePeerHumanInboxAnswer}
	}
	// Read state is the one grantable part: a peer holding
	// node.messages.mark-read marks notifications read as opening one does
	// locally, without any answer authority.
	rules["POST /api/human-inbox/read"] = peerViewRule{feature: "node.messages.mark-read", requires: PermNodeMessagesMarkRead, write: servePeerHumanInboxAnswer}
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
	// Approvable says whether this peer may approve the request now; when it
	// may not, ApproveReason says why (it may still deny).
	Approvable    bool   `json:"approvable"`
	ApproveReason string `json:"approve_reason,omitempty"`
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

// peerHumanInboxHeld reports whether a restricted peer's operator holds slug
// in any active group, so the route is theirs at all.
func peerHumanInboxHeld(v *peerView, slug string) bool {
	if v.agent != nil {
		return false
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() && fedPeerAllows(v.peer.InstanceID, g.ID, slug) {
			return true
		}
	}
	return false
}

// peerHumanMessageGroup is the group a restricted peer reads notice m in: one
// an agent sent in a group the peer holds slug in, while the peer still sees
// that agent. A senderless notice is no group's.
func peerHumanMessageGroup(peer string, m *db.HumanMessage, slug string) (*db.AgentGroup, bool) {
	if m == nil || m.FromAgent == "" && m.FromConv == "" || m.GroupName == "" {
		return nil, false
	}
	g, err := db.GetAgentGroupByName(m.GroupName)
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(peer, g.ID, slug) {
		return nil, false
	}
	agentID := m.FromAgent
	if agentID == "" {
		if agentID, err = db.AgentIDForConv(m.FromConv); err != nil || agentID == "" {
			return nil, false
		}
	}
	if !peerSeesAgent(peer, agentID) {
		return nil, false
	}
	return g, true
}

// peerSeesAgent reports whether the agent is in an active group the peer sees.
func peerSeesAgent(peer, agentID string) bool {
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil || a.CurrentConvID == "" {
		return false
	}
	groups, err := db.ListGroupsForConv(a.CurrentConvID)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() && fedPeerGroupVisible(peer, g.ID) {
			return true
		}
	}
	return false
}

// peerHumanInboxRequestSeen reports whether a restricted peer holding slug
// may see (and, with the answer slug, deny) one of this node's own pending
// ask-human requests: the requester's every group carries slug, and what the
// request targets is visible to the peer.
func peerHumanInboxRequestSeen(peer string, req *approvalRequest, slug string) bool {
	if req == nil || req.peerAccess != nil || req.agentID == "" {
		return false
	}
	a, err := db.GetAgent(req.agentID)
	if err != nil || a == nil || a.CurrentConvID == "" || !peerAgentActionAllowed(peer, a.CurrentConvID, slug) {
		return false
	}
	if req.targetGroup != "" {
		g, err := db.GetAgentGroupByName(req.targetGroup)
		if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(peer, g.ID) {
			return false
		}
	}
	if req.targetConvID != "" {
		target, err := db.AgentIDForConv(req.targetConvID)
		if err != nil || target == "" || !peerSeesAgent(peer, target) {
			return false
		}
	}
	return true
}

// peerHumanInboxApproveFailure says why a restricted peer may not approve
// req now, or "" when it may: it answers for the requester, and the slug, as
// the action is scoped, is within the peer ceiling.
func peerHumanInboxApproveFailure(peer string, req *approvalRequest) string {
	if !peerHumanInboxRequestSeen(peer, req, PermHumanInboxAnswer) {
		return "requires " + PermHumanInboxAnswer + " for every group of the requesting agent"
	}
	var scope PermissionScope
	if req.scopeJSON != "" {
		parsed, _, err := parsePermissionScope(json.RawMessage(req.scopeJSON))
		if err != nil {
			return "the request's scope cannot be read; only this node's operator may approve it"
		}
		scope = parsed
	}
	if err := peerCeilingCovers(peer, req.perm, scope); err != nil {
		return "approving it would exceed what you may grant here: " + err.Error()
	}
	return ""
}

func pendingApproval(id string) *approvalRequest {
	approvals.mu.Lock()
	defer approvals.mu.Unlock()
	return approvals.pending[id]
}

func servePeerHumanInbox(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	restricted := !v.unrestricted()
	if restricted && !peerHumanInboxHeld(v, rule.requires) {
		writeError(w, 403, "permission", "requires "+rule.requires)
		return
	}
	peer := v.peer.InstanceID
	// Only unread notifications: the remote inbox is a to-do list, and an
	// unread one must not drop behind newer read ones.
	var all []*db.HumanMessage
	for offset := 0; len(all) < peerHumanInboxMessages && offset < peerHumanInboxScan; {
		page, err := db.ListHumanMessagesPage(db.HumanMessageFilter{Unread: true}, offset, 200)
		if err != nil {
			writeError(w, 500, "io", "list human messages")
			return
		}
		for _, m := range page {
			if len(all) == peerHumanInboxMessages {
				break
			}
			if restricted {
				if _, ok := peerHumanMessageGroup(peer, m, PermHumanInboxRead); !ok {
					continue
				}
			}
			all = append(all, m)
		}
		if !restricted || len(page) < 200 {
			break
		}
		offset += len(page)
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
		approvable, reason := true, ""
		if restricted {
			req := pendingApproval(ar.ID)
			if !peerHumanInboxRequestSeen(peer, req, PermHumanInboxRead) {
				continue
			}
			reason = peerHumanInboxApproveFailure(peer, req)
			approvable = reason == ""
		}
		body, _ := capText(ar.Body, peerHumanInboxBodyMax)
		requests = append(requests, peerAccessRequest{ID: ar.ID, Perm: ar.Perm, AgentID: ar.AgentID, Title: ar.ConvTitle, Path: ar.Path, Body: body,
			TargetGroup: ar.TargetGroup, TargetConvTitle: ar.TargetConvTitle, CreatedAt: ar.CreatedAt, Deadline: ar.Deadline,
			Approvable: approvable, ApproveReason: reason})
	}
	writeJSON(w, 200, map[string]any{"messages": messages, "access_requests": requests})
}

func servePeerHumanInboxAnswer(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	answer := rule.requires == PermHumanInboxAnswer
	if answer && !v.unrestricted() && !peerHumanInboxHeld(v, rule.requires) || !answer && !v.allows(rule, 0) {
		writeError(w, 403, "permission", "requires "+rule.requires)
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
		decidePeerHumanInboxRequest(w, v, r.PathValue("id"), body.Decision)
	case r.URL.Path == "/api/human-inbox/read":
		// The same envelope as this node's own /api/human-messages/read:
		// {"id":N} marks read, {"id":N,"read":false} unread, {"all":true}
		// marks every notification read.
		var body struct {
			ID   int64 `json:"id"`
			All  bool  `json:"all"`
			Read *bool `json:"read"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		if body.All {
			v.auditDetail = "read all"
			n, err := db.MarkAllHumanMessagesRead()
			if err != nil {
				writeError(w, 500, "io", "mark read")
				return
			}
			writeJSON(w, 200, map[string]any{"marked": n})
			return
		}
		if body.ID <= 0 {
			writeError(w, 400, "invalid_arg", "id is required")
			return
		}
		read := body.Read == nil || *body.Read
		var err error
		if read {
			v.auditDetail = fmt.Sprintf("read id=%d", body.ID)
			_, err = db.MarkHumanMessageRead(body.ID)
		} else {
			v.auditDetail = fmt.Sprintf("unread id=%d", body.ID)
			_, err = db.MarkHumanMessageUnread(body.ID)
		}
		if err != nil {
			writeError(w, 500, "io", "mark read")
			return
		}
		writeJSON(w, 200, map[string]any{"id": body.ID, "read": read})
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
		if !v.unrestricted() {
			// Only a notice the peer reads, in a group it answers in.
			g, ok := peerHumanMessageGroup(v.peer.InstanceID, m, PermHumanInboxRead)
			if !ok || !fedPeerAllows(v.peer.InstanceID, g.ID, PermHumanInboxAnswer) {
				writeError(w, 404, "not_found", "no such human message")
				return
			}
			v.groupName = g.Name
		}
		if !humanInboxReplyable(m) {
			writeError(w, 409, "not_replyable", "this notification cannot be answered from another node")
			return
		}
		v.auditDetail = fmt.Sprintf("reply id=%d", body.ID)
		// The agent sees who answered, as with a peer operator message: the
		// reply does not pass as this node's own operator.
		if body.Body != "" {
			body.Body = fedRemoteBanner("operator", peerDisplay(v.peer), v.peer.InstanceID) + body.Body
		}
		raw, _ := json.Marshal(map[string]any{"id": body.ID, "body": body.Body})
		r.Body = io.NopCloser(bytes.NewReader(raw))
		serveHumanMessagesReply(w, r)
	}
}

// decidePeerHumanInboxRequest answers one of this node's own pending ask-human
// requests once, recording the peer as the decider in the approval history.
func decidePeerHumanInboxRequest(w http.ResponseWriter, v *peerView, id, decision string) {
	req := pendingApproval(id)
	if req == nil || req.peerAccess != nil {
		writeError(w, 404, "not_found", "no such pending access request")
		return
	}
	v.auditDetail = fmt.Sprintf("decision=%s id=%s perm=%s", decision, id, req.perm)
	if !v.unrestricted() {
		// Checked now, against the grants as they stand, not as they were
		// when the request was listed.
		peer := v.peer.InstanceID
		if !peerHumanInboxRequestSeen(peer, req, PermHumanInboxRead) {
			writeError(w, 404, "not_found", "no such pending access request")
			return
		}
		why := ""
		if decision == "approve" {
			why = peerHumanInboxApproveFailure(peer, req)
		} else if !peerHumanInboxRequestSeen(peer, req, PermHumanInboxAnswer) {
			why = "requires " + PermHumanInboxAnswer + " for every group of the requesting agent"
		}
		if why != "" {
			writeError(w, 403, "permission", why)
			return
		}
	}
	outcome := outcomeDeny
	if decision == "approve" {
		outcome = outcomeApprove
	}
	req.mu.Lock()
	prev := req.delegatedDecider
	req.delegatedDecider = v.peer.InstanceID
	req.mu.Unlock()
	select {
	case req.decision <- outcome:
		writeJSON(w, 200, map[string]any{"decision": decision})
	default:
		req.mu.Lock()
		req.delegatedDecider = prev
		req.mu.Unlock()
		writeError(w, 409, "already_decided", "this request was already answered")
	}
}
