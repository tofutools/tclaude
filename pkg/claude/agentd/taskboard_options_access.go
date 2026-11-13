package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Who reads and changes a board's options (docs/taskboard.md "Who may read
// and change them"). The options are the board's setup (its idle check,
// event messages and work-in-progress limits), not its tasks, so they have
// slugs of their own, apart from taskboard.read / taskboard.write:
//
//   - taskboard.options.own.read: read the options of a board of a group the
//     agent is an active member of. Member-implied; never reaches another
//     group, whatever a grant's scope says.
//   - taskboard.options.own.write: change them, on the same terms. Never
//     member-implied: the operator grants it (a default permission, say)
//     without it reaching any other group. Implies own.read.
//   - taskboard.options.read: read any board's options, scoped group=<name>
//     (unscoped: every group); peer=<inst>/<group> for a board on a linked
//     node (agent peer views). Owner-implied for owned groups.
//   - taskboard.options.write: change them, scoped the same way.
//     Owner-implied. Implies taskboard.options.read; taskboard.manage, which
//     manages the board as a whole, implies it (permissionUmbrellaFor).
//
// The operator always passes. A change is narrowed by the writer lock as any
// change of the board is: the group's owners, its listed writers and an
// explicit options grant whose scope the group alone satisfies pass it.
//
// A board's columns (adding, renaming, removing, hiding, cordoning) stay
// taskboard.columns; a column's limit is an option. A task's own queue
// position and a refusal at a full lane are task facts, not settings: they
// read with the board under taskboard.read.

const (
	PermTaskboardOptionsOwnRead  = "taskboard.options.own.read"
	PermTaskboardOptionsOwnWrite = "taskboard.options.own.write"
	PermTaskboardOptionsRead     = "taskboard.options.read"
	PermTaskboardOptionsWrite    = "taskboard.options.write"
)

// agentIsActiveMember reports whether the agent behind conv is an active
// member of g.
func agentIsActiveMember(conv string, g *db.AgentGroup) bool {
	id, err := db.AgentIDForConv(conv)
	return err == nil && id != "" && activeGroupMember(g, id) != ""
}

// agentAllowsOn reports, silently, whether the agent behind conv holds slug
// on g's board.
func agentAllowsOn(r *http.Request, conv, slug string, g *db.AgentGroup) bool {
	allowed, _, err := permissionAllowsAction(r, conv, slug, groupResourceContext(g.Name))
	return err == nil && allowed
}

// agentReadsTaskboardOptions reports, silently, whether the agent behind
// conv reads g's options: taskboard.options.read (or what implies it), or,
// on a group it is an active member of, taskboard.options.own.read (or
// own.write).
func agentReadsTaskboardOptions(r *http.Request, conv string, g *db.AgentGroup) bool {
	if conv == "" || g == nil {
		return false
	}
	if agentAllowsOn(r, conv, PermTaskboardOptionsRead, g) {
		return true
	}
	return agentIsActiveMember(conv, g) && agentAllowsOn(r, conv, PermTaskboardOptionsOwnRead, g)
}

// agentWritesTaskboardOptions reports, silently, whether the agent behind
// conv holds a slug that changes g's options (the writer lock aside).
func agentWritesTaskboardOptions(r *http.Request, conv string, g *db.AgentGroup) bool {
	if conv == "" || g == nil {
		return false
	}
	if agentAllowsOn(r, conv, PermTaskboardOptionsWrite, g) {
		return true
	}
	return agentIsActiveMember(conv, g) && agentAllowsOn(r, conv, PermTaskboardOptionsOwnWrite, g)
}

// requireTaskboardOptionsRead gates a read of g's options. A confirmed agent
// passes on agentReadsTaskboardOptions; anyone else (the operator, a linked
// node under its peer grants) gets the shared gate's answer.
func requireTaskboardOptionsRead(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	clearAuthorizedPermission(r)
	if conv := confirmedAgentConv(r); conv != "" && agentReadsTaskboardOptions(r, conv, g) {
		recordAuthorizedPermission(r, PermTaskboardOptionsRead, 0)
		return true
	}
	_, ok := requireGroupPermission(w, r, PermTaskboardOptionsRead, g)
	return ok
}

