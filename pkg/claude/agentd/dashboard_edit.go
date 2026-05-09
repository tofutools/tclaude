package agentd

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Mutating endpoints for the dashboard. These live on the loopback
// HTTP server (alongside /api/snapshot) and are gated by the same
// dashboard cookie + Origin/Referer pinning that protects reads.
//
// Why not just call /v1/groups/...? Those endpoints are mounted on
// the daemon's Unix socket and authenticate via SO_PEERCRED — the
// browser can't speak that. So the dashboard server gets parallel
// endpoints that internally call the same db.* helpers.
//
// Same threat model as the rest of the loopback HTTP surface: the
// per-process random session token guards against drive-by browser
// tabs, but a same-user process with /proc access can scrape the
// cookie. Documented and accepted in dashboard.go.
//
// All mutations record `<human-dashboard>` as the granter on
// audit-trail columns (agent_group_owners.granted_by) — the
// dashboard is human-only by definition; agents talk to /v1/.

const dashboardGranter = "<human-dashboard>"

// registerDashboardEditRoutes wires the mutation endpoints onto the
// loopback mux. Called from registerDashboardRoutes.
func registerDashboardEditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/groups/", handleDashboardGroupsAPI)
}

// handleDashboardGroupsAPI dispatches:
//
//	DELETE /api/groups/{name}                   → delete group
//	DELETE /api/groups/{name}/members/{conv}    → remove from group
//	POST   /api/groups/{name}/owners            → grant owner (body: {conv})
//	DELETE /api/groups/{name}/owners/{conv}     → revoke owner
//
// Anything else returns 404.
func handleDashboardGroupsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/groups/{name}[/{members|owners}[/{conv}]]", http.StatusNotFound)
		return
	}
	groupName := parts[0]
	if u, err := url.PathUnescape(groupName); err == nil {
		groupName = u
	}
	g, err := db.GetAgentGroupByName(groupName)
	if err != nil {
		http.Error(w, "group lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if g == nil {
		http.Error(w, "no such group "+groupName, http.StatusNotFound)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		dashboardDeleteGroup(w, groupName)
		return
	}
	switch parts[1] {
	case "members":
		// /api/groups/{name}/members/{conv} — DELETE only.
		if len(parts) < 3 || parts[2] == "" {
			http.Error(w, "expected /api/groups/{name}/members/{conv}", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		dashboardRemoveMember(w, g, parts[2])
	case "owners":
		switch r.Method {
		case http.MethodPost:
			// Body: {"conv": "<selector>"}
			if len(parts) >= 3 && parts[2] != "" {
				http.Error(w, "POST takes the conv in the request body, not the URL", http.StatusBadRequest)
				return
			}
			dashboardAddOwner(w, r, g)
		case http.MethodDelete:
			if len(parts) < 3 || parts[2] == "" {
				http.Error(w, "expected /api/groups/{name}/owners/{conv}", http.StatusNotFound)
				return
			}
			dashboardRemoveOwner(w, g, parts[2])
		default:
			http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, "unknown endpoint /api/groups/{name}/"+parts[1], http.StatusNotFound)
	}
}

func dashboardDeleteGroup(w http.ResponseWriter, name string) {
	if err := db.DeleteAgentGroup(name); err != nil {
		// ON DELETE RESTRICT on agent_messages; surface as 409.
		http.Error(w, "delete group: "+err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func dashboardRemoveMember(w http.ResponseWriter, g *db.AgentGroup, convSelector string) {
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := db.RemoveAgentGroupMember(g.ID, res.ConvID); err != nil {
		http.Error(w, "remove member: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func dashboardAddOwner(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		Conv string `json:"conv"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Conv = strings.TrimSpace(body.Conv)
	if body.Conv == "" {
		http.Error(w, "missing conv", http.StatusBadRequest)
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := db.AddAgentGroupOwner(g.ID, res.ConvID, dashboardGranter); err != nil {
		http.Error(w, "add owner: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

func dashboardRemoveOwner(w http.ResponseWriter, g *db.AgentGroup, convSelector string) {
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	n, err := db.RemoveAgentGroupOwner(g.ID, res.ConvID)
	if err != nil {
		http.Error(w, "remove owner: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "not an owner of this group", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
