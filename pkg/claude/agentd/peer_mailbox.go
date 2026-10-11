package agentd

import (
	"context"
	"net/http"
)

// Message history is instance-wide and opt-in: a group mail grant permits
// delivery, not inspection of private mail across the node. Mutations are
// separately gated and do not expand human-inbox approval authority.
func addPeerMailboxRules(rules map[string]peerViewRule) {
	for _, pattern := range []string{"GET /api/mailboxes", "GET /api/mailbox"} {
		rules[pattern] = peerViewRule{feature: "node.messages", requires: PermNodeMessagesRead, serve: servePeerMailbox}
	}
	rules["POST /api/mailbox/mark-read"] = peerViewRule{feature: "node.messages.manage", requires: PermNodeMessagesManage, write: servePeerMailbox}
}

func servePeerMailbox(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	// Reuse the production dashboard handler after independent peer authority
	// validation, as other peer read projections do. Only these exact routes
	// can reach it; delete/wipe and human decisions remain classified local-only.
	r = r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
	switch r.URL.Path {
	case "/api/mailboxes":
		handleDashboardMailboxes(w, r)
	case "/api/mailbox":
		handleDashboardMailbox(w, r)
	case "/api/mailbox/mark-read":
		handleDashboardMailboxMarkRead(w, r)
	}
}
