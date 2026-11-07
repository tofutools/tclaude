package agentd

import (
	"context"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskboard.own (tcl-h4zc3i, docs/taskboard.md): an agent tracks its own work
// on the board of a group it is an active member of, without the broad
// taskboard.write. It is member-implied, scoped by group, and never reaches a
// board of a group the agent is not an active member of, whatever a grant's
// scope says. Under it an agent:
//
//   - creates tasks, each assigned to itself as it is created (in progress,
//     blocked or done); an unassigned task (an idea, or in next) needs write;
//   - edits, promotes/demotes, blocks, unblocks, reopens, closes and reports
//     on the tasks assigned to it;
//   - takes any open, unassigned task (an idea is promoted on the way) and
//     releases its own; an assigned task is taskboard.assign's;
//   - comments on any task of the board (requireTaskboardComment).
//
// It is narrowed by the writer lock, as write is, except a report; an explicit
// taskboard.own grant scoped to the group passes the lock, as an explicit
// write or assign grant does. self.taskboard, the slug it replaces, stays a
// deprecated alias: an explicit deny of it still denies own.

const PermTaskboardOwn = "taskboard.own"

// taskboardSlugDenied reports whether conv holds an explicit deny of slug.
func taskboardSlugDenied(r *http.Request, conv, slug string) bool {
	return resolvePermissionVerdictForRequest(r, conv, slug).Resolution == permDeny
}

// agentOwnsTaskboard reports, silently, whether the agent behind conv holds
// taskboard.own on g: it is an active member of g, neither taskboard.own nor
// its alias self.taskboard is denied, and the slug resolves.
func agentOwnsTaskboard(r *http.Request, conv string, g *db.AgentGroup) bool {
	if conv == "" || g == nil {
		return false
	}
	if id, err := db.AgentIDForConv(conv); err != nil || id == "" || activeGroupMember(g, id) == "" {
		return false
	}
	if taskboardSlugDenied(r, conv, PermSelfTaskboard) {
		return false
	}
	allowed, _, err := permissionAllowsAction(r, conv, PermTaskboardOwn, groupResourceContext(g.Name))
	return err == nil && allowed
}

// agentWritesTaskboard reports, silently and without recording an audit
// source, whether the agent behind conv holds taskboard.write on g.
func agentWritesTaskboard(r *http.Request, conv string, g *db.AgentGroup) bool {
	if conv == "" || g == nil {
		return false
	}
	allowed, _, err := permissionAllowsAction(r, conv, PermTaskboardWrite, groupResourceContext(g.Name))
	return err == nil && allowed
}

// taskboardOwnOnly reports whether the calling agent acts on g under
// taskboard.own alone: it owns its board but does not write it.
func taskboardOwnOnly(r *http.Request, g *db.AgentGroup) bool {
	conv := confirmedAgentConv(r)
	return conv != "" && !agentWritesTaskboard(r, conv, g) && agentOwnsTaskboard(r, conv, g)
}

// requireTaskboardOwn gates a self-tracking action on g's board by the
// calling agent: taskboard.own, on a group it is an active member of, and,
// when lockApplies, a pass of the writer lock. A refusal writes the 403.
func requireTaskboardOwn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, lockApplies bool) bool {
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	if conv != "" {
		if id, err := db.AgentIDForConv(conv); err != nil || id == "" || activeGroupMember(g, id) == "" {
			writeError(w, http.StatusForbidden, "permission_denied",
				PermTaskboardOwn+" covers only the board of a group you are an active member of; "+g.Name+" is not one")
			return false
		}
		if taskboardSlugDenied(r, conv, PermSelfTaskboard) {
			writeError(w, http.StatusForbidden, "permission_denied",
				PermSelfTaskboard+" (the deprecated alias of "+PermTaskboardOwn+") is denied to you")
			return false
		}
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardOwn, g); !ok {
		return false
	}
	return !lockApplies || requireTaskboardUnlocked(w, r, g)
}

// requireTaskboardTaskChange gates a change of task t on g's board: its
// assignee changes it under taskboard.own, anyone else needs
// taskboard.write. Both are narrowed by the writer lock.
func requireTaskboardTaskChange(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, t *db.GroupTask) bool {
	if self, conv := callerAgentID(r); self != "" && t.AssigneeID == self &&
		!agentWritesTaskboard(r, conv, g) && agentOwnsTaskboard(r, conv, g) {
		return requireTaskboardOwn(w, r, g, true)
	}
	return requireTaskboard(w, r, g, true)
}

// requireTaskboardComment gates a comment on a task of g's board: the
// operator, a taskboard.write holder, or a taskboard.own holder on its own
// group's board (any task in it). Both are narrowed by the writer lock. A
// refusal writes the 403.
func requireTaskboardComment(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	if taskboardOwnOnly(r, g) {
		return requireTaskboardOwn(w, r, g, true)
	}
	return requireTaskboard(w, r, g, true)
}

// taskboardMemberWrites reports whether the member (or owner) behind conv may
// change g's board as a writer: it holds taskboard.write and passes the
// writer lock. It is evaluated for that member alone, so none of the
// caller's own grants (a sudo, say) count for it.
func taskboardMemberWrites(r *http.Request, g *db.AgentGroup, conv string) bool {
	alone := r.WithContext(context.Background())
	if !agentWritesTaskboard(alone, conv, g) {
		return false
	}
	ok, err := taskboardLockPasses(alone, g, conv)
	return err == nil && ok
}
