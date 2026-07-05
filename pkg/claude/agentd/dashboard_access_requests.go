package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Access requests — the in-dashboard home of the human-approval flow.
//
// Historically an agent that hit a permission-gated action it couldn't
// self-satisfy raised a BROWSER POPUP: the daemon xdg-open'd a loopback-only
// /approve/{id} page on the HOST and blocked the agent until the human
// clicked. That popup can't reach a remote operator (a phone never receives
// the host's browser launch), so once the dashboard can be exposed off
// loopback the approval has to live INSIDE the dashboard the operator already
// has open.
//
// This file surfaces in-flight approvals plus persisted handled history on the
// 2s snapshot (the Messages tab's "Access requests" folder + the attention
// overlay render off it) and adds one dashboard-authed decision endpoint. The
// in-memory registry (popup.go) remains the only actionable waiter store, while
// handled cards come back from SQLite so they survive agentd restarts. Because
// the routes ride checkDashboardAuth they work on both the loopback listener
// and the remote (mTLS) listener.

// setDeadline records the wall-clock instant the auto-deny timer will fire,
// under the request mutex. Called by the approval waiter at start and on each
// extend so the dashboard countdown reflects "+extend" clicks.
func (req *approvalRequest) setDeadline(t time.Time) {
	req.mu.Lock()
	req.deadline = t
	req.mu.Unlock()
}

