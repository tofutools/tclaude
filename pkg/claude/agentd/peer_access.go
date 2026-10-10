package agentd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxPeerGrantTTL = 30 * 24 * 60 * 60

// The dispatcher is the requestable permission catalog as well as the action
// authority boundary. A local-only route never becomes grantable by asking.
// Only the compile-time mapping is cached; peer trust and grants are always
// read for the current request. Lazy initialization avoids the handler/mapping
// initialization cycle.
var requestablePeerPermissions struct {
	once        sync.Once
	permissions map[string]bool
}

func requestablePeerPermission(slug string) (group bool, ok bool) {
	requestablePeerPermissions.once.Do(func() {
		permissions := map[string]bool{}
		for _, rule := range peerViewRules() {
			if !rule.unrestrictedOnly && !rule.accessRequest && !rule.visible && !rule.publicRead && (rule.serve != nil || rule.write != nil) {
				permissions[rule.requires] = rule.group
			}
		}
		requestablePeerPermissions.permissions = permissions
	})
	group, ok = requestablePeerPermissions.permissions[slug]
	return group, ok
}
func peerHasAccess(peer string) bool {
	p, err := db.GetFederationPeer(peer)
	if err != nil || p == nil {
		return false
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	meaningful, _ := peerAccessCatalog()
	for _, g := range grants {
		// Public node summaries, obsolete slugs, and grants on deleted groups do
		// not constitute access that may be used to solicit more authority.
		if !slices.Contains(meaningful, g.Slug) {
			continue
		}
		if g.Scope == "" || (g.Slug == PermModelsProxy || g.Slug == PermModelsProxyLeased) && strings.HasPrefix(g.Scope, "http_proxy=") {
			return true
		}
		groupID, err := strconv.ParseInt(strings.TrimPrefix(g.Scope, "group="), 10, 64)
		if err == nil {
			if group, err := db.GetAgentGroupByID(groupID); err == nil && group != nil && !group.IsArchived() {
				return true
			}
		}
	}
	return false
}
func servePeerAccessRequest(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	peer := v.peer.InstanceID
	if !v.allows(rule, 0) {
		writeError(w, 403, "peer_access", "existing peer access required")
		return
	}
	if r.Method == http.MethodGet {
		row, err := db.GetFederationPeerAccessRequest(r.PathValue("id"))
		if err != nil {
			writeError(w, 503, "peer_access", "could not read request")
			return
		}
		if row == nil || row.Peer != peer {
			writeError(w, 404, "not_found", "no such access request")
			return
		}
		if row.Status == db.AccessRequestStatusPending {
			approvals.mu.Lock()
			_, pending := approvals.pending[row.ID]
			approvals.mu.Unlock()
			if !pending {
				row.Status = "interrupted"
			}
		}
		writeJSON(w, 200, row)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var in struct {
		Permission      string `json:"permission"`
		GroupID         int64  `json:"group_id"`
		Reason          string `json:"reason"`
		GrantTTLSeconds *int   `json:"grant_ttl_seconds"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, "invalid_arg", "invalid request body")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid_arg", "one JSON body required")
		return
	}
	groupPermission, ok := requestablePeerPermission(in.Permission)
	if !ok || len(in.Reason) > 4096 || in.GroupID < 0 || !groupPermission && in.GroupID != 0 {
		writeError(w, 400, "invalid_arg", "permission or scope is not requestable")
		return
	}
	// Restricted callers can request a grant only in a group already visible to
	// them. An unscoped request can be narrowed by the receiving operator.
	groupName := ""
	if in.GroupID != 0 {
		group, err := db.GetAgentGroupByID(in.GroupID)
		if err != nil || group == nil || group.IsArchived() || !fedPeerGroupVisible(peer, in.GroupID) {
			writeError(w, 404, "not_found", "no such shared group")
			return
		}
		groupName = group.Name
	}
	ttl := 3600
	if in.GrantTTLSeconds != nil {
		ttl = *in.GrantTTLSeconds
	}
	if ttl < 0 || ttl > maxPeerGrantTTL {
		writeError(w, 400, "invalid_arg", "grant TTL must be 0 (permanent) through 30 days")
		return
	}
	pa := &db.FederationPeerAccessRequest{ID: newApprovalID(), Peer: peer, Slug: in.Permission, GroupID: in.GroupID, GrantGroupID: in.GroupID, GrantTTLSeconds: ttl, Status: db.AccessRequestStatusPending}
	req := &approvalRequest{id: pa.ID, peerAccess: pa, perm: in.Permission, convTitle: "operator@" + peer, method: http.MethodPost, path: "/api/peer-access-requests", bodyPreview: in.Reason, bodyLabel: "Reason", targetGroup: groupName, scopeDisplay: fmt.Sprintf("group_id=%d", in.GroupID), createdAt: time.Now(), timeout: 300 * time.Second, decision: make(chan approvalOutcome, 1), extend: make(chan time.Duration, 1), delegated: make(chan fedAwayDecision, 1)}
	approvals.mu.Lock()
	pending := 0
	for _, other := range approvals.pending {
		if other.peerAccess != nil && other.peerAccess.Peer == peer {
			pending++
		}
	}
	if pending >= 8 {
		approvals.mu.Unlock()
		writeError(w, 429, "peer_access_busy", "too many pending access requests")
		return
	}
	if err := db.UpsertFederationPeerAccessRequest(*pa); err != nil {
		approvals.mu.Unlock()
		writeError(w, 503, "peer_access", "could not persist request")
		return
	}
	// Persist before acknowledging, and register before the waiter runs so an
	// immediate decision cannot be lost. The normal waiter owns all resolution.
	if err := db.UpsertAccessRequest(accessRequestDB(req, db.AccessRequestStatusPending, time.Time{})); err != nil {
		approvals.mu.Unlock()
		writeError(w, 503, "peer_access", "could not persist request")
		return
	}
	approvals.pending[req.id] = req
	approvals.mu.Unlock()
	ack := *pa
	go waitHumanApproval(req, popupBaseURL)
	writeJSON(w, 202, ack)
}
func servePeerAccessDecision(w http.ResponseWriter, req *approvalRequest, decision string, ttl *int, groupID *int64) {
	if decision != "approve" && decision != "deny" {
		writeError(w, 400, "invalid_arg", "peer decisions are approve, deny, or extend")
		return
	}
	req.mu.Lock()
	defer req.mu.Unlock()
	if req.peerDecisionQueued {
		writeError(w, 409, "already_decided", "decision already queued")
		return
	}
	pa := *req.peerAccess
	if decision == "deny" {
		select {
		case req.decision <- outcomeDeny:
			req.peerDecisionQueued = true
			writeJSON(w, 200, map[string]any{"decision": "deny"})
		default:
			writeError(w, 409, "already_decided", "decision already queued")
		}
		return
	}
	requestedTTL := pa.GrantTTLSeconds
	if ttl != nil {
		pa.GrantTTLSeconds = *ttl
	}
	if requestedTTL > 0 && (pa.GrantTTLSeconds == 0 || pa.GrantTTLSeconds > requestedTTL) {
		writeError(w, 400, "invalid_arg", "approval cannot extend the requested grant lifetime")
		return
	}
	if groupID != nil {
		pa.GrantGroupID = *groupID
	}
	isGroup, _ := requestablePeerPermission(pa.Slug)
	if pa.GrantTTLSeconds < 0 || pa.GrantTTLSeconds > maxPeerGrantTTL || pa.GrantGroupID < 0 || !isGroup && pa.GrantGroupID != 0 || pa.GroupID != 0 && pa.GrantGroupID != pa.GroupID {
		writeError(w, 400, "invalid_arg", "approval cannot widen requested scope or TTL bounds")
		return
	}
	if pa.GrantGroupID != 0 {
		g, err := db.GetAgentGroupByID(pa.GrantGroupID)
		if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(pa.Peer, g.ID) {
			writeError(w, 404, "not_found", "no such shared group")
			return
		}
	}
	outcome := outcomeApprove
	if decision == "deny" {
		outcome = outcomeDeny
	}
	select {
	case req.decision <- outcome:
		req.peerDecisionQueued = true
		req.peerAccess.GrantGroupID = pa.GrantGroupID
		req.peerAccess.GrantTTLSeconds = pa.GrantTTLSeconds
		req.peerAccess.ExpiresAt = pa.ExpiresAt
	default:
		writeError(w, 409, "already_decided", "decision already queued")
		return
	}
	writeJSON(w, 200, map[string]any{"decision": decision})
}
func applyPeerAccessOutcome(req *approvalRequest, outcome approvalOutcome) bool {
	req.mu.Lock()
	pa := *req.peerAccess
	req.mu.Unlock()
	approved := false
	scope := ""
	if pa.GrantGroupID != 0 {
		scope = db.FederationGroupScope(pa.GrantGroupID)
	}
	if outcome.approved() {
		meaningful, groupSlugs := peerAccessCatalog()
		approved, _ = db.ApproveFederationPeerAccessRequest(&pa, meaningful, groupSlugs)
	}
	req.mu.Lock()
	req.peerAccess.GrantGroupID = pa.GrantGroupID
	req.peerAccess.GrantTTLSeconds = pa.GrantTTLSeconds
	req.peerAccess.ExpiresAt = pa.ExpiresAt
	req.mu.Unlock()
	if !approved {
		_ = db.UpsertFederationPeerAccessRequest(pa)
	}
	verb := "federation.access.deny"
	status := 403
	if approved {
		verb = "federation.access.approve"
		status = 200
	}
	recordFederationAudit(verb, "operator", "", req.targetGroup, fmt.Sprintf("peer=%s permission=%s scope=%s", pa.Peer, pa.Slug, scope), status)
	return approved
}
func handleFederationAccessRequests(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "peer access requests") {
		return
	}
	if r.Method == http.MethodPost {
		approvals.mu.Lock()
		req := approvals.pending[r.PathValue("id")]
		approvals.mu.Unlock()
		if req == nil || req.peerAccess == nil {
			writeError(w, 404, "not_found", "no such peer request")
			return
		}
		serveAccessRequestDecision(w, r)
		return
	}
	out := []dashboardAccessRequest{}
	for _, row := range approvals.dashboardSnapshot() {
		if row.OriginPeer != "" {
			out = append(out, row)
		}
	}
	writeJSON(w, 200, out)
}
func registerFederationAccessRequestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/access-requests", handleFederationAccessRequests)
	mux.HandleFunc("POST /v1/federation/access-requests/{id}/decision", handleFederationAccessRequests)
}

// Requesting operators see the terminal decision via their owned status read;
// both sides retain its audit without retaining the request reason or body.
func auditPeerAccessProxy(r *http.Request, peer, phase string, status int, body []byte) {
	tail := r.PathValue("tail")
	if r.Method == http.MethodPost && tail == "peer-access-requests" {
		recordFederationAudit("federation.access.out", "operator", "", "", fmt.Sprintf("peer=%s phase=%s", peer, phase), status)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(tail, "peer-access-requests/") && phase == "result" && status == 200 {
		var row db.FederationPeerAccessRequest
		if json.Unmarshal(body, &row) == nil {
			_, idErr := hex.DecodeString(row.ID)
			terminal := row.Status == "approved" || row.Status == "declined" || row.Status == "timed out" || row.Status == "interrupted"
			if idErr == nil && len(row.ID) == 32 && terminal {
				recordFederationAudit("federation.access.out.decision", "operator", "", "", fmt.Sprintf("peer=%s request=%s decision=%s", peer, auditClip(row.ID, 40), auditClip(row.Status, 40)), status)
			}
		}
	}
}

func peerAccessCatalog() (meaningful, groupSlugs []string) {
	for slug := range federationPeerSlugs {
		groupSlugs = append(groupSlugs, slug)
		meaningful = append(meaningful, slug)
	}
	for _, rule := range peerViewRules() {
		if _, ok := requestablePeerPermission(rule.requires); ok {
			meaningful = append(meaningful, rule.requires)
		}
	}
	meaningful = append(meaningful, "config.offer", PermApprovalsAnswer, PermModelsProxy, PermModelsProxyLeased)
	return meaningful, groupSlugs
}

// Agent requests go to their own operator only after that operator already
// sees the receiving group; trust/placement metadata alone is insufficient.
func operatorPeerActionVisible(action ActionContext) bool {
	peer, err := db.GetFederationPeer(action.RemotePeer)
	if err != nil || peer == nil {
		return false
	}
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil || cat == nil {
		return false
	}
	for _, group := range cat.Groups {
		if action.RemoteGroup == "" || group.Name == action.RemoteGroup {
			return true
		}
	}
	return false
}
