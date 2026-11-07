package agentd

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A group's task board over federation (tcl-0jgecz, docs/taskboard.md): a
// linked node's operator, or one of its agents, reads and changes a board on
// this node under the peer grants this node's operator gave it:
// taskboard.read, taskboard.write, taskboard.assign (this node's members, on
// the task's group) and taskboard.manage (the writer lock), scoped per group
// (default nothing). Any of them implies read. Taking, releasing and
// reporting stay with this node's own agents. Every route runs this node's own
// handler (the dashboard mux, taskboard.go) under a peerGrantAuthority for the
// board's group, so the board's gates, validation and history are the ones
// local callers get; history and the peer-view audit row name the peer.
// Unrestricted trust acts as this node's operator does.
//
// A task id outside the groups the peer reads answers 404, as it does for a
// local agent, so a peer cannot tell an existing id from a missing one.

func addPeerTaskboardRules(rules map[string]peerViewRule) {
	read := peerViewRule{feature: "groups.taskboard", requires: PermTaskboardRead,
		orGrants: []string{PermTaskboardWrite, PermTaskboardAssign, PermTaskboardManage}, group: true}
	write := peerViewRule{feature: "groups.taskboard.write", requires: PermTaskboardWrite, group: true}

	invoke := &peerInvokeSpec{param: "name", extra: peerTaskboardImpliedGates}
	group := func(pattern string, rule peerViewRule) {
		rule.invoke = invoke
		if strings.HasPrefix(pattern, "GET ") {
			rule.serve = servePeerTaskboardGroup
		} else {
			rule.write = servePeerTaskboardGroup
		}
		rules[pattern] = rule
	}
	group("GET /v1/groups/{name}/taskboard", read)
	group("POST /v1/groups/{name}/taskboard/tasks", write)

	context := read
	context.serve = servePeerTaskboardContext
	rules["GET /v1/taskboard/context"] = context

	for _, pattern := range []string{"GET /v1/taskboard/tasks/{id}", "GET /v1/taskboard/tasks/{id}/events"} {
		r := read
		r.serve = servePeerTaskboardTask
		rules[pattern] = r
	}
	for _, pattern := range []string{"PATCH /v1/taskboard/tasks/{id}", "POST /v1/taskboard/tasks/{id}/move",
		"POST /v1/taskboard/tasks/{id}/stage", "POST /v1/taskboard/tasks/{id}/status"} {
		w := write
		w.write = servePeerTaskboardTask
		rules[pattern] = w
	}

	// Assigning this node's members (an active member of the task's group,
	// as for any assigner) and the writer lock.
	assign := peerViewRule{feature: "groups.taskboard.assign", requires: PermTaskboardAssign, group: true, write: servePeerTaskboardTask}
	rules["POST /v1/taskboard/tasks/{id}/assign"] = assign
	rules["POST /v1/taskboard/tasks/{id}/unassign"] = assign
	group("GET /v1/groups/{name}/taskboard/writers", read)
	group("POST /v1/groups/{name}/taskboard/writers", peerViewRule{feature: "groups.taskboard.manage", requires: PermTaskboardManage, group: true})
}

// taskboardPeerAuthorKey carries, on an unrestricted peer's request (which
// runs as this node's operator), who the linked node's actor is, so a task's
// history and an assignment's brief name it rather than this operator.
type taskboardPeerAuthorKey struct{}

// peerTaskboardAsLocal is an unrestricted peer's request as this node's
// operator, its author attached.
func peerTaskboardAsLocal(r *http.Request, v *peerView) *http.Request {
	local := peerAsLocalRequest(r)
	return local.WithContext(context.WithValue(local.Context(), taskboardPeerAuthorKey{}, peerFollowUpAuthor(v, v.peer.InstanceID)))
}

// taskboardPeerAuthor names the linked node behind a request, or "" for a
// local caller.
func taskboardPeerAuthor(r *http.Request) string {
	if a := peerGrantFromRequest(r); a != nil {
		return a.author
	}
	author, _ := r.Context().Value(taskboardPeerAuthorKey{}).(string)
	return author
}

// servePeerTaskboardGroup serves a route addressed by a group:
// servePeerInvoke under the peer's grants, or as this node's operator under
// unrestricted trust, with the peer named in the board's history.
func servePeerTaskboardGroup(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		v.groupName = r.PathValue("name")
		peerDashboardMux().ServeHTTP(w, peerTaskboardAsLocal(r, v))
		return
	}
	servePeerInvoke(w, r, v, rule)
}

