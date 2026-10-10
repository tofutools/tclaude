package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The local CLI reads the same per-group aggregation as the dashboard marker.
// These links reveal other trust relationships and are never shared with peers.
func handleFederationLinks(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read per-group federation links") {
		return
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		writeError(w, 500, "links_unavailable", "local groups unavailable")
		return
	}
	type groupLinks struct {
		GroupID int64                 `json:"group_id"`
		Name    string                `json:"name"`
		Links   []groupFederationLink `json:"federation_links"`
	}
	rows := []groupLinks{}
	filter := r.URL.Query().Get("group")
	links := gatherGroupFederationLinks()
	for _, g := range groups {
		if filter != "" && g.Name != filter {
			continue
		}
		groupRows := links[g.ID]
		if groupRows == nil {
			groupRows = []groupFederationLink{}
		}
		rows = append(rows, groupLinks{GroupID: g.ID, Name: g.Name, Links: groupRows})
	}
	if filter != "" && len(rows) == 0 {
		writeError(w, 404, "not_found", "local group not found")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]any{"groups": rows})
}
