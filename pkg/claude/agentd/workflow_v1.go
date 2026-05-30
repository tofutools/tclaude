package agentd

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// workflow_v1.go is the peer-cred /v1/workflows* socket surface — the CLI-facing
// twin of the cookie-gated dashboard handlers (dashboard_workflows.go). The CLI
// (`tclaude workflow …`, JOH-13) and agents reach these over the Unix socket
// with peer-credential identity (authedCaller); the browser dashboard uses the
// cookie surface. Both share the same cores (createWorkflowInstance,
// workflowDetailJSON, applyWorkflowNodePatch, cancelWorkflowInstance,
// collectWorkflowsSnapshot) so there is one implementation of the behaviour and
// only the auth layer differs.
//
// authz model:
//   - reads (list / detail / events / where) — any authedCaller; `where` is
//     first-person (an agent sees only its own assignments).
//   - create / cancel / delete — human/owner: a human bypasses, an agent must
//     own the instance's bound group.
//   - node-PATCH — assignee-or-human/owner: a human bypasses; an agent may
//     settle a node it is the (succession-resolved) assignee of, or that
//     belongs to a group it owns.

func registerWorkflowV1Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/workflows", handleV1Workflows)
	mux.HandleFunc("/v1/workflows/", handleV1WorkflowsByID)
}

// handleV1Workflows dispatches the collection: GET (list) and POST (create).
// /v1/workflows/where is a sibling literal path handled here too, since it does
// not carry an {id}.
func handleV1Workflows(w http.ResponseWriter, r *http.Request) {
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"instances": collectWorkflowsSnapshot()})
	case http.MethodPost:
		// create — human/owner. An agent with no group it could own the binding
		// of still can't instantiate (matches the dashboard's human-consent gate).
		var body workflowCreateBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "bad JSON: "+err.Error())
			return
		}
		if !isHuman && !callerOwnsCreateGroup(caller, body.Group) {
			writeError(w, http.StatusForbidden, "forbidden",
				"only the human operator or an owner of the bound group may instantiate a workflow")
			return
		}
		res, fail := createWorkflowInstance(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleV1WorkflowsByID dispatches everything under /v1/workflows/{id}, plus the
// literal /v1/workflows/where path (which has no {id}).
func handleV1WorkflowsByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/workflows/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "not_found", "expected /v1/workflows/{id} or /v1/workflows/where")
		return
	}
	// /v1/workflows/where — the reflection endpoint (no {id}).
	if rest == "where" {
		handleV1WorkflowWhere(w, r)
		return
	}

	parts := strings.SplitN(rest, "/", 3)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be an integer")
		return
	}
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	inst, err := db.GetWorkflowInstance(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "not_found", "workflow "+strconv.FormatInt(id, 10)+" not found")
		return
	}

	switch {
	case len(parts) == 1: // /v1/workflows/{id}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, workflowDetailJSON(inst))
		case http.MethodDelete:
			if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
				writeError(w, http.StatusForbidden, "forbidden", "only the human or the bound-group owner may delete a workflow")
				return
			}
			if err := db.DeleteWorkflowInstance(id); err != nil {
				writeError(w, http.StatusInternalServerError, "io", "delete: "+err.Error())
				return
			}
			workflowInstanceLocks.Delete(id)
			w.WriteHeader(http.StatusNoContent)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method", "GET or DELETE on /v1/workflows/{id}")
		}
	case parts[1] == "events" && len(parts) == 2:
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
			return
		}
		handleV1WorkflowEvents(w, r, id)
	case parts[1] == "cancel" && len(parts) == 2:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
			writeError(w, http.StatusForbidden, "forbidden", "only the human or the bound-group owner may cancel a workflow")
			return
		}
		if fail := cancelWorkflowInstance(id); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_status": db.WorkflowStatusCancelled})
	case parts[1] == "nodes" && len(parts) == 3 && parts[2] != "":
		handleV1WorkflowNodePatch(w, r, inst, caller, isHuman, parts[2])
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown path under /v1/workflows/"+strconv.FormatInt(id, 10))
	}
}