// peerTaskboardSlugs are the board's peer grants; any of them in a group
// lets the peer read that group's board.
var peerTaskboardSlugs = []string{PermTaskboardRead, PermTaskboardWrite, PermTaskboardAssign, PermTaskboardManage}

// peerTaskboardImpliedGates: a peer that writes, assigns on or manages a
// group's board also reads it, so the board's read gate opens to those
// grants.
func peerTaskboardImpliedGates(peer string, g *db.AgentGroup) []string {
	if peerTaskboardReads(peer, g.ID) {
		return []string{PermTaskboardRead}
	}
	return nil
}

// peerTaskboardReads reports whether peer reads group's board: any of its
// taskboard grants there.
func peerTaskboardReads(peer string, groupID int64) bool {
	for _, slug := range peerTaskboardSlugs {
		if fedPeerAllows(peer, groupID, slug) {
			return true
		}
	}
	return false
}

// servePeerTaskboardTask serves a route addressed by a task id: it finds the
// task's group, answers 404 unless the peer reads that board, and runs the
// local handler under the peer's grants there.
func servePeerTaskboardTask(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		v.auditDetail = "task=" + guideText(r.PathValue("id"), 40)
		peerDashboardMux().ServeHTTP(w, peerTaskboardAsLocal(r, v))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	notFound := func() { writeError(w, http.StatusNotFound, "not_found", "no such task "+guideText(id, 40)) }
	if !db.IsGroupTaskID(id) {
		notFound()
		return
	}
	t, err := db.GetGroupTask(id)
	if err != nil {
		notFound()
		return
	}
	g, err := db.GetAgentGroupByID(t.GroupID)
	peer := v.peer.InstanceID
	if err != nil || g == nil || g.IsArchived() || !peerTaskboardReads(peer, g.ID) {
		notFound()
		return
	}
	v.groupName = g.Name
	v.auditDetail = "task=" + id
	if !v.allows(rule, g.ID) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", rule.requires, g.Name))
		return
	}
	a := &peerGrantAuthority{peer: peer, group: g, gates: peerGrantGates(peer, g.ID), author: peerFollowUpAuthor(v, peer)}
	for _, slug := range peerTaskboardImpliedGates(peer, g) {
		a.gates[slug] = true
	}
	ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), peerGrantKey{}, a)
	peerDashboardMux().ServeHTTP(w, r.Clone(ctx))
}

// servePeerTaskboardContext is the dashboard's board context for a peer: the
// groups whose board it reads, and no other.
func servePeerTaskboardContext(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	if !taskboardEnabled(w) {
		return
	}
	all, err := db.ListAgentGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	now := time.Now()
	view := taskboardContextView{Groups: []taskboardGroupContext{}}
	for _, g := range all {
		if g.IsArchived() || !peerTaskboardReads(v.peer.InstanceID, g.ID) {
			continue
		}
		entry, err := buildTaskboardGroupContext(g, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		view.Groups = append(view.Groups, entry)
	}
	writeJSON(w, http.StatusOK, view)
}

// peerSummaryTaskboardCounts sums the counts of the boards the node summary's
// reader reads: every active group's for this node's operator (v nil) and
// unrestricted trust, the groups whose board a peer reads otherwise. Nil while
// the board is off or no board is read.
func peerSummaryTaskboardCounts(v *peerView) *taskboardCounts {
	if cfg, _ := config.Load(); !cfg.TaskboardEnabled() {
		return nil
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil
	}
	var sum *taskboardCounts
	now := time.Now()
	for _, g := range groups {
		if g.IsArchived() || v != nil && !v.unrestricted() && !peerTaskboardReads(v.peer.InstanceID, g.ID) {
			continue
		}
		entry, err := buildTaskboardGroupContext(g, now)
		if err != nil {
			return nil
		}
		if sum == nil {
			sum = &taskboardCounts{}
		}
		c := entry.Counts
		sum.Live += c.Live
		sum.NeedsYou += c.NeedsYou
		sum.InProgress += c.InProgress
		sum.Next += c.Next
		sum.Ideas += c.Ideas
		sum.Blocked += c.Blocked
		sum.Queued += c.Queued
		sum.DoneThisWeek += c.DoneThisWeek
		sum.Dropped += c.Dropped
	}
	return sum
}
