package agentd

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/workflow"
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
			// Serialise the delete against any in-flight drive on this instance
			// (the engine's spawn/settle, a node-PATCH's advance) under the SAME
			// per-instance lock the mutating handlers take — exactly as the
			// dashboard DELETE twin does. Without it a delete could wipe the rows
			// mid read-modify-write, and dropping the map entry while another
			// goroutine still holds the mutex would let a fresh caller mint a
			// SECOND mutex for the same id, breaking the mutual exclusion.
			unlock := lockWorkflowInstance(id)
			delErr := db.DeleteWorkflowInstance(id)
			if delErr == nil {
				workflowInstanceLocks.Delete(id) // row is gone; drop the now-unreachable mutex
			}
			unlock()
			if delErr != nil {
				writeError(w, http.StatusInternalServerError, "io", "delete: "+delErr.Error())
				return
			}
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
		// parts[2] is "{nodeId}" (PATCH) or "{nodeId}/start" (the driver's
		// spawn-into-node verb). Node ids are mermaid ids (no slashes), so the
		// first "/" cleanly separates the node from its sub-action.
		nodeID, sub, _ := strings.Cut(parts[2], "/")
		if nodeID == "" {
			writeError(w, http.StatusNotFound, "not_found", "expected /v1/workflows/{id}/nodes/{nodeId}")
			return
		}
		switch sub {
		case "":
			handleV1WorkflowNodePatch(w, r, inst, caller, isHuman, nodeID)
		case "start":
			handleV1WorkflowNodeStart(w, r, inst, caller, isHuman, nodeID)
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown node subpath "+strconv.Quote(sub))
		}
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

// handleV1WorkflowNodeStart is the agent-engine driver's spawn-worker-into-node
// verb (JOH-15 B1): POST /v1/workflows/{id}/nodes/{nodeId}/start spawns a fresh
// agent into the instance's bound group for a ready ai node, reusing the same
// spawnWorkerIntoNodeCore the dashboard start path uses (so guards, state, and
// the spawned-into-group result are byte-identical across the two surfaces).
//
// Authorised for the instance's group-OWNER (a human bypasses): the driver of an
// engine:agent instance is a group-owner, which ALREADY carries graph-level drive
// authority (callerOwnsInstanceGroup — the same authority that settles any node
// via the PATCH gate, cancels, and deletes), so this adds NO new authz surface —
// only a new verb behind the existing owner gate (F2). A bare node assignee is
// deliberately NOT enough: spawning a worker into a node is a graph-drive action,
// not settling one's own assigned node.
func handleV1WorkflowNodeStart(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, caller string, isHuman bool, nodeID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only on /v1/workflows/{id}/nodes/{nodeId}/start")
		return
	}
	if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
		writeError(w, http.StatusForbidden, "forbidden",
			"only the human or the bound-group owner may spawn a worker into node "+nodeID)
		return
	}
	res, fail := spawnWorkerIntoNodeCore(inst.ID, nodeID)
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
				"instance":  instanceDetailJSON(inst),
				"node":      nodeToJSON(n, tmpl),
				"self_view": workflowNodeSelfView(inst, n, tmpl),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"caller":      caller,
		"assignments": assignments,
	})
}

// workflowSelfView is the JOH-15 Slice-A self-view: everything an agent embedded
// in a node needs to do its node WITHOUT parsing the mermaid chart or
// re-resolving interpolation by hand. It is computed server-side from the
// instance's live scope (params + captured vars) and the snapshotted template,
// and attached to each `tclaude workflow where` assignment.
type workflowSelfView struct {
	// Task is the node's raw instruction by executor kind: an ai node's prompt,
	// a human node's instructions, a tool/program node's run command.
	Task string `json:"task"`
	// TaskInterpolated is Task with {{param}} / {{captured}} / {{node.output}}
	// refs resolved against the instance's current scope — the agent's actual,
	// inputs-filled-in task. MissingRefs names any ref that did not resolve yet
	// (e.g. an upstream output not produced), left verbatim in TaskInterpolated.
	TaskInterpolated string   `json:"task_interpolated"`
	MissingRefs      []string `json:"missing_refs"`
	// AllowedOutcomes are the outcome labels this node may settle with (the enum
	// values for an enum-verified node, else pass/fail), surfaced here so the
	// agent need not guess what to pass to `node … done --outcome`.
	AllowedOutcomes []string `json:"allowed_outcomes"`
	// Successors maps each outcome to where the graph goes next, resolved
	// server-side from the chart so the agent never parses mermaid.
	Successors []workflowSuccessor `json:"successors"`
}

// workflowSuccessor is one outcome→target edge in the self-view.
type workflowSuccessor struct {
	Outcome string `json:"outcome"`  // the outcome that takes this edge ("pass" for an unlabeled/default edge)
	To      string `json:"to"`       // successor node id
	ToLabel string `json:"to_label"` // successor's display label (falls back to its id)
}

// workflowNodeSelfView builds the self-view for one assigned node. A nil template
// (a corrupt snapshot) degrades to just the raw task, so `where` never fails.
func workflowNodeSelfView(inst *db.WorkflowInstance, n *db.WorkflowNode, tmpl *workflow.Template) workflowSelfView {
	sv := workflowSelfView{MissingRefs: []string{}, AllowedOutcomes: []string{}, Successors: []workflowSuccessor{}}
	if tmpl == nil {
		return sv
	}
	def := tmpl.Nodes[n.NodeID]
	sv.Task = nodeTaskText(def)
	// Interpolate against the live scope (params, shadowed by captured vars) — the
	// same scope the engine uses to run the node. Interpolate already returns the
	// unresolved refs sorted + deduped.
	interp, missing := instanceScope(inst).Interpolate(sv.Task)
	sv.TaskInterpolated = interp
	if len(missing) > 0 {
		sv.MissingRefs = missing
	}
	if oc := tmpl.AllowedOutcomes(n.NodeID); len(oc) > 0 {
		sv.AllowedOutcomes = oc
	}
	// Successors are the STATIC chart edges out of this node (outcome → target),
	// NOT the Advance-computed runtime frontier: they answer "if I settle with
	// outcome X, the flow heads toward Y", not join-gating / skip / reachability.
	for _, e := range tmpl.OutEdges(n.NodeID) {
		outcome := e.Label
		if outcome == "" {
			outcome = workflow.OutcomePass // an unlabeled edge is followed on pass
		}
		sv.Successors = append(sv.Successors, workflowSuccessor{
			Outcome: outcome, To: e.To, ToLabel: tmpl.DisplayLabel(e.To),
		})
	}
	return sv
}

// nodeTaskText returns a node's raw instruction by executor kind: the ai prompt,
// the human instructions, or the tool/program run command.
func nodeTaskText(def *workflow.Node) string {
	if def == nil {
		return ""
	}
	switch def.Executor.Kind {
	case workflow.ExecAI:
		return def.Executor.Prompt
	case workflow.ExecHuman:
		return def.Executor.Instructions
	case workflow.ExecTool, workflow.ExecProgram:
		return def.Executor.Run
	}
	return ""
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