// handleV1WorkflowEvents serves the per-instance (or per-node) timeline.
func handleV1WorkflowEvents(w http.ResponseWriter, r *http.Request, instanceID int64) {
	var events []*db.WorkflowEvent
	var err error
	if nodeID := strings.TrimSpace(r.URL.Query().Get("node")); nodeID != "" {
		events, err = db.ListWorkflowEvents(instanceID, nodeID)
	} else {
		events, err = db.ListWorkflowEvents(instanceID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list events: "+err.Error())
		return
	}
	out := make([]workflowEventJSON, 0, len(events))
	for _, e := range events {
		out = append(out, eventToJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// handleV1WorkflowNodePatch is the agent/CLI completion path: it authorises the
// caller against the node (assignee-or-human/owner), then applies the shared
// node-PATCH core. The CLI's `tclaude workflow node <inst> <node> {start|done|
// fail}` wraps this with body {status: running|done|failed, outcome?, output?}.
func handleV1WorkflowNodePatch(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, caller string, isHuman bool, nodeID string) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method", "PATCH only on /v1/workflows/{id}/nodes/{nodeId}")
		return
	}
	node, err := db.GetWorkflowNode(inst.ID, nodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup node: "+err.Error())
		return
	}
	if node == nil {
		writeError(w, http.StatusNotFound, "not_found", "node "+nodeID+" not found")
		return
	}
	if !isHuman && !callerMaySettleNode(caller, inst, node) {
		writeError(w, http.StatusForbidden, "forbidden",
			"only the human, the node's assignee, or the bound-group owner may settle node "+nodeID)
		return
	}
	var body workflowNodePatchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "bad JSON: "+err.Error())
		return
	}
	res, fail := applyWorkflowNodePatch(inst.ID, nodeID, body)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleV1WorkflowWhere is the first-person reflection endpoint: "which
// workflow/node am I (the caller) assigned to?". An agent caller sees only its
// own assignments (matched reincarnation-correctly via ResolveLatestConv); a
// human caller has no conv-id and gets an empty list (humans use list/detail for
// the global view). Optional ?instance=<id> scopes to one instance.
func handleV1WorkflowWhere(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	caller, _, ok := authedCaller(w, r)
	if !ok {
		return
	}

	assignments := []map[string]any{}
	// A human caller (caller == "") is never a node assignee → empty list.
	if caller != "" {
		var nodes []*db.WorkflowNode
		if instStr := strings.TrimSpace(r.URL.Query().Get("instance")); instStr != "" {
			id, perr := strconv.ParseInt(instStr, 10, 64)
			if perr != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", "instance must be an integer")
				return
			}
			inst, ierr := db.GetWorkflowInstance(id)
			if ierr != nil {
				writeError(w, http.StatusInternalServerError, "io", "lookup: "+ierr.Error())
				return
			}
			if inst == nil {
				writeError(w, http.StatusNotFound, "not_found", "workflow "+instStr+" not found")
				return
			}
			nodes, _ = db.ListWorkflowNodes(id)
		} else {
			nodes, _ = db.ListAssignedWorkflowNodes()
		}

		// Cache rebuilt templates + instance lookups per instance across the
		// (already instance-ordered) node list.
		instCache := map[int64]*db.WorkflowInstance{}
		for _, n := range nodes {
			if n.Assignee == "" || db.ResolveLatestConv(n.Assignee) != caller {
				continue
			}
			inst, oki := instCache[n.InstanceID]
			if !oki {
				inst, _ = db.GetWorkflowInstance(n.InstanceID)
				instCache[n.InstanceID] = inst
			}
			if inst == nil {
				continue
			}
			tmpl, _ := rebuildInstanceTemplate(inst)
			assignments = append(assignments, map[string]any{
				"instance": instanceDetailJSON(inst),
				"node":     nodeToJSON(n, tmpl),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"caller":      caller,
		"assignments": assignments,
	})
}

// ----- authz helpers --------------------------------------------------

// callerMaySettleNode reports whether a (non-human) agent caller may settle a
// node: it is the node's assignee (resolved forward through succession so a
// reincarnated assignee still matches), OR it owns the instance's bound group.
func callerMaySettleNode(caller string, inst *db.WorkflowInstance, node *db.WorkflowNode) bool {
	if caller == "" {
		return false
	}
	if node.Assignee != "" && db.ResolveLatestConv(node.Assignee) == caller {
		return true
	}
	return callerOwnsInstanceGroup(caller, inst)
}

// callerOwnsInstanceGroup reports whether caller owns the instance's bound group.
func callerOwnsInstanceGroup(caller string, inst *db.WorkflowInstance) bool {
	if caller == "" || inst.GroupID == 0 {
		return false
	}
	owned, err := db.ListGroupsOwnedBy(caller)
	if err != nil {
		return false
	}
	return slices.Contains(owned, inst.GroupID)
}

// callerOwnsCreateGroup reports whether caller owns the group named in a create
// request. Instantiation binds an optional group; an agent may only instantiate
// into a group it owns (a human bypasses this check at the call site). An
// unbound create (no group) by a bare agent is refused — there is nothing it
// owns to authorise against.
func callerOwnsCreateGroup(caller, groupName string) bool {
	if caller == "" {
		return false
	}
	g := strings.TrimSpace(groupName)
	if g == "" {
		return false
	}
	grp, err := db.GetAgentGroupByName(g)
	if err != nil || grp == nil {
		return false
	}
	owned, err := db.ListGroupsOwnedBy(caller)
	if err != nil {
		return false
	}
	return slices.Contains(owned, grp.ID)
}
