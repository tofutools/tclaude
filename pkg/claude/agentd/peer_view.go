package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// PeerViewHandler is the receiving-node UI boundary. Its caller must authenticate
// the transport and supply the pinned instance ID, never a request header or
// browser-selected identity. No route falls through to the local dashboard.
// Trust and grants are rechecked on every request, including existing handlers.
func PeerViewHandler(instanceID string) http.Handler {
	mux := http.NewServeMux()
	for pattern, rule := range peerViewRules() {
		if strings.HasPrefix(pattern, "feature:") {
			continue
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			p, err := db.GetFederationPeer(instanceID)
			if err != nil || p == nil {
				writeError(w, 403, "peer_view", "trusted peer required")
				return
			}
			out := &peerViewResponse{header: make(http.Header)}
			view := &peerView{peer: p}
			if rule.serve == nil && rule.write == nil {
				writeError(out, 403, "peer_view_local_only", "endpoint is local-only")
			} else if r.Method != http.MethodGet && r.Method != http.MethodHead && rule.write == nil {
				writeError(out, 403, "peer_view_local_only", "method is local-only")
			} else if rule.write != nil {
				rule.write(out, r, view, rule)
			} else {
				rule.serve(out, r, view, rule)
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				// Do not audit untrusted input (body, target selector, query) as a label.
				recordFederationAudit("federation.peer_view", "operator@"+instanceID, view.targetConv, view.groupName, r.Method+" "+pattern, out.statusCode())
			}
			if out.statusCode() >= 200 && out.statusCode() < 300 && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				out.addMetadata(view.metadata())
			}
			if rule.summary && out.statusCode() == 200 {
				peerSummaryETag(out, r)
			}
			if out.Header().Get("Cache-Control") == "" {
				out.Header().Set("Cache-Control", "no-store")
			}
			for k, vals := range out.Header() {
				w.Header()[k] = vals
			}
			w.WriteHeader(out.statusCode())
			if r.Method != http.MethodHead {
				_, _ = w.Write(out.body.Bytes())
			}
		})
	}
	return mux
}

type peerViewRule struct {
	feature       string
	requires      string
	group         bool
	visible       bool
	summary       bool
	accessRequest bool
	publicRead    bool
	serve         func(http.ResponseWriter, *http.Request, *peerView, peerViewRule)
	write         func(http.ResponseWriter, *http.Request, *peerView, peerViewRule)
}