// dashboardAccessRequest is the wire shape of one pending approval on the
// snapshot. All string fields but Deadline are immutable after the request is
// constructed (set once at creation), matching how the tray snapshot reads
// them lock-free; only deadline is mutated, and dashboardSnapshot reads it
// under the request mutex.
type dashboardAccessRequest struct {
	ID        string `json:"id"`
	Perm      string `json:"perm"`
	ConvID    string `json:"conv_id,omitempty"`
	ConvTitle string `json:"conv_title,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	// Path / Body / BodyLabel describe the action being gated (the HTTP path
	// the agent called and a prettified body preview) so the operator can see
	// WHAT they are approving, not just which slug.
	Path      string `json:"path,omitempty"`
	Body      string `json:"body,omitempty"`
	BodyLabel string `json:"body_label,omitempty"`
	// Target* are populated for cross-agent / group-scoped actions — the
	// group or the other conversation the action touches.
	TargetGroup     string `json:"target_group,omitempty"`
	TargetConvID    string `json:"target_conv_id,omitempty"`
	TargetConvTitle string `json:"target_conv_title,omitempty"`
	// AutoGrantable gates the "Always allow for this agent" button; the
	// decision endpoint re-checks it server-side so a hand-crafted POST can't
	// persist an ineligible slug.
	AutoGrantable bool `json:"auto_grantable,omitempty"`
	// CreatedAt / Deadline drive the "waiting Xs / auto-declines in Ys"
	// countdown. RFC3339.
	CreatedAt string `json:"created_at"`
	Deadline  string `json:"deadline,omitempty"`
	// Status is "pending" for an actionable request, or the decided outcome
	// ("approved" | "declined" | "always" | "timed out") for a recently-handled
	// one the folder shows as history. DecidedAt (RFC3339) is set only for a
	// handled entry.
	Status    string `json:"status"`
	DecidedAt string `json:"decided_at,omitempty"`
}

// toDashboardAccessRequest builds the common wire fields from a request. The
// caller stamps Status (+ DecidedAt for a handled entry). Reads only set-once
// fields plus the mutable deadline (under the request mutex).
func toDashboardAccessRequest(req *approvalRequest) dashboardAccessRequest {
	req.mu.Lock()
	deadline := req.deadline
	req.mu.Unlock()
	if deadline.IsZero() {
		// The waiter hasn't stamped a live deadline yet (a brief window between
		// registry insert and the waiter's first line): fall back to the
		// request's own createdAt + timeout.
		deadline = req.createdAt.Add(req.timeout)
	}
	return dashboardAccessRequest{
		ID:              req.id,
		Perm:            req.perm,
		ConvID:          req.convID,
		ConvTitle:       req.convTitle,
		AgentID:         peerAgentID(req.convID),
		Path:            req.path,
		Body:            req.bodyPreview,
		BodyLabel:       req.bodyLabel,
		TargetGroup:     req.targetGroup,
		TargetConvID:    req.targetConvID,
		TargetConvTitle: req.targetConvTitle,
		AutoGrantable:   req.autoGrantable,
		CreatedAt:       req.createdAt.Format(time.RFC3339),
		Deadline:        deadline.Format(time.RFC3339),
	}
}

func accessRequestDB(req *approvalRequest, status string, decidedAt time.Time) *db.AccessRequest {
	req.mu.Lock()
	deadline := req.deadline
	req.mu.Unlock()
	if deadline.IsZero() && !req.createdAt.IsZero() && req.timeout > 0 {
		deadline = req.createdAt.Add(req.timeout)
	}
	return &db.AccessRequest{
		ID:              req.id,
		Perm:            req.perm,
		ConvID:          req.convID,
		ConvTitle:       req.convTitle,
		Method:          req.method,
		Path:            req.path,
		RawQuery:        req.rawQuery,
		BodyPreview:     req.bodyPreview,
		BodyLabel:       req.bodyLabel,
		TargetGroup:     req.targetGroup,
		TargetConvID:    req.targetConvID,
		TargetConvTitle: req.targetConvTitle,
		AutoGrantable:   req.autoGrantable,
		Status:          status,
		CreatedAt:       req.createdAt,
		DeadlineAt:      deadline,
		DecidedAt:       decidedAt,
	}
}

func dbAccessRequestToDashboard(ar *db.AccessRequest) dashboardAccessRequest {
	out := dashboardAccessRequest{
		ID:              ar.ID,
		Perm:            ar.Perm,
		ConvID:          ar.ConvID,
		ConvTitle:       ar.ConvTitle,
		AgentID:         ar.AgentID,
		Path:            ar.Path,
		Body:            ar.BodyPreview,
		BodyLabel:       ar.BodyLabel,
		TargetGroup:     ar.TargetGroup,
		TargetConvID:    ar.TargetConvID,
		TargetConvTitle: ar.TargetConvTitle,
		AutoGrantable:   ar.AutoGrantable,
		Status:          ar.Status,
	}
	if !ar.CreatedAt.IsZero() {
		out.CreatedAt = ar.CreatedAt.Format(time.RFC3339)
	}
	if !ar.DeadlineAt.IsZero() {
		out.Deadline = ar.DeadlineAt.Format(time.RFC3339)
	}
	if !ar.DecidedAt.IsZero() {
		out.DecidedAt = ar.DecidedAt.Format(time.RFC3339)
	}
	return out
}

// dashboardSnapshot returns the access-requests list for the dashboard: the
// in-flight (pending) approvals oldest-first — the longest-blocked agent leads,
// where the operator's eye lands — followed by the recently-handled history
// newest-first, so "what did I just decide" reads top-down under the pending
// ones. Safe from any goroutine; takes the registry mutex briefly to copy, then
// each request's mutex only to read its live deadline.
func (a *approvalRegistry) dashboardSnapshot() []dashboardAccessRequest {
	a.mu.Lock()
	pending := make([]*approvalRequest, 0, len(a.pending))
	for _, req := range a.pending {
		pending = append(pending, req)
	}
	a.mu.Unlock()

	out := make([]dashboardAccessRequest, 0, len(pending))
	for _, req := range pending {
		ar := toDashboardAccessRequest(req)
		ar.Status = "pending"
		out = append(out, ar)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	handled, err := db.ListRecentHandledAccessRequests(maxResolvedApprovals)
	if err != nil {
		slog.Warn("access requests: failed to load handled history", "err", err)
		return out
	}
	for _, ar := range handled {
		out = append(out, dbAccessRequestToDashboard(ar))
	}
	return out
}

// accessRequestDeepLinkQuery is the query fragment (no leading '?') that opens
// the dashboard's Messages tab focused on a specific approval. The auto-raise
// browser launch and the tray menu build a full URL around it; the dashboard
// JS reads tab + access_request to select the folder and pop the overlay.
func accessRequestDeepLinkQuery(id string) string {
	return "tab=messages&access_request=" + url.QueryEscape(id)
}

func registerDashboardAccessRequestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/access-requests/{id}/decision", handleDashboardAccessRequestDecision)
}

// handleDashboardAccessRequestDecision records the operator's decision on a
// pending approval from within the dashboard — the remote-capable replacement
// for the loopback /approve/{id} POST. Cookie + host-relative Origin (or a
// pre-authed remote request) are the human-consent layer, the same gate every
// other dashboard mutation rides; there is no per-approval popup token because
// the operator is already an authenticated dashboard session.
//
// Body: {"decision":"approve|always|deny|extend","secs":<n>}. "always" is
// gated server-side on the request's AutoGrantable flag so a hand-crafted POST
// can't self-grant an ineligible slug even though the frontend hides the
// button. The send onto req.decision is non-blocking (the channel is buffered
// cap 1 and the waiter consumes exactly one); the waiter runs
// applyApprovalOutcome so audit + always-allow persistence happen exactly once
// for the decision that took effect.
func handleDashboardAccessRequestDecision(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing approval id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var body struct {
		Decision string `json:"decision"`
		Secs     int    `json:"secs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}

	approvals.mu.Lock()
	req, ok := approvals.pending[id]
	approvals.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such access request (already decided, expired, or unknown)")
		return
	}

	switch strings.ToLower(strings.TrimSpace(body.Decision)) {
	case "approve":
		select {
		case req.decision <- outcomeApprove:
		default:
		}
		writeJSON(w, http.StatusOK, map[string]any{"decision": "approve"})
	case "always":
		if !req.autoGrantable {
			writeError(w, http.StatusForbidden, "not_grantable", "this permission is not eligible for \"always allow\"")
			return
		}
		select {
		case req.decision <- outcomeApproveAlways:
		default:
		}
		writeJSON(w, http.StatusOK, map[string]any{"decision": "always"})
	case "deny":
		select {
		case req.decision <- outcomeDeny:
		default:
		}
		writeJSON(w, http.StatusOK, map[string]any{"decision": "deny"})
	case "extend":
		// Bounded so an unattended request still eventually auto-denies.
		// Default +5m; secs caps at 300 to match the daemon's AskHuman ceiling.
		extendBy := 5 * time.Minute
		if body.Secs > 0 {
			n := body.Secs
			if n > 300 {
				n = 300
			}
			extendBy = time.Duration(n) * time.Second
		}
		select {
		case req.extend <- extendBy:
		default:
		}
		writeJSON(w, http.StatusOK, map[string]any{"decision": "extend", "extended_by_secs": int(extendBy / time.Second)})
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "decision must be approve, always, deny, or extend")
	}
}
