package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleGroupParent serves PUT /v1/groups/{name}/parent — nest the group
// under another group (n-level groups-in-groups, JOH-392), or clear its
// parent so it returns to the top level.
//
// Body: {"parent": "<group-name>"}. An empty/absent parent ("" or {} or no
// body) clears the nesting. This is board-organisation only: it shapes the
// dashboard's group tree and does NOT change messaging, permissions, cron
// multicast or spawn-target — those never traverse the parent edge in v1.
//
// Permission: groups.nest, with an owner-of-THIS-group bypass (you may
// re-parent a group you own without the explicit slug). Owning the parent is
// not required — filing your group under someone else's is allowed, the same
// way an inter-group link's authority sits on the FROM group.
//
// db.SetAgentGroupParent rejects cycles (self-parent or nesting under a
// descendant) with a 409; a missing parent name is a 404.
func handleGroupParent(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requireGroupPermission(w, r, PermGroupsNest, g); !ok {
		return
	}

	var body struct {
		Parent string `json:"parent"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				`body must be JSON {"parent":"<group-name>"} ("" clears the parent)`)
			return
		}
	}
	parent := strings.TrimSpace(body.Parent)
	if parent == g.Name {
		// Friendlier than round-tripping to the cycle sentinel.
		writeError(w, http.StatusConflict, "cycle", db.ErrGroupParentCycle.Error())
		return
	}

	updated, err := db.SetAgentGroupParent(g.ID, parent)
	switch {
	case errors.Is(err, db.ErrGroupParentNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	case errors.Is(err, db.ErrGroupParentCycle):
		writeError(w, http.StatusConflict, "cycle", err.Error())
		return
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "group vanished")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}

	// Resolve the (now-authoritative) parent id back to a name for the reply.
	parentName := ""
	if updated.ParentGroupID != nil {
		if p, perr := db.GetAgentGroupByID(*updated.ParentGroupID); perr == nil && p != nil {
			parentName = p.Name
		}
	}
	action := "nested"
	if parentName == "" {
		action = "unnested"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":  updated.Name,
		"parent": parentName,
		"action": action,
	})
}