// This single mapping drives endpoint dispatch, refusals and omitted features.
// Local-only entries are also classified here; unknown paths match the denied
// root, even for unrestricted peers. Method-specific entries open a route only
// for that method, leaving the original dashboard pattern local-only.
func peerViewRules() map[string]peerViewRule {
	rules := map[string]peerViewRule{}
	for _, pattern := range peerViewLocalRoutes {
		rules[pattern] = peerViewRule{feature: "local_dashboard", requires: "local_only"}
	}
	rules["/"] = peerViewRule{feature: "local_dashboard", requires: "local_only"}
	for _, pattern := range []string{"POST /api/peer-access-requests", "GET /api/peer-access-requests/{id}"} {
		rule := peerViewRule{feature: "permissions.requests", requires: "peer_access", accessRequest: true}
		if strings.HasPrefix(pattern, "GET ") {
			rule.serve = servePeerAccessRequest
		} else {
			rule.write = servePeerAccessRequest
		}
		rules[pattern] = rule
	}
	rules["GET /api/snapshot"] = peerViewRule{feature: "agents.status", requires: PermAgentsStatusRead, group: true, serve: servePeerSnapshot}
	rules["GET /api/groups"] = peerViewRule{feature: "groups", requires: PermGroupsRosterRead, group: true, visible: true, serve: servePeerGroups}
	rules["GET /api/groups/{name}"] = peerViewRule{feature: "groups", requires: PermGroupsRosterRead, group: true, visible: true, serve: servePeerGroup}
	rules["GET /api/agents/{id}"] = peerViewRule{feature: "agents.status", requires: PermAgentsStatusRead, group: true, serve: servePeerAgent}
	rules["GET /api/harnesses/operations"] = peerViewRule{feature: "node.harnesses.install", requires: PermNodeHarnessesInstall, serve: servePeerHarnessOperations}
	rules["GET /api/harnesses/operations/jobs/{id}"] = peerViewRule{feature: "node.harnesses.install", requires: PermNodeHarnessesInstall, serve: servePeerHarnessOperations}
	rules["POST /api/harnesses/operations"] = peerViewRule{feature: "node.harnesses.install", requires: PermNodeHarnessesInstall, write: servePeerHarnessOperations}
	for _, path := range []string{"GET /api/harnesses/credentials/backups", "POST /api/harnesses/credentials/push", "POST /api/harnesses/credentials/backup", "POST /api/harnesses/credentials/restore"} {
		rule := peerViewRule{feature: "node.credentials.receive", requires: PermNodeCredentialsReceive}
		if strings.HasPrefix(path, "GET ") {
			rule.serve = servePeerHarnessCredentials
		} else {
			rule.write = servePeerHarnessCredentials
		}
		rules[path] = rule
	}
	rules["POST /api/harnesses/credentials"] = peerViewRule{feature: "node.credentials.receive", requires: PermNodeCredentialsReceive, write: servePeerHarnessOperations}
	for _, pattern := range []string{"GET /api/node/run", "POST /api/node/run", "GET /api/node/run/jobs/{id}", "GET /api/node/run/jobs/{id}/logs"} {
		rule := peerViewRule{feature: "node.exec", requires: PermNodeExec}
		if strings.HasPrefix(pattern, "GET ") {
			rule.serve = servePeerNodeRun
		} else {
			rule.write = servePeerNodeRun
		}
		rules[pattern] = rule
	}
	rules["GET /api/node/update"] = peerViewRule{feature: "node.update", requires: PermNodeUpdate, serve: servePeerNodeUpdate}
	rules["GET /api/node/update/jobs/{id}"] = peerViewRule{feature: "node.update", requires: PermNodeUpdate, serve: servePeerNodeUpdate}
	rules["POST /api/node/update"] = peerViewRule{feature: "node.update", requires: PermNodeUpdate, write: servePeerNodeUpdate}
	rules["GET /api/harnesses/availability"] = peerViewRule{feature: "node.harnesses", requires: PermNodeHarnessesRead, serve: servePeerHarnessAvailability}
	rules["GET /api/node-summary"] = peerViewRule{feature: "node.summary", publicRead: true, summary: true, serve: servePeerSummary}
	rules["GET /api/instance"] = peerViewRule{feature: "health", requires: PermNodeRead, serve: servePeerNode}
	rules["GET /api/costs"] = peerViewRule{feature: "costs", requires: PermCostsRead, serve: servePeerGlobalRead(handleDashboardCosts)}
	rules["GET /api/audit"] = peerViewRule{feature: "audit", requires: PermFederationAuditRead, serve: servePeerGlobalRead(handleDashboardAudit)}
	rules["POST /api/groups/{name}/spawn"] = peerViewRule{feature: "spawn", requires: PermGroupsMembersSpawn, group: true, write: servePeerSpawn}
	rules["GET /api/spawn-requests/{id}"] = peerViewRule{feature: "spawn", requires: PermGroupsMembersSpawn, group: true, serve: servePeerSpawnStatus}
	rules["POST /api/operator-message"] = peerViewRule{feature: "messaging", requires: PermMessageDirect, group: true, write: servePeerMessage}
	// Concepts consumed by the UI, whose transports remain outside this contract.
	rules["/api/term/"] = peerViewRule{feature: "terminals", requires: PermSessionsAttach, group: true}
	rules["/api/spawn"] = peerViewRule{feature: "spawn.inline", requires: PermGroupsMembersSpawn, group: true}
	for _, action := range []struct{ tail, permission string }{{"stop", PermGroupsMembersStop}, {"retire", PermGroupsMembersRetire}, {"clone", PermGroupsMembersClone}, {"move", PermAgentMove}, {"teleport", PermAgentMove}} {
		rules["POST /api/agents/{id}/"+action.tail] = peerViewRule{feature: "lifecycle." + action.tail, requires: action.permission, group: true, write: servePeerAgentAction}
	}
	rules["feature:roster"] = peerViewRule{feature: "groups.roster", requires: PermGroupsRosterRead, group: true, serve: servePeerGroups}
	rules["feature:presence"] = peerViewRule{feature: "groups.presence", requires: PermGroupsPresenceRead, group: true, serve: servePeerGroups}
	return rules
}