// requireTaskboardOptionsChange gates a change of g's options: the slug,
// then the writer lock. A refusal writes the 403.
func requireTaskboardOptionsChange(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) bool {
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	if conv != "" && agentIsActiveMember(conv, g) && !agentAllowsOn(r, conv, PermTaskboardOptionsWrite, g) &&
		agentAllowsOn(r, conv, PermTaskboardOptionsOwnWrite, g) {
		recordAuthorizedPermission(r, PermTaskboardOptionsOwnWrite, 0)
	} else if _, ok := requireGroupPermission(w, r, PermTaskboardOptionsWrite, g); !ok {
		return false
	}
	if conv == "" {
		return true
	}
	passes, err := taskboardOptionsLockPasses(r, g, conv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return false
	}
	if !passes {
		writeError(w, http.StatusForbidden, "taskboard_locked",
			"the "+g.Name+" task board is locked to its listed writers; ask a group owner to add you (tclaude agent groups set-taskboard-writers)")
		return false
	}
	return true
}

// taskboardOptionsLockPasses reports whether the agent behind conv passes
// g's writer lock for a change of its options: as for any change of the
// board (taskboardLockPasses), or through an explicit options grant whose
// scope the group alone satisfies.
func taskboardOptionsLockPasses(r *http.Request, g *db.AgentGroup, conv string) (bool, error) {
	passes, err := taskboardLockPasses(r, g, conv)
	if err != nil || passes {
		return passes, err
	}
	ctx := ActionContext{Group: g.Name}
	for _, slug := range []string{PermTaskboardOptionsWrite, PermTaskboardOptionsOwnWrite} {
		v := resolvePermissionVerdictForRequest(r, conv, slug)
		if v.Resolution == permAllow && evalPermissionScope(v, conv, ctx).Satisfied {
			return true, nil
		}
	}
	return false, nil
}

// callerReadsTaskboardOptions reports, silently, whether the caller of r
// reads g's options: the operator does, a confirmed agent on
// agentReadsTaskboardOptions, a linked node on its peer grants. Anything
// else does not. It decides what a read of the board carries of them (the
// context's summary) and whether a copy of the board's setup (export, clone,
// a template snapshot) carries it.
func callerReadsTaskboardOptions(r *http.Request, g *db.AgentGroup) bool {
	if g == nil {
		return false
	}
	if a := peerGrantFromRequest(r); a != nil {
		return peerReadsTaskboardOptions(a.peer, g.ID)
	}
	if conv := confirmedAgentConv(r); conv != "" {
		return agentReadsTaskboardOptions(r, conv, g)
	}
	if callerIsHuman(r) {
		return true
	}
	return requireTaskboardOptionsRead(&taskboardGateWriter{header: http.Header{}}, r, g)
}

// peerTaskboardOptionsReadGrants are the peer grants under which a linked
// node reads a board's options; peerTaskboardOptionsWriteGrants those under
// which it changes them. taskboard.manage covers the board as a whole.
var (
	peerTaskboardOptionsReadGrants  = []string{PermTaskboardOptionsRead, PermTaskboardOptionsWrite, PermTaskboardManage}
	peerTaskboardOptionsWriteGrants = []string{PermTaskboardOptionsWrite, PermTaskboardManage}
)

// peerReadsTaskboardOptions reports whether peer reads the options of the
// board of group groupID.
func peerReadsTaskboardOptions(peer string, groupID int64) bool {
	for _, slug := range peerTaskboardOptionsReadGrants {
		if fedPeerAllows(peer, groupID, slug) {
			return true
		}
	}
	return false
}

// peerWritesTaskboardOptions reports whether peer changes them.
func peerWritesTaskboardOptions(peer string, groupID int64) bool {
	for _, slug := range peerTaskboardOptionsWriteGrants {
		if fedPeerAllows(peer, groupID, slug) {
			return true
		}
	}
	return false
}

// agentCarriesTaskboardSetup reports whether a copy of g's board setup (a
// group export, a clone, a template snapshot) carries it for the caller of
// r: a confirmed agent only when it reads g's options. Anyone else is the
// operator or a linked node, whose route decides on its own peer grants
// (peerReadsTaskboardOptions) before the copy runs.
func agentCarriesTaskboardSetup(r *http.Request, g *db.AgentGroup) bool {
	conv := confirmedAgentConv(r)
	return conv == "" || agentReadsTaskboardOptions(r, conv, g)
}
