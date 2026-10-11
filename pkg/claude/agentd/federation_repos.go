package agentd

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
)

// Names are display metadata; the immutable definition keeps group IDs as authority.
type federationRepoResponse struct {
	db.FederationRepo
	GroupNames []string `json:"group_names"`
}

func registerFederationRepoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/repos", handleFederationRepos)
	mux.HandleFunc("POST /v1/federation/repos", handleFederationRepos)
	mux.HandleFunc("PUT /v1/federation/repos/{name}", handleFederationRepos)
	mux.HandleFunc("DELETE /v1/federation/repos/{name}", handleFederationRepos)
}
func handleFederationRepos(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage allowed repositories") {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		rows, e := db.ListFederationRepos()
		if e != nil {
			writeFedErr(w, e)
			return
		}
		groups, e := db.ListAgentGroups()
		if e != nil {
			writeFedErr(w, e)
			return
		}
		names := make(map[int64]string, len(groups))
		for _, group := range groups {
			names[group.ID] = group.Name
		}
		result := make([]federationRepoResponse, 0, len(rows))
		for _, row := range rows {
			groupNames := []string{}
			for _, id := range row.Definition.Groups {
				if name, exists := names[id]; exists {
					groupNames = append(groupNames, name)
				}
			}
			result = append(result, federationRepoResponse{FederationRepo: row, GroupNames: groupNames})
		}
		writeJSON(w, 200, map[string]any{"repos": result})
		return
	}
	// Share the policy mutex with trust/grant/profile changes; a launch pins
	// this immutable repo ID and revision and rechecks them under this gate.
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	if r.Method == http.MethodDelete {
		if e := db.DisableFederationRepo(r.PathValue("name")); e != nil {
			writeFedErr(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	var body struct {
		Name     string   `json:"name"`
		URL      string   `json:"url"`
		Clone    string   `json:"clone"`
		Groups   []string `json:"groups"`
		Revision int64    `json:"revision"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	if !jobrepo.ValidName(body.Name) {
		writeError(w, 400, "repository", "invalid repository name")
		return
	}
	groups := []int64{}
	groupNames := []string{}
	seen := map[int64]bool{}
	for _, name := range body.Groups {
		g, e := db.GetAgentGroupByName(name)
		if e != nil || g == nil || g.IsArchived() {
			writeError(w, 400, "group", "allowed receiving group must exist and be active")
			return
		}
		if !seen[g.ID] {
			groups = append(groups, g.ID)
			groupNames = append(groupNames, g.Name)
			seen[g.ID] = true
		}
	}
	def, e := jobrepo.Inspect(r.Context(), body.URL, body.Clone, groups)
	if e != nil {
		writeError(w, 400, "repository", e.Error())
		return
	}
	row := db.FederationRepo{Name: body.Name, Definition: def, Enabled: true}
	if r.Method == http.MethodPut {
		old, e := db.GetFederationRepo(r.PathValue("name"))
		if e != nil {
			writeError(w, 404, "repository", "repository not found")
			return
		}
		row.ID = old.ID
		row.Revision = body.Revision
	}
	if e = db.SaveFederationRepo(&row); e != nil {
		writeError(w, 409, "repository", e.Error())
		return
	}
	writeJSON(w, 200, federationRepoResponse{FederationRepo: row, GroupNames: groupNames})
}