type peerView struct {
	peer       *db.FederationPeer
	targetConv string
	groupName  string
}
type peerViewOmission struct {
	Feature  string `json:"feature"`
	Requires string `json:"requires"`
}
type peerViewMetadata struct {
	Peer     string             `json:"peer"`
	Included []string           `json:"included"`
	Omitted  []peerViewOmission `json:"omitted"`
}

func (v *peerView) allows(rule peerViewRule, groupID int64) bool {
	if v != nil && rule.accessRequest {
		return peerHasAccess(v.peer.InstanceID)
	}
	if v == nil || rule.publicRead {
		return true
	}
	if rule.requires == "local_only" {
		return false
	}
	if rule.visible {
		return fedPeerGroupVisible(v.peer.InstanceID, groupID)
	}
	if rule.group {
		return fedPeerAllows(v.peer.InstanceID, groupID, rule.requires)
	}
	if db.FederationPeerUnrestricted(v.peer.InstanceID) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(v.peer.InstanceID)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Slug == rule.requires && g.Scope == "" {
			return true
		}
	}
	return false
}
func (v *peerView) metadata() peerViewMetadata {
	out := peerViewMetadata{Peer: peerDisplay(v.peer), Included: []string{}, Omitted: []peerViewOmission{}}
	groups, _ := db.ListAgentGroups()
	features := map[string]peerViewRule{}
	for _, rule := range peerViewRules() {
		features[rule.feature] = rule
	}
	for _, rule := range features {
		allowed := v.allows(rule, 0)
		if rule.group {
			for _, g := range groups {
				if !g.IsArchived() && v.allows(rule, g.ID) {
					allowed = true
					break
				}
			}
		}
		// A feature whose transport is local-only is always omitted, even if the
		// peer holds its related federation grant (e.g. terminal attach).
		if rule.serve == nil && rule.write == nil {
			allowed = false
		}
		if allowed {
			out.Included = append(out.Included, rule.feature)
		} else {
			out.Omitted = append(out.Omitted, peerViewOmission{rule.feature, rule.requires})
		}
	}
	sort.Strings(out.Included)
	sort.Slice(out.Omitted, func(i, j int) bool { return out.Omitted[i].Feature < out.Omitted[j].Feature })
	return out
}

type peerViewResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *peerViewResponse) Header() http.Header { return w.header }
func (w *peerViewResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *peerViewResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}
func (w *peerViewResponse) statusCode() int {
	if w.status == 0 {
		return 200
	}
	return w.status
}
func (w *peerViewResponse) addMetadata(meta peerViewMetadata) {
	var obj map[string]any
	if json.Unmarshal(w.body.Bytes(), &obj) != nil || obj == nil {
		return
	}
	obj["peer_view"] = meta
	w.body.Reset()
	_ = json.NewEncoder(&w.body).Encode(obj)
	w.Header().Del("Content-Length")
}

