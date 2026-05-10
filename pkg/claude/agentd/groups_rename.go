package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleGroupRename renames a group's canonical name. The schema stores
// group references as integer foreign keys (group_id), so renaming is a
// single-row UPDATE — no cascading required across agent_messages,
// agent_group_members, agent_group_owners, or agent_cron_jobs.
//
// Permission slug: groups.rename (default human-only).
//
// Errors:
//   - 400 invalid name (empty, embedded slash, control char) — the URL
//     dispatcher splits on slashes, so we reject any name that would
//     break the routing for /v1/groups/<name>/...
//   - 404 if the source group doesn't exist
//   - 409 if the target name already belongs to another group
//
// Same-name rename is a no-op (200 OK + audit row) so the human can
// safely retry after a typo elsewhere without an error.
func handleGroupRename(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	caller, ok := requirePermission(w, r, PermGroupsRename)
	if !ok {
		return
	}
	var body struct {
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if err := validateGroupName(body.NewName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	renamed, err := db.RenameAgentGroup(g.Name, body.NewName, caller)
	if errors.Is(err, db.ErrGroupNameTaken) {
		writeError(w, http.StatusConflict, "exists",
			"a group named \""+body.NewName+"\" already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if renamed == nil {
		// db.RenameAgentGroup signals "no such group" via (nil, nil),
		// but the dispatcher already resolved g from the path so this
		// branch is unreachable in practice — keep it as defence
		// against a race where the group is deleted between dispatch
		// and rename.
		writeError(w, http.StatusNotFound, "not_found",
			"group \""+g.Name+"\" no longer exists")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":    renamed.Name,
		"old_name": g.Name,
		"action":   "renamed",
	})
}

// validateGroupName rejects names that would break the URL dispatcher.
// Liberal otherwise: emoji, unicode, periods are all fine. Pinned
// rules:
//   - non-empty
//   - no embedded slash (would split the URL path)
//   - no control characters (would garble logs / breaks completion)
//   - no leading/trailing whitespace (caller should have already
//     trimmed; rejecting here surfaces sloppy callers)
func validateGroupName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if strings.ContainsAny(name, "/\\") {
		return errors.New("name must not contain slashes (URL routing)")
	}
	if strings.TrimSpace(name) != name {
		return errors.New("name must not have leading or trailing whitespace")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("name must not contain control characters")
		}
	}
	return nil
}
