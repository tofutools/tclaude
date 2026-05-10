package agentd

import (
	"errors"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleGroupArchive marks a group as archived (soft-deleted).
// Mutating operations on the group (member.add, owners.*, message)
// will refuse afterwards; listing surfaces hide it from defaults.
// Members + ownership rows + message history all stay intact for
// forensic queries; reverse with /v1/groups/{name}/unarchive.
//
// Idempotent: re-archiving an already-archived group bumps the
// timestamp without erroring. Distinct from `groups rm` which
// destroys the group + its history outright.
//
// Note: archive does NOT auto-stop the group's running members.
// If the human wants to seal the membership AND end the panes,
// they should call `groups stop` first. We considered chaining
// stop into archive but the blast radius is too high to bake in
// silently; explicit two-step keeps the destructive part visible.
func handleGroupArchive(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsArchive); !ok {
		return
	}
	if err := db.ArchiveAgentGroup(g.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":  g.Name,
		"action": "archived",
		"note":   "membership + ownership + history preserved; mutating ops will refuse until unarchived",
	})
}

// handleGroupUnarchive reverses handleGroupArchive — clears
// archived_at so the group is active again. Idempotent on
// already-active groups.
func handleGroupUnarchive(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsArchive); !ok {
		return
	}
	if err := db.UnarchiveAgentGroup(g.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":  g.Name,
		"action": "unarchived",
	})
}

// errGroupArchived is the sentinel returned by mutation guards when a
// caller targets an archived group. Translated to a 409 Conflict at
// the HTTP layer so clients can distinguish "the group is sealed"
// from "you don't have permission" (403) and "the group is gone"
// (404).
var errGroupArchived = errors.New("group is archived; unarchive it first or pick a different group")

// requireGroupActive refuses if g is archived. Returns true on
// active groups; on archived groups it writes a 409 + descriptive
// error and returns false. Use at the top of every mutation handler
// (member.add, owners.*, message, etc.) — a sealed group should
// reject all writes.
func requireGroupActive(w http.ResponseWriter, g *db.AgentGroup) bool {
	if g == nil || !g.IsArchived() {
		return true
	}
	writeError(w, http.StatusConflict, "archived", errGroupArchived.Error())
	return false
}

// isTruthy maps the common HTTP query-string conventions for "yes"
// to bool. Used by listing endpoints that grow ?archived=1 /
// ?include_x=true style toggles.
func isTruthy(v string) bool {
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
