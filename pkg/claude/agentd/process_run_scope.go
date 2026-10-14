package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// process_run_scope.go binds runtime process runs to a context: none (the
// pre-scope behaviour), a group (⌂) or an agent (◉). See "Run scope" in
// docs/processes.md for the model this enforces.
//
// Start gates, by scope:
//
//   - none:  process.runs.manage for the template, exactly as before.
//   - group: process.runs.start covering the group. Ownership contributes it
//     scoped to each owned group, so an owner may start runs there.
//   - agent: self.process when the bound agent is the caller (default-
//     granted), else agent.process or groups.members.process by the usual
//     manager pattern.
//
// The scope gates confer the binding only. A request that authorizes program
// profiles additionally needs process.runs.manage, evaluated without the
// group as a structural subject, so ownership never confers host program
// execution: scope can narrow authority, never widen it.

var errProcessRunOrphaned = errors.New("process run is orphaned: its scope target is gone; re-scope it to continue")

// processRunScopeView is the wire shape of a run's scope binding.
type processRunScopeView struct {
	Kind    string `json:"kind"`
	GroupID int64  `json:"groupId,omitempty"`
	Group   string `json:"group,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	Owner   string `json:"owner,omitempty"`
	State   string `json:"state"`
}

func processRunScopeViewOf(scope db.ProcessRunScope) processRunScopeView {
	return processRunScopeView{
		Kind: scope.Kind, GroupID: scope.GroupID, Group: scope.GroupName,
		AgentID: scope.AgentID, Owner: scope.Owner, State: scope.State,
	}
}

// authorizeProcessRunCreate resolves the requested scope and runs its start
// gate. On failure the response is already written.
func authorizeProcessRunCreate(w http.ResponseWriter, r *http.Request, request processRunCreateRequest, templateID string) (string, db.ProcessRunScope, bool) {
	groupRef := strings.TrimSpace(request.Group)
	agentRef := strings.TrimSpace(request.Agent)
	programs := len(request.AuthorizeProgramProfiles) > 0
	switch {
	case groupRef != "" && agentRef != "":
		writeError(w, http.StatusBadRequest, "process_run_scope", "a run is scoped to a group or an agent, not both")
		return "", db.ProcessRunScope{}, false
	case groupRef != "":
		g, ok := resolveProcessRunScopeGroup(w, groupRef)
		if !ok {
			return "", db.ProcessRunScope{}, false
		}
		caller, ok := requireGroupPermission(w, r, PermProcessRunsStart, g, ActionContext{ProcessTemplate: templateID})
		if !ok {
			return "", db.ProcessRunScope{}, false
		}
		if programs {
			if _, ok := requirePermission(w, r, PermProcessRunsManage, ActionContext{ProcessTemplate: templateID, Group: g.Name}); !ok {
				return "", db.ProcessRunScope{}, false
			}
		}
		return caller, db.ProcessRunScope{Kind: db.ProcessRunScopeGroup, GroupID: g.ID}, true
	case agentRef != "":
		bound, ok := resolveProcessRunScopeAgent(w, agentRef)
		if !ok {
			return "", db.ProcessRunScope{}, false
		}
		var caller string
		if p := peerFromContext(r.Context()); p != nil && p.ConvID != "" && sameActor(p.ConvID, bound.CurrentConvID) {
			caller, ok = requirePermission(w, r, PermSelfProcess)
		} else {
			caller, ok = requireCrossAgentPermission(w, r, PermAgentProcess, bound.CurrentConvID,
				ActionContext{TargetAgent: bound.AgentID})
		}
		if !ok {
			return "", db.ProcessRunScope{}, false
		}
		if programs {
			if _, ok := requirePermission(w, r, PermProcessRunsManage, ActionContext{ProcessTemplate: templateID}); !ok {
				return "", db.ProcessRunScope{}, false
			}
		}
		return caller, db.ProcessRunScope{Kind: db.ProcessRunScopeAgent, AgentID: bound.AgentID}, true
	default:
		caller, ok := requirePermission(w, r, PermProcessRunsManage, ActionContext{ProcessTemplate: templateID})
		return caller, db.ProcessRunScope{Kind: db.ProcessRunScopeNone}, ok
	}
}

// resolveProcessRunScopeGroup resolves a group name or id. resolveGroupToken
// reports an unknown group as an error, so a miss answers 404.
func resolveProcessRunScopeGroup(w http.ResponseWriter, ref string) (*db.AgentGroup, bool) {
	g, err := resolveGroupToken(ref)
	if err != nil || g == nil {
		writeError(w, http.StatusNotFound, "process_run_scope_not_found", fmt.Sprintf("no group %q", ref))
		return nil, false
	}
	if g.IsArchived() {
		writeError(w, http.StatusConflict, "process_run_scope_archived", fmt.Sprintf("group %q is archived", g.Name))
		return nil, false
	}
	return g, true
}

// resolveProcessRunScopeAgent resolves an agent selector to its active
// identity. The cached resolver keeps a miss bounded to SQLite, since the
// lookup runs before the caller is authorized.
func resolveProcessRunScopeAgent(w http.ResponseWriter, ref string) (*db.Agent, bool) {
	resolved, _, err := agent.ResolveSelectorCached(ref)
	if err != nil || resolved == nil || resolved.AgentID == "" {
		writeError(w, http.StatusNotFound, "process_run_scope_not_found", fmt.Sprintf("no agent %q", ref))
		return nil, false
	}
	bound, err := db.GetAgent(resolved.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_run_scope", err.Error())
		return nil, false
	}
	if bound == nil || !bound.Active() || bound.CurrentConvID == "" {
		writeError(w, http.StatusConflict, "process_run_scope_retired", fmt.Sprintf("agent %q is retired", ref))
		return nil, false
	}
	return bound, true
}

// processRunListFilter reads the scope filters of GET /v1/process/runs.
func processRunListFilter(w http.ResponseWriter, r *http.Request) (db.ProcessRunFilter, bool) {
	var filter db.ProcessRunFilter
	query := r.URL.Query()
	if ref := strings.TrimSpace(query.Get("group")); ref != "" {
		// A miss and a lookup failure both answer 404: resolveGroupToken
		// reports an unknown group as an error.
		g, err := resolveGroupToken(ref)
		if err != nil || g == nil {
			writeError(w, http.StatusNotFound, "process_run_scope_not_found", fmt.Sprintf("no group %q", ref))
			return filter, false
		}
		filter.GroupID = g.ID
	}
	for _, field := range []struct {
		name string
		dst  *string
	}{{"agent", &filter.AgentID}, {"performer", &filter.Performer}} {
		ref := strings.TrimSpace(query.Get(field.name))
		if ref == "" {
			continue
		}
		// Retired agents stay addressable here: listing an orphaned agent's
		// runs is exactly how a human finds what to re-scope.
		resolved, _, err := agent.ResolveSelectorCached(ref)
		if err != nil || resolved == nil || resolved.AgentID == "" {
			writeError(w, http.StatusNotFound, "process_run_scope_not_found", fmt.Sprintf("no agent %q", ref))
			return filter, false
		}
		*field.dst = resolved.AgentID
	}
	return filter, true
}

type processRunRescopeRequest struct {
	Group string `json:"group,omitempty"`
	Agent string `json:"agent,omitempty"`
	// None explicitly unbinds an orphaned run. It is a separate flag so an
	// empty body can never re-scope by accident.
	None bool `json:"none,omitempty"`
}

// handleProcessRunRescope is the one explicit scope change: the human
// re-binding an orphaned run, or promoting an agent-scoped run to a group the
// agent belongs to. It is human-only; the runtime never re-scopes on its own.
func handleProcessRunRescope(w http.ResponseWriter, r *http.Request) {
	if classify(peerFromContext(r.Context())) != classHuman {
		writeError(w, http.StatusForbidden, "process_run_rescope_human", "only the human may re-scope a process run")
		return
	}
	caller := ""
	var request processRunRescopeRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	groupRef, agentRef := strings.TrimSpace(request.Group), strings.TrimSpace(request.Agent)
	set := 0
	for _, present := range []bool{groupRef != "", agentRef != "", request.None} {
		if present {
			set++
		}
	}
	if set != 1 {
		writeError(w, http.StatusBadRequest, "process_run_scope", "name exactly one of group, agent, or none")
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	current, err := db.GetProcessRunScope(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	next := db.ProcessRunScope{Kind: db.ProcessRunScopeNone}
	switch {
	case groupRef != "":
		g, ok := resolveProcessRunScopeGroup(w, groupRef)
		if !ok {
			return
		}
		if current.Kind == db.ProcessRunScopeAgent && !current.Orphaned() {
			member, err := processRunScopeAgentInGroup(g.ID, current.AgentID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "process_run_scope", err.Error())
				return
			}
			if !member {
				writeError(w, http.StatusConflict, "process_run_rescope",
					fmt.Sprintf("promotion needs a group the bound agent belongs to; it is not a member of %q", g.Name))
				return
			}
		}
		next = db.ProcessRunScope{Kind: db.ProcessRunScopeGroup, GroupID: g.ID}
	case agentRef != "":
		bound, ok := resolveProcessRunScopeAgent(w, agentRef)
		if !ok {
			return
		}
		next = db.ProcessRunScope{Kind: db.ProcessRunScopeAgent, AgentID: bound.AgentID}
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	if _, err := db.RescopeProcessRun(runID, next, string(actor)); err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, view)
}

// processRunOrphaned reports whether a run's scope target is gone, first
// catching up a bound agent-scoped run whose agent was retired since the
// last sweep. It is consulted before an owner plans more work.
func processRunOrphaned(runID string) (bool, error) {
	scope, err := db.GetProcessRunScope(runID)
	if err != nil {
		return false, err
	}
	if scope.Orphaned() || scope.Kind == db.ProcessRunScopeNone {
		return scope.Orphaned(), nil
	}
	if scope.Kind == db.ProcessRunScopeAgent {
		bound, err := db.GetAgent(scope.AgentID)
		if err != nil {
			return false, err
		}
		if bound != nil && bound.Active() {
			return false, nil
		}
	} else {
		g, err := db.GetAgentGroupByID(scope.GroupID)
		if err != nil {
			return false, err
		}
		if g != nil {
			return false, nil
		}
	}
	if _, err := db.OrphanProcessRunsWithMissingScope(); err != nil {
		return false, err
	}
	return true, nil
}

func processRunScopeAgentInGroup(groupID int64, agentID string) (bool, error) {
	member, err := db.FindAgentMemberInGroup(groupID, agentID)
	return member != nil, err
}
