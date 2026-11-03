package agentd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// The operator's lock on a group resource (tcl-dwzsq0).
//
// A process template, a collab board or a live page can be locked by the
// operator against every agent change. While locked, an agent's change is
// refused with resource_locked (403) whatever the agent's grants: no slug,
// sudo grant, group ownership or template carries past it. Reads stay open.
//
// Only the operator sets or clears a lock, through the dashboard API below
// (POST /api/resource-locks), which no agent reaches: it sits behind
// checkDashboardAuth, and the agent socket has no route to it. Process and
// board locks are stored (group_resource_locks); a page's lock lives with the
// page (livePage.agentsLocked), and ends when it is closed.
//
// What a lock refuses an agent:
//   - process: saving or deleting the template, changing its library listing,
//     unpinning it from a group, and importing over it (a .pwf or a config
//     bundle). Pinning it to another group is allowed: that changes the group,
//     not the template.
//   - board: every patch (the agent PATCH route); the operator's dashboard
//     patch stays open.
//   - page: patches, eval, close and re-show, by its shower or a member.

// Resource kinds as the API names them.
const (
	resourceKindProcess = db.GroupResourceProcess
	resourceKindBoard   = db.GroupResourceBoard
	resourceKindPage    = "page"
)

func writeResourceLocked(w http.ResponseWriter, what string) {
	writeError(w, http.StatusForbidden, "resource_locked",
		what+" is locked by the operator: no agent may change it until the operator unlocks it in the dashboard")
}

// resourceLockedError is a planned change refused by a lock, for a caller
// that answers with writeResourceLocked later.
type resourceLockedError struct{ what string }

func (e *resourceLockedError) Error() string { return e.what + " is locked by the operator" }

// callerIsHuman reports whether the request comes from the operator (the
// dashboard, or a CLI call with no agent behind it).
func callerIsHuman(r *http.Request) bool {
	return classify(peerFromContext(r.Context())) == classHuman
}

// refuseLockedForAgent refuses an agent's change to a locked stored resource,
// and reports whether it did. The operator is never refused.
func refuseLockedForAgent(w http.ResponseWriter, r *http.Request, kind, id, what string) bool {
	if callerIsHuman(r) || !db.GroupResourceLocked(kind, id) {
		return false
	}
	writeResourceLocked(w, what)
	return true
}

// pageLockAllows is the page gate's lock check (pageForAs): on a locked page
// only reads pass. Every page caller is an agent.
func pageLockAllows(w http.ResponseWriter, p *livePage, perm string) bool {
	if perm == PermGroupsPagesRead {
		return true
	}
	livePages.mu.Lock()
	locked := p.agentsLocked
	livePages.mu.Unlock()
	if locked {
		writeResourceLocked(w, "page "+p.owner+"/"+p.name)
		return false
	}
	return true
}

// POST /api/resource-locks {kind, id, locked} — the operator locks or
// unlocks a resource. kind is process (a template id), board (a board id) or
// page (a page's public id).
func handleDashboardResourceLock(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Locked *bool  `json:"locked"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Locked == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", `body must be {"kind": "process"|"board"|"page", "id": "...", "locked": true|false}`)
		return
	}
	id := strings.TrimSpace(req.ID)
	switch req.Kind {
	case resourceKindProcess:
		if err := store.ValidateTemplateID(id); err != nil {
			writeError(w, http.StatusBadRequest, "process_template_id", err.Error())
			return
		}
		if *req.Locked && !processTemplateExists(w, r, id) {
			return
		}
	case resourceKindBoard:
		if _, err := db.GetCollabBoard(id); *req.Locked && err != nil {
			writeBoardStoreError(w, err)
			return
		}
	case resourceKindPage:
		livePages.mu.Lock()
		p := livePages.byID[id]
		if p != nil {
			p.agentsLocked = *req.Locked
		}
		livePages.mu.Unlock()
		if p == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such page")
			return
		}
		setAuditTargetLabel(r, "page "+p.owner+"/"+p.name)
		writeJSON(w, http.StatusOK, map[string]any{"kind": req.Kind, "id": id, "locked": *req.Locked})
		return
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "kind must be process, board or page")
		return
	}
	if err := db.SetGroupResourceLocked(req.Kind, id, *req.Locked); err != nil {
		writeError(w, http.StatusInternalServerError, "resource_lock", err.Error())
		return
	}
	if req.Kind == resourceKindBoard {
		boardLive.publish(id, "")
	}
	setAuditTargetLabel(r, req.Kind+" "+id)
	writeJSON(w, http.StatusOK, map[string]any{"kind": req.Kind, "id": id, "locked": *req.Locked})
}
