package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A peer's file downloads from this node: the staged-download path
// (dashboard_peer_downloads.go) runs a download route for the peer under that
// route's own peer rule. Unrestricted trust stages any of peerDownloadRoutes
// as this node's operator does. A restricted peer stages only a route with a
// peer gate here, which its grants pass. The gate runs again at every chunk
// read, so a grant revoked after staging stops the download.
//
// The other staged downloads (snapshot, template and log exports) and the
// staged upload path (spawn attachments) stay unrestricted only.

// peerStageGate checks a restricted peer's request on one download route. It
// answers 0 to allow it, or the refusal's status, code and message.
type peerStageGate func(v *peerView, r *http.Request) (int, string, string)

// peerStageGates are the download routes a restricted peer may stage.
var peerStageGates = map[string]peerStageGate{
	"GET /api/groups/{name}/export": peerGroupExportGate,
	"GET /api/agent-bundle/export":  peerAgentBundleExportGate,
	"GET /api/config-bundle/export": peerConfigExportGate,
}

// peerStageRewrites adjust a gated download's request before its gate judges
// it and it is staged.
var peerStageRewrites = map[string]func(v *peerView, sub *http.Request){
	"GET /api/config-bundle/export": peerConfigExportSkips,
}

func addPeerTransferRules(rules map[string]peerViewRule) {
	// Any trusted peer may ask to stage and read its own staged files; the
	// staged route's own rule decides.
	rules["/api/peer-downloads"] = peerViewRule{feature: "transfer.downloads", publicRead: true, write: servePeerDownloadStage}
	rules["GET /api/peer-downloads/{id}"] = peerViewRule{feature: "transfer.downloads", publicRead: true, serve: servePeerDownloadChunk}
	// A peer view job, read by the peer that started it (servePeerViewJob).
	rules["GET /api/peer-view-jobs/{id}"] = peerViewRule{feature: "transfer.downloads", publicRead: true, serve: servePeerViewJob}
	rules["GET /api/config-bundle/export"] = peerViewRule{feature: "config.export", requires: PermConfigExport, serve: servePeerStageable(peerConfigExportGate)}
	rules["GET /api/groups/{name}/export"] = peerViewRule{feature: "groups.export", requires: PermGroupsExport, group: true, serve: servePeerStageable(peerGroupExportGate)}
	rules["GET /api/agent-bundle/export"] = peerViewRule{feature: "agent.bundle.export", requires: PermAgentBundleExport, group: true, serve: servePeerStageable(peerAgentBundleExportGate)}
}

// peerAsLocalRequest is r as this node's own dashboard handler reads it.
func peerAsLocalRequest(r *http.Request) *http.Request {
	return r.Clone(context.WithValue(withImportForwarded(r.Context()), remoteAuthedCtxKey{}, true))
}

// servePeerStageable serves a download route directly through the peer view
// to unrestricted trust only. A restricted peer downloads through the staged
// path, which is audited, counted against its jobs and rechecked per chunk;
// its gate names what a refused staging lacks.
func servePeerStageable(gate peerStageGate) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
		if !v.unrestricted() {
			if status, code, msg := gate(v, r); status != 0 {
				writeError(w, status, code, msg)
				return
			}
			writeError(w, http.StatusForbidden, "staged_only", "download this through /api/peer-downloads")
			return
		}
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
	}
}

// servePeerDownloadStage stages a download for the peer, under its route's
// own peer rule.
func servePeerDownloadStage(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	sub, ok := peerDownloadTarget(w, peerAsLocalRequest(r))
	if !ok {
		return
	}
	peer := v.peer.InstanceID
	mux := peerDashboardMux()
	if _, pattern := mux.Handler(sub); !peerDownloadRoutes[pattern] {
		writeError(w, http.StatusForbidden, "not_downloadable", "that route is not a download")
		return
	}
	if v.unrestricted() {
		stagePeerDownloadFile(w, sub, mux, peer, func() bool { return db.FederationPeerUnrestricted(peer) })
		return
	}
	pattern, matched := peerStageMatch(sub)
	gate := peerStageGates[pattern]
	if gate == nil {
		writeError(w, http.StatusForbidden, "peer_view_local_only", "this download requires unrestricted trust")
		return
	}
	if rewrite := peerStageRewrites[pattern]; rewrite != nil {
		// Before the gate, which then judges (and every chunk rechecks)
		// exactly what is staged.
		rewrite(v, sub)
		matched.URL.RawQuery = sub.URL.RawQuery
	}
	if status, code, msg := gate(v, matched); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	v.groupName = matched.PathValue("name")
	stagePeerDownloadFile(w, sub, mux, peer, func() bool {
		p, err := db.GetFederationPeer(peer)
		if err != nil || p == nil {
			return false
		}
		status, _, _ := gate(&peerView{peer: p}, matched)
		return status == 0
	})
}

