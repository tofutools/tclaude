package agentd

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

var fedNodeGroupsMu sync.Mutex
var nodeGroupNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// One lifecycle/away lock covers the affected peer set. Nesting individual
// peer locks would deadlock because they share the same lifecycle mutex.
func lockNodeGroupAuthorityMutation(id, extraPeer string) (func(), error) {
	fedNodeGroupsMu.Lock()
	members, err := db.ListFederationNodeGroupMembers(id)
	if err != nil {
		fedNodeGroupsMu.Unlock()
		return nil, err
	}
	affected := map[string]bool{extraPeer: true}
	for _, p := range members {
		affected[p.InstanceID] = true
	}
	fedLifecycleMu.Lock()
	rt := currentFederation()
	if rt != nil {
		rt.awayMu.Lock()
	}
	return func() {
		if rt != nil {
			if rt.away != nil && affected[rt.away.Cover] {
				rt.away.Epoch = newApprovalID()
			}
			rt.awayMu.Unlock()
		}
		fedLifecycleMu.Unlock()
		fedNodeGroupsMu.Unlock()
	}, nil
}
func registerFederationNodeGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/nodes/groups", handleFederationNodeGroups)
	mux.HandleFunc("POST /v1/federation/nodes/groups", handleFederationNodeGroups)
	mux.HandleFunc("DELETE /v1/federation/nodes/groups/{name}", handleFederationNodeGroups)
	mux.HandleFunc("POST /v1/federation/nodes/groups/{name}/members", handleFederationNodeGroupMembers)
	mux.HandleFunc("DELETE /v1/federation/nodes/groups/{name}/members", handleFederationNodeGroupMembers)
}
func handleFederationNodeGroups(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage local node groups") {
		return
	}
	if r.Method == http.MethodGet {
		groups, err := db.ListFederationNodeGroups()
		if err != nil {
			writeError(w, 500, "node_groups", err.Error())
			return
		}
		out := []map[string]any{}
		for _, g := range groups {
			members, e := db.ListFederationNodeGroupMembers(g.ID)
			if e != nil {
				writeError(w, 500, "node_groups", e.Error())
				return
			}
			out = append(out, map[string]any{"id": g.ID, "name": g.Name, "created_at": g.CreatedAt, "members": nodeGroupMemberViews(members)})
		}
		writeJSON(w, 200, map[string]any{"groups": out})
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || !nodeGroupNamePattern.MatchString(in.Name) {
			writeError(w, 400, "node_group", "name must be 1-64 lowercase letters, digits, dots, underscores or hyphens, starting with a letter or digit")
			return
		}
		fedNodeGroupsMu.Lock()
		g, err := db.CreateFederationNodeGroup(in.Name)
		fedNodeGroupsMu.Unlock()
		if err != nil {
			writeError(w, 409, "node_group", err.Error())
			return
		}
		setAuditTargetLabel(r, "group:"+g.Name)
		writeJSON(w, 200, g)
		return
	}
	g, err := db.GetFederationNodeGroup(r.PathValue("name"))
	if err != nil {
		writeError(w, 404, "node_group", err.Error())
		return
	}
	finish, err := lockNodeGroupAuthorityMutation(g.ID, "")
	if err != nil {
		writeError(w, 500, "node_group", err.Error())
		return
	}
	err = db.DeleteFederationNodeGroup(g.ID)
	finish()
	if err != nil {
		writeError(w, 500, "node_group", err.Error())
		return
	}
	broadcastFederationCatalogs()
	setAuditTargetLabel(r, "group:"+g.Name)
	writeJSON(w, 200, map[string]any{"ok": true})
}
func handleFederationNodeGroupMembers(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage local node group membership") {
		return
	}
	g, err := db.GetFederationNodeGroup(r.PathValue("name"))
	if err != nil {
		writeError(w, 404, "node_group", err.Error())
		return
	}
	var in struct {
		Peer string `json:"peer"`
	}
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, 400, "node_group", err.Error())
		return
	}
	p, err := resolveFederationPeer(in.Peer)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	finish, err := lockNodeGroupAuthorityMutation(g.ID, p.InstanceID)
	if err != nil {
		writeError(w, 500, "node_group", err.Error())
		return
	}
	if r.Method == http.MethodPost {
		err = db.AddFederationNodeGroupPeer(g.ID, p.InstanceID)
	} else {
		err = db.RemoveFederationNodeGroupPeer(g.ID, p.InstanceID)
	}
	finish()
	if err != nil {
		writeError(w, 409, "node_group", err.Error())
		return
	}
	broadcastFederationCatalogs()
	setAuditTargetLabel(r, "group:"+g.Name+"/"+p.InstanceID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func nodeGroupMemberViews(peers []db.FederationPeer) []map[string]any {
	out := []map[string]any{}
	for _, p := range peers {
		out = append(out, map[string]any{"instance_id": p.InstanceID, "label": p.Label, "name": p.Name})
	}
	return out
}