// Instance grants authorize the entire concept; scoped group grants never
// imply costs or audit visibility. Only these read handlers use dashboard auth
// internally after the independent peer check. No mutation can take this path.
func servePeerGlobalRead(next http.HandlerFunc) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !v.allows(rule, 0) {
			writeJSON(w, 200, map[string]any{"rows": []any{}})
			return
		}
		r = r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
		next(w, r)
	}
}
func servePeerNode(w http.ResponseWriter, _ *http.Request, v *peerView, rule peerViewRule) {
	out := map[string]any{}
	if v.allows(rule, 0) {
		out["node"] = localNodeMetadata()
	}
	writeJSON(w, 200, out)
}

func servePeerSnapshot(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if db.FederationPeerUnrestricted(v.peer.InstanceID) {
		// Unrestricted trust grants complete collection visibility, while route
		// classification still limits the available actions.
		ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), peerSnapshotCtxKey{}, true)
		handleDashboardSnapshot(w, r.Clone(ctx))
		return
	}
	out, err := v.snapshot()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	writeJSON(w, 200, out)
}
func servePeerGroups(w http.ResponseWriter, _ *http.Request, v *peerView, _ peerViewRule) {
	groups, _, err := v.groups()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"groups": groups})
}
func servePeerGroup(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	groups, _, err := v.groups()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	for _, g := range groups {
		if g.Name == r.PathValue("name") {
			writeJSON(w, 200, map[string]any{"group": g})
			return
		}
	}
	writeError(w, 404, "not_found", "object not found")
}
func servePeerAgent(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	_, agents, err := v.groups()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	for _, a := range agents {
		if a.AgentID == r.PathValue("id") {
			writeJSON(w, 200, map[string]any{"agent": a})
			return
		}
	}
	writeError(w, 404, "not_found", "object not found")
}
func servePeerMessage(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	var in struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, operatorMessageMaxBody+4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || strings.TrimSpace(in.Body) == "" || len(in.Body) > operatorMessageMaxBody || len([]rune(in.Subject)) > operatorMessageMaxSubject {
		writeError(w, 400, "invalid_arg", "expected to, subject and nonempty body within message limits")
		return
	}
	// Resolve only the filtered roster: an invisible target yields no candidates
	// and no names, regardless of aliases or ambiguous local selectors.
	_, agents, err := v.groups()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	var target *db.Agent
	for _, a := range agents {
		if a.AgentID == in.To {
			target, _ = db.GetAgent(a.AgentID)
			break
		}
	}
	if target == nil {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	groups, _ := db.ListGroupsForConv(target.CurrentConvID)
	var gid int64
	var visible string
	for _, g := range groups {
		if fedPeerGroupVisible(v.peer.InstanceID, g.ID) && visible == "" {
			visible = g.Name
		}
		if v.allows(rule, g.ID) {
			gid = g.ID
			break
		}
	}
	v.targetConv = target.CurrentConvID
	v.groupName = visible
	if gid == 0 {
		writeError(w, 403, "permission", fmt.Sprintf("requires %s for group:%s", rule.requires, visible))
		return
	}
	if g, _ := db.GetAgentGroupByID(gid); g != nil {
		v.groupName = g.Name
	}
	name := "operator"
	id, err := db.InsertFederationInboundMessage(&db.AgentMessage{GroupID: gid, ToConv: target.CurrentConvID, Subject: in.Subject, Body: fedRemoteBanner("operator", peerDisplay(v.peer), v.peer.InstanceID) + in.Body, ToRecipients: []string{target.CurrentConvID}}, db.FederationInbound{EnvelopeID: uuid.NewString(), FromInstance: v.peer.InstanceID, FromName: name}, time.Now().Add(24*time.Hour), regularAgentMessageQueueLimit, nil)
	if err != nil {
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, target.CurrentConvID, full)
			return
		}
		writeError(w, 500, "io", "could not queue message")
		return
	}
	enqueueDeliveryForConv(target.CurrentConvID)
	writeJSON(w, 202, map[string]any{"id": id, "queued": true, "to": target.AgentID})
}