// servePeerDownloadChunk serves one chunk of a download this peer staged.
func servePeerDownloadChunk(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	servePeerDownloadChunkFor(w, r, v.peer.InstanceID)
}

// peerStageMatch matches sub to its gated download route, with its path
// values set ("" when no gated route matches).
func peerStageMatch(sub *http.Request) (string, *http.Request) {
	var pattern string
	var matched *http.Request
	mux := http.NewServeMux()
	for p := range peerStageGates {
		mux.HandleFunc(p, func(_ http.ResponseWriter, r *http.Request) { pattern, matched = p, r })
	}
	mux.ServeHTTP(&peerViewResponse{header: make(http.Header)}, sub)
	return pattern, matched
}

// peerQueryWithin refuses a query parameter outside allowed: the options a
// restricted peer may pass (carrying workflows, a flagged export and the
// rest take unrestricted trust).
func peerQueryWithin(r *http.Request, allowed ...string) (int, string, string) {
	for key := range r.URL.Query() {
		ok := false
		for _, a := range allowed {
			ok = ok || key == a
		}
		if !ok {
			return http.StatusForbidden, "peer_view_local_only", fmt.Sprintf("the %q option requires unrestricted trust", key)
		}
	}
	return 0, "", ""
}

// peerGroupExportGate: the archive holds every member's conversation and the
// group's settings, so it takes groups.export and sessions.files.read in the
// group, and groups.settings.environment when the group has an environment,
// whose values the peer view otherwise withholds.
func peerGroupExportGate(v *peerView, r *http.Request) (int, string, string) {
	g, err := db.GetAgentGroupByName(r.PathValue("name"))
	if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
		return http.StatusNotFound, "not_found", "object not found"
	}
	if status, code, msg := peerQueryWithin(r, "format"); status != 0 {
		return status, code, msg
	}
	need := []string{PermGroupsExport, PermSessionsFilesRead}
	if len(g.Environment) > 0 {
		need = append(need, PermGroupsSettingsEnvironment)
	}
	for _, slug := range need {
		if !fedPeerAllows(v.peer.InstanceID, g.ID, slug) {
			return http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", slug, g.Name)
		}
	}
	return 0, "", ""
}

// peerAgentBundleExportGate: an agent the peer sees, under
// agent.bundle.export in every group it is in; its private transcript
// (history) also takes sessions.files.read in each.
func peerAgentBundleExportGate(v *peerView, r *http.Request) (int, string, string) {
	res, _, err := agent.ResolveSelector(r.URL.Query().Get("agent"))
	if err != nil || res == nil || !peerAgentActionAllowed(v.peer.InstanceID, res.ConvID, "") {
		return http.StatusNotFound, "not_found", "object not found"
	}
	if status, code, msg := peerQueryWithin(r, "agent", "history", "carry_permissions", "format", "no_native", "no_context"); status != 0 {
		return status, code, msg
	}
	need := []string{PermAgentBundleExport}
	if r.URL.Query().Get("history") == "true" {
		need = append(need, PermSessionsFilesRead)
	}
	inline, profile := peerAgentBundleEnvironment(res.ConvID)
	if inline || profile {
		need = append(need, PermGroupsSettingsEnvironment)
	}
	for _, slug := range need {
		if !peerAgentActionAllowed(v.peer.InstanceID, res.ConvID, slug) {
			return http.StatusForbidden, "permission", fmt.Sprintf("requires %s in every group of the agent", slug)
		}
	}
	if profile && !fedPeerHoldsUnscoped(v.peer.InstanceID, PermNodeSpawnRead) {
		return http.StatusForbidden, "permission", "requires " + PermNodeSpawnRead + ": the archive carries the agent's spawn profile environment"
	}
	return 0, "", ""
}

// peerAgentBundleEnvironment reports whether an agent's archive would carry
// environment values (collectAgentBundle): its spawn request's own, or its
// spawn profile's. The peer view withholds both without their grants. An
// unreadable record counts as carrying them.
func peerAgentBundleEnvironment(conv string) (inline, profile bool) {
	source, err := db.AgentInitialSpawnConfigForConv(conv)
	if err != nil {
		return true, true
	}
	if source == "" {
		return false, false
	}
	var requested struct {
		Profile     string          `json:"profile"`
		Environment json.RawMessage `json:"environment"`
	}
	if json.Unmarshal([]byte(source), &requested) != nil {
		return true, true
	}
	inline = len(requested.Environment) > 0 && !emptyJSONValue(requested.Environment)
	if requested.Profile != "" {
		p, err := db.ResolveSpawnProfile(requested.Profile)
		profile = err != nil || p != nil && len(p.Environment) > 0
	}
	return inline, profile
}
