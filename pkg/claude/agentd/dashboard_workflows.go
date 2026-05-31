package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/workflow"
)

// Dashboard workflow routes — the human-facing CRUD + manual-driving surface
// for workflow instances. Sibling of dashboard_cron.go: every handler is
// cookie-authenticated for the browser, path-dispatched, and responds via
// writeJSON. Wired into the popup mux from registerDashboardEditRoutes.
//
// Scope (Step 3): instantiate from a snapshotted template, inspect, manually
// advance nodes (which follows the template's branches via workflow.Advance),
// cancel and delete.
//
// Scope (Step 4 — group integration): bind an instance to a tclaude agent
// group at create (group_id), spawn/attach the per-node ai agent into that
// group (start/attach, replacing Step 3's 501 stubs), and the human-approval
// verify gate (approve/reject). All mutating paths serialise per instance via
// lockWorkflowInstance so two concurrent drives never read-modify-write stale
// node state. The execution engine that AUTO-drives nodes is Step 6; the manual
// PATCH / start / approve paths and that engine all share workflow.Advance.

// workflowProjectDirsFn yields the project-source directories searched when
// resolving / listing workflow templates. The daemon serves the whole machine,
// so there is no single project context — it derives one from its own working
// directory, which is the repo a `tclaude agentd` started from in dev. Tests
// override it (see SetWorkflowProjectDirsForTest) to point at a fixture dir.
var workflowProjectDirsFn = defaultWorkflowProjectDirs

func defaultWorkflowProjectDirs() []string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return nil
	}
	pd := workflow.ProjectDir(wd)
	if pd == "" {
		return nil
	}
	return []string{pd}
}

func registerDashboardWorkflowsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/workflows", handleDashboardWorkflowsCreate)
	mux.HandleFunc("/api/workflows/", handleDashboardWorkflowsAPI)
}

// handleDashboardWorkflowsCreate handles POST /api/workflows: resolve+load the
// referenced template, SNAPSHOT its chart + node defs into a new instance, lay
// down every node (entry nodes ready, the rest pending), and record the
// instance_created event. Returns {"id": <instanceID>}.
func handleDashboardWorkflowsCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body workflowCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, fail := createWorkflowInstance(body)
	if fail != nil {
		http.Error(w, fail.Msg, fail.Status)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// workflowCreateBody is the instantiate wire shape shared by the dashboard POST
// and the /v1 socket twin.
type workflowCreateBody struct {
	TemplateRef string          `json:"template_ref"`
	Title       string          `json:"title"`
	Params      json.RawMessage `json:"params"`
	Group       string          `json:"group"` // optional agent-group NAME to bind; "" = unbound
}

// createWorkflowInstance is the shared instantiation core: resolve + snapshot
// the template, bind an optional group, validate required params, insert the
// instance + all nodes (entry→ready, rest→pending), and emit the creation
// events. Returns {id, group_id} on success or a typed *spawnFailure. The caller
// authenticates/authorises first (dashboard cookie, or /v1 human/owner check).
func createWorkflowInstance(body workflowCreateBody) (map[string]any, *spawnFailure) {
	ref := strings.TrimSpace(body.TemplateRef)
	if ref == "" {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "template_ref is required"}
	}
	tmpl, err := workflow.Resolve(ref, workflowProjectDirsFn()...)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "resolve template: " + err.Error()}
	}

	// Optional group binding. The instance stores a soft link by group_id
	// (Step 2 schema); we accept the friendlier group NAME on the wire and
	// resolve it. Instantiation never auto-creates a group — an unknown name
	// is a 400, so a typo can't silently leave the instance unbound.
	var groupID int64
	if gname := strings.TrimSpace(body.Group); gname != "" {
		g, gerr := db.GetAgentGroupByName(gname)
		if gerr != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", "lookup group: " + gerr.Error()}
		}
		if g == nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				"group " + strconv.Quote(gname) + " not found (create it first; instantiation does not auto-create groups)"}
		}
		groupID = g.ID
	}

	// Params: an opaque JSON object. We only validate that every required
	// param is present — interpolation into prompts/commands is the engine's
	// job. Store a canonical re-marshal so the column is clean JSON.
	params := map[string]any{}
	if len(body.Params) > 0 {
		if err := json.Unmarshal(body.Params, &params); err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "params must be a JSON object: " + err.Error()}
		}
	}
	var missing []string
	for _, p := range tmpl.Params {
		if p.IsRequired() {
			if _, ok := params[p.Name]; !ok {
				missing = append(missing, p.Name)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "missing required params: " + strings.Join(missing, ", ")}
	}
	paramsJSON, _ := json.Marshal(params)

	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = tmpl.Name
	}

	inst := &db.WorkflowInstance{
		TemplateRef:  tmpl.Ref,
		TemplateName: tmpl.Name,
		Title:        title,
		Status:       db.WorkflowStatusRunning,
		Mermaid:      tmpl.Mermaid, // snapshot — later template edits never reshape this instance
		Params:       string(paramsJSON),
		Vars:         "{}",
		GroupID:      groupID,             // 0 = unbound
		EngineMode:   string(tmpl.Engine), // snapshot system|agent — later template edits never re-home the engine (JOH-15 B1)
	}
	id, err := db.InsertWorkflowInstance(inst)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "create instance: " + err.Error()}
	}

	entry := map[string]bool{}
	for _, e := range tmpl.Entry {
		entry[e] = true
	}
	ids := sortedTemplateNodeIDs(tmpl)
	nodes := make([]*db.WorkflowNode, 0, len(ids))
	for _, nid := range ids {
		n := tmpl.Nodes[nid]
		detail, _ := json.Marshal(n) // node-def snapshot; round-trips back via RebuildFromSnapshot
		status := db.WorkflowNodeStatusPending
		if entry[nid] {
			status = db.WorkflowNodeStatusReady
		}
		nodes = append(nodes, &db.WorkflowNode{
			NodeID:       nid,
			Label:        tmpl.DisplayLabel(nid),
			ExecutorKind: string(n.Executor.Kind),
			Status:       status,
			Detail:       string(detail),
		})
	}
	if err := db.InsertWorkflowNodes(id, nodes); err != nil {
		// Roll back the instance so a failed node batch leaves no orphan.
		_ = db.DeleteWorkflowInstance(id)
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "create nodes: " + err.Error()}
	}

	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
		InstanceID: id,
		Kind:       db.WorkflowEventInstanceCreated,
		Message:    "instantiated " + tmpl.Ref,
	})
	for _, nid := range ids {
		if entry[nid] {
			_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
				InstanceID: id, NodeID: nid, Kind: db.WorkflowEventNodeReady,
			})
		}
	}

	return map[string]any{"id": id, "group_id": groupID}, nil
}

// handleDashboardWorkflowsAPI dispatches the /api/workflows/{id}... surface:
//
//	GET    /api/workflows/{id}                         → full detail
//	DELETE /api/workflows/{id}                         → delete (cascades nodes+events)
//	POST   /api/workflows/{id}/cancel                  → cancel
//	PATCH  /api/workflows/{id}/nodes/{nodeId}          → manual node update + advance
//	GET    /api/workflows/{id}/nodes/{nodeId}/audit    → that node's event timeline
//	POST   /api/workflows/{id}/nodes/{nodeId}/start    → spawn the node's ai agent into the bound group
//	POST   /api/workflows/{id}/nodes/{nodeId}/attach   → attach an existing group member to the node
//	POST   /api/workflows/{id}/nodes/{nodeId}/approve  → human-verify gate (approve/reject)
func handleDashboardWorkflowsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/workflows/"), "/")
	if rest == "" {
		http.Error(w, "expected /api/workflows/{id}", http.StatusNotFound)
		return
	}
	parts := strings.SplitN(rest, "/", 4)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "id must be an integer", http.StatusBadRequest)
		return
	}
	inst, err := db.GetWorkflowInstance(id)
	if err != nil {
		http.Error(w, "lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if inst == nil {
		http.Error(w, "workflow "+strconv.FormatInt(id, 10)+" not found", http.StatusNotFound)
		return
	}

	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			dashboardWorkflowDetail(w, inst)
		case http.MethodDelete:
			// Serialise the delete against any in-flight drive on this instance
			// (start's up-to-30s spawn poll, a settle's advance) under the SAME
			// per-instance lock the mutating handlers take. Without it a delete
			// could wipe the rows mid read-modify-write, and removing the map
			// entry while another goroutine still holds the mutex would let a
			// fresh caller mint a second mutex for the same id — breaking the
			// mutual exclusion. Holding the lock here closes both.
			unlock := lockWorkflowInstance(id)
			delErr := db.DeleteWorkflowInstance(id)
			if delErr == nil {
				workflowInstanceLocks.Delete(id) // row is gone; drop the now-unreachable mutex
			}
			unlock()
			if delErr != nil {
				http.Error(w, "delete: "+delErr.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "GET or DELETE on /api/workflows/{id}", http.StatusMethodNotAllowed)
		}
	case parts[1] == "cancel" && len(parts) == 2:
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		dashboardWorkflowCancel(w, inst)
	case parts[1] == "nodes" && len(parts) >= 3 && parts[2] != "":
		sub := ""
		if len(parts) == 4 {
			sub = parts[3]
		}
		dashboardWorkflowNode(w, r, inst, parts[2], sub)
	default:
		http.Error(w, "unknown path under /api/workflows/"+strconv.FormatInt(id, 10), http.StatusNotFound)
	}
}

// dashboardWorkflowDetail returns one instance with its nodes, snapshotted
// chart, params/vars, and recent timeline — everything the detail view renders.
func dashboardWorkflowDetail(w http.ResponseWriter, inst *db.WorkflowInstance) {
	writeJSON(w, http.StatusOK, workflowDetailJSON(inst))
}

// workflowDetailJSON builds the full instance-detail payload (instance + mermaid
// + params/vars + nodes + recent events + topology warnings) shared by the
// dashboard GET and the /v1 socket twin.
func workflowDetailJSON(inst *db.WorkflowInstance) map[string]any {
	nodes, _ := db.ListWorkflowNodes(inst.ID)
	events, _ := db.ListWorkflowEvents(inst.ID)
	const maxEvents = 200
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	tmpl, _ := rebuildInstanceTemplate(inst) // best-effort; nil → no allowed-outcome hints

	nodeViews := make([]workflowNodeJSON, 0, len(nodes))
	for _, n := range nodes {
		nodeViews = append(nodeViews, nodeToJSON(n, tmpl))
	}
	eventViews := make([]workflowEventJSON, 0, len(events))
	for _, e := range events {
		eventViews = append(eventViews, eventToJSON(e))
	}

	// Topology warnings (Step 2b smells). RebuildFromSnapshot skips analysis,
	// so re-run it here over the instance's snapshotted chart — the warnings
	// match what the template carried at instantiation. Non-nil so the
	// front-end's .map() is safe even when clean.
	warnings := []string{}
	if tmpl != nil {
		warnings = append(warnings, tmpl.Analyze()...)
	}

	return map[string]any{
		"instance": instanceDetailJSON(inst),
		"mermaid":  inst.Mermaid,
		"params":   rawJSONOrEmpty(inst.Params),
		"vars":     rawJSONOrEmpty(inst.Vars),
		"nodes":    nodeViews,
		"events":   eventViews,
		"warnings": warnings,
	}
}

// dashboardWorkflowNode dispatches the per-node subroutes.
func dashboardWorkflowNode(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, nodeID, sub string) {
	node, err := db.GetWorkflowNode(inst.ID, nodeID)
	if err != nil {
		http.Error(w, "lookup node: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if node == nil {
		http.Error(w, "node "+nodeID+" not found", http.StatusNotFound)
		return
	}
	switch sub {
	case "":
		if r.Method != http.MethodPatch {
			http.Error(w, "PATCH only on /nodes/{nodeId}", http.StatusMethodNotAllowed)
			return
		}
		dashboardWorkflowNodePatch(w, r, inst, node)
	case "audit":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		events, _ := db.ListWorkflowEvents(inst.ID, nodeID)
		out := make([]workflowEventJSON, 0, len(events))
		for _, e := range events {
			out = append(out, eventToJSON(e))
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": out})
	case "start", "attach", "approve":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only on /nodes/{nodeId}/"+sub, http.StatusMethodNotAllowed)
			return
		}
		switch sub {
		case "start":
			dashboardWorkflowNodeStart(w, inst, node.NodeID)
		case "attach":
			dashboardWorkflowNodeAttach(w, r, inst, node.NodeID)
		case "approve":
			dashboardWorkflowNodeApprove(w, r, inst, node.NodeID)
		}
	default:
		http.Error(w, "unknown node subpath "+sub, http.StatusNotFound)
	}
}

// dashboardWorkflowNodePatch applies a manual node update — the MVP driving
// path. A status of done/failed is a settle: it runs workflow.Advance to ready
// the taken successors and skip the abandoned branches, then recomputes the
// instance status. Events are appended for every transition.
func dashboardWorkflowNodePatch(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, node *db.WorkflowNode) {
	var body workflowNodePatchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, fail := applyWorkflowNodePatch(inst.ID, node.NodeID, body)
	if fail != nil {
		http.Error(w, fail.Msg, fail.Status)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// workflowNodePatchBody is the wire shape for a manual node update — shared by
// the dashboard PATCH and the /v1 socket twin. All fields optional; a settling
// status (done/failed) drives Advance.
type workflowNodePatchBody struct {
	Status   *string `json:"status"`
	Outcome  *string `json:"outcome"`
	Output   *string `json:"output"`
	Assignee *string `json:"assignee"`
}

// applyWorkflowNodePatch is the shared core behind both the dashboard PATCH and
// the /v1 socket node-PATCH: it takes the (already authorised) patch, applies it
// under the per-instance lock with all the guards (re-settle 409, manual-drive
// status allowlist, engine-sentinel rejection, outcome validation), settles +
// advances the graph on a done/failed, and recomputes the instance status. It
// returns the JSON result map on success, or a *spawnFailure (reused as a
// typed status+message carrier) the caller maps onto its transport's error.
//
// Authorisation is the CALLER's job (done before this) — the dashboard's cookie
// gate, or the /v1 twin's assignee-or-human/owner check — because the two
// surfaces authenticate differently; this core only enforces state correctness.
func applyWorkflowNodePatch(instanceID int64, nodeID string, body workflowNodePatchBody) (map[string]any, *spawnFailure) {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	// Re-read fresh under the lock: a concurrent settle/cancel/start may have
	// moved this node or the instance since the caller fetched them, and the
	// guards below must see current state.
	inst, ierr := db.GetWorkflowInstance(instanceID)
	if ierr != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "lookup instance: " + ierr.Error()}
	}
	if inst == nil {
		return nil, &spawnFailure{http.StatusNotFound, "not_found", "workflow not found"}
	}
	node, ferr := db.GetWorkflowNode(instanceID, nodeID)
	if ferr != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "lookup node: " + ferr.Error()}
	}
	if node == nil {
		return nil, &spawnFailure{http.StatusNotFound, "not_found", "node " + nodeID + " not found"}
	}

	// Re-settling an already-terminal node is rejected: it would append a
	// duplicate node_done/failed event and re-run Advance over now-stale state
	// (successors already moved past pending), so the second advance is a
	// no-op at best and a wrong skip at worst. A patch that only touches
	// output/assignee on a settled node is still allowed.
	if isSettledWorkflowNodeStatus(node.Status) && body.Status != nil &&
		isSettledWorkflowNodeStatus(strings.TrimSpace(*body.Status)) {
		return nil, &spawnFailure{http.StatusConflict, "conflict", "node " + nodeID + " is already " + node.Status + "; cannot re-settle"}
	}

	tmpl, _ := rebuildInstanceTemplate(inst) // nil only on an unparsable snapshot

	now := time.Now()
	patch := db.WorkflowNodePatch{}
	newStatus := node.Status
	if body.Status != nil {
		s := strings.TrimSpace(*body.Status)
		if !validWorkflowNodeStatus(s) {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "invalid node status " + strconv.Quote(s)}
		}
		// Manual PATCH may only drive a node to running / done / failed. A
		// direct hop to "skipped" would settle the node WITHOUT running
		// Advance, stranding the sub-tree behind it; "pending"/"ready"/
		// "awaiting_verify" are engine-internal frontier states. Skipping a
		// branch is reached by cancelling the instance, and the frontier is
		// only ever moved by a settle's Advance. (Cold-review #230 finding.)
		if !isManualDriveStatus(s) {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				"node status " + strconv.Quote(s) + " is not manually settable; " +
					"use running, done or failed (skip a branch by cancelling the instance)"}
		}
		// ai-verify interception: a worker reporting its node `done`, on a node
		// whose definition-of-done is an AI judge, does NOT settle — it parks in
		// awaiting_verify for the engine to spawn a judge (JOH-35). The park fires
		// ONLY from `running` (the executor-done report); the judge's later verdict
		// PATCH comes from awaiting_verify and settles for real, so there is no
		// re-park loop. When ai-verify can't run (engine off / unbound instance) it
		// falls through to the normal settle — the slice-B self-report fallback —
		// so a dashboard-only flow still completes instead of stranding the node.
		if s == db.WorkflowNodeStatusDone && node.Status == db.WorkflowNodeStatusRunning &&
			nodeWantsAIVerify(tmpl, nodeID) && aiVerifyCanRun(inst) {
			return parkNodeForAIVerify(instanceID, nodeID, body)
		}
		// In-place retry interception (JOH-39): an ai node reporting `failed`, with
		// retry budget left and an engine that can re-run it (bound group + engine
		// on — the same gate ai-verify uses), re-arms to ready instead of settling.
		// The next tick re-spawns the agent (a fresh conv, so its JOH-40 handoff
		// re-fires with the failure context as the fix brief). Falls through to a
		// real settle once the budget is spent or the engine can't re-run it (so a
		// dashboard-only / engine-off flow still fails cleanly). Only fires from a
		// `running` report (the worker's own verdict), never re-interpreting a
		// human's later force-fail of an already-settled node.
		if s == db.WorkflowNodeStatusFailed && node.Status == db.WorkflowNodeStatusRunning &&
			node.ExecutorKind == string(workflow.ExecAI) && aiVerifyCanRun(inst) &&
			tmpl != nil && tmpl.Nodes[nodeID] != nil &&
			retriesUsedThisActivation(instanceID, nodeID) < tmpl.Nodes[nodeID].Retries {
			return rearmAINodeForRetry(instanceID, nodeID, deref(body.Output))
		}
		patch.Status = &s
		newStatus = s
		if s == db.WorkflowNodeStatusRunning && node.StartedAt.IsZero() {
			patch.StartedAt = &now
		}
		if isSettledWorkflowNodeStatus(s) {
			patch.FinishedAt = &now
			if node.StartedAt.IsZero() {
				patch.StartedAt = &now
			}
		}
	}
	if body.Output != nil {
		patch.Output = body.Output
	}
	if body.Assignee != nil {
		// A client must not be able to stamp the engine-owner sentinel: it is the
		// marker the startup reaper trusts to tell an engine corpse from a
		// human-driven running node, so letting a manual PATCH set it would let a
		// caller trick the reaper into resetting (and the engine into re-running)
		// an arbitrary node. The sentinel is engine-internal only.
		if strings.TrimSpace(*body.Assignee) == engineAssignee {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				"assignee " + strconv.Quote(engineAssignee) + " is reserved for the engine"}
		}
		patch.Assignee = body.Assignee
	}

	// Resolve the settling outcome (and validate it) before mutating anything,
	// so a bad outcome is a clean 400 with no partial write.
	settling := body.Status != nil && (newStatus == db.WorkflowNodeStatusDone || newStatus == db.WorkflowNodeStatusFailed)
	var settleOutcome string
	if settling {
		if newStatus == db.WorkflowNodeStatusFailed {
			settleOutcome = workflow.OutcomeFail
		} else { // done
			settleOutcome = strings.TrimSpace(deref(body.Outcome))
			if settleOutcome == "" {
				if tmpl != nil && isEnumNode(tmpl, nodeID) {
					return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
						"node " + nodeID + " is enum-verified; an outcome is required"}
				}
				settleOutcome = workflow.OutcomePass
			}
			if tmpl != nil && !slices.Contains(tmpl.AllowedOutcomes(nodeID), settleOutcome) {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					"outcome " + strconv.Quote(settleOutcome) + " is not valid for node " + nodeID +
						" (allowed: " + strings.Join(tmpl.AllowedOutcomes(nodeID), ", ") + ")"}
			}
		}
		patch.Outcome = &settleOutcome
	} else if body.Outcome != nil {
		o := strings.TrimSpace(*body.Outcome)
		patch.Outcome = &o
	}

	if _, err := db.UpdateWorkflowNode(instanceID, nodeID, patch); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "update node: " + err.Error()}
	}
	if body.Status != nil {
		appendNodeStatusEvent(instanceID, nodeID, newStatus)
	}

	// Settle → advance the graph, but only while the instance is still
	// running. A terminal instance (completed / failed / cancelled) must not
	// be re-driven: advancing or recomputing it from a stray node PATCH would
	// resurrect a cancelled instance back to completed, or un-complete a
	// finished one. The node field write above still lands (bookkeeping);
	// the instance status is left frozen.
	instanceRunning := inst.Status == db.WorkflowStatusRunning
	advanced := workflow.AdvanceResult{Ready: []string{}, Skipped: []string{}}
	if settling && tmpl != nil && instanceRunning {
		advanced = workflow.Advance(tmpl, nodeID, settleOutcome, nodeStateMap(instanceID))
		applyWorkflowAdvance(instanceID, nodeID, tmpl, advanced, now)
	}

	newInstStatus := inst.Status
	if instanceRunning {
		nodes, _ := db.ListWorkflowNodes(instanceID)
		newInstStatus = recomputeWorkflowInstanceStatus(tmpl, nodes)
		if newInstStatus != inst.Status {
			if _, err := db.UpdateWorkflowInstanceStatus(instanceID, newInstStatus); err == nil {
				appendInstanceStatusEvent(instanceID, newInstStatus)
			}
		}
	}

	return map[string]any{
		"ok":              true,
		"node_id":         nodeID,
		"status":          newStatus,
		"instance_status": newInstStatus,
		"ready":           advanced.Ready,
		"skipped":         advanced.Skipped,
	}, nil
}

// parkNodeForAIVerify lands an ai-executor node in awaiting_verify when its
// worker reports done but the node's verify.kind is ai — the executor is done,
// but the definition-of-done is a judge agent the engine will spawn. It captures
// the worker's reported output (so the judge can inspect it) and CLEARS the
// assignee: an empty assignee on an awaiting_verify node is the engine judge
// pass's "ready to judge" marker (the worker no longer owns the node, so it can't
// self-approve). It does NOT advance — the node settles only on the judge's
// verdict. The caller holds the per-instance lock.
func parkNodeForAIVerify(instanceID int64, nodeID string, body workflowNodePatchBody) (map[string]any, *spawnFailure) {
	awaiting := db.WorkflowNodeStatusAwaitingVerify
	cleared := ""
	patch := db.WorkflowNodePatch{Status: &awaiting, Assignee: &cleared}
	if body.Output != nil {
		patch.Output = body.Output
	}
	if _, err := db.UpdateWorkflowNode(instanceID, nodeID, patch); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "park for ai-verify: " + err.Error()}
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
		InstanceID: instanceID, NodeID: nodeID, Kind: db.WorkflowEventNodeAwaitingVerify,
		Message: "executor reported done; awaiting ai-verify judge",
	})
	return map[string]any{
		"ok":              true,
		"node_id":         nodeID,
		"status":          awaiting,
		"instance_status": db.WorkflowStatusRunning,
		"ready":           []string{},
		"skipped":         []string{},
	}, nil
}

// rearmAINodeForRetry re-arms a failed ai node for an in-place retry (JOH-39):
// it captures the worker's reported output (so the retry's fresh agent gets the
// failure context via its JOH-40 handoff), CLEARS the assignee, and sets the
// node back to ready so the engine's next tick re-spawns the agent. A node_retry
// event records the attempt (and feeds retriesUsedThisActivation). It does NOT
// advance — the node has not settled. The caller holds the per-instance lock.
func rearmAINodeForRetry(instanceID int64, nodeID, output string) (map[string]any, *spawnFailure) {
	ready := db.WorkflowNodeStatusReady
	cleared := ""
	patch := db.WorkflowNodePatch{Status: &ready, Assignee: &cleared}
	if output != "" {
		patch.Output = &output
	}
	if _, err := db.UpdateWorkflowNode(instanceID, nodeID, patch); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "re-arm for retry: " + err.Error()}
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
		InstanceID: instanceID, NodeID: nodeID, Kind: db.WorkflowEventNodeRetry,
		Message: "ai node reported failed; retrying in place (re-spawn next tick)",
	})
	return map[string]any{
		"ok":              true,
		"node_id":         nodeID,
		"status":          ready,
		"instance_status": db.WorkflowStatusRunning,
		"ready":           []string{},
		"skipped":         []string{},
	}, nil
}

// applyWorkflowAdvance writes the proposed transitions: pending → ready for each
// taken successor, pending → skipped for each abandoned branch, and — for a
// back-edge loop-back (res.Reentry) — resets the loop body so the iteration
// re-runs (JOH-39). Only nodes still pending are touched by Ready/Skipped, so a
// re-run (or a node a human already moved) is a no-op there. settledID is the
// node that just settled (the back-edge source); tmpl supplies the topology for
// the loop-body computation. The caller holds the per-instance lock.
func applyWorkflowAdvance(instanceID int64, settledID string, tmpl *workflow.Template, res workflow.AdvanceResult, now time.Time) {
	for _, nid := range res.Ready {
		cur, _ := db.GetWorkflowNode(instanceID, nid)
		if cur == nil || cur.Status != db.WorkflowNodeStatusPending {
			continue
		}
		st := db.WorkflowNodeStatusReady
		_, _ = db.UpdateWorkflowNode(instanceID, nid, db.WorkflowNodePatch{Status: &st})
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: nid, Kind: db.WorkflowEventNodeReady})
	}
	for _, nid := range res.Skipped {
		cur, _ := db.GetWorkflowNode(instanceID, nid)
		if cur == nil || cur.Status != db.WorkflowNodeStatusPending {
			continue
		}
		st := db.WorkflowNodeStatusSkipped
		fin := now
		_, _ = db.UpdateWorkflowNode(instanceID, nid, db.WorkflowNodePatch{Status: &st, FinishedAt: &fin})
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: nid, Kind: db.WorkflowEventNodeSkipped})
	}
	for _, target := range res.Reentry {
		reenterLoop(instanceID, settledID, target, tmpl)
	}
}

// reenterLoop re-arms a back-edge loop: the loop body (nodes on a path from
// target to the back-edge source settledID — see workflow.Template.LoopBody) is
// reset so the iteration re-runs. The re-entry target → ready; every other
// settled body node (done/failed/skipped) → pending so the normal advance
// re-readies it from THIS iteration's outcomes (a branch skipped last time may be
// taken now — we never preserve a prior iteration's skip/branch decisions).
// Assignee + outcome + finished_at are cleared on every reset node, so an ai node
// re-spawns under a fresh conv (which re-fires its JOH-40 handoff with the new
// iteration's input). Visits is NOT reset — it is the absolute execution counter
// the claim-time MaxVisits guard reads, so a runaway loop still halts. Events are
// append-only (node_reentry marks each reset); the visit counter in the node row
// distinguishes iterations on the timeline. The caller holds the instance lock.
//
// Two known limitations, acceptable for the common single-predecessor fix-loop
// and flagged for the rare cases: (1) the target is re-armed straight to ready
// without re-checking joinReady, so a multi-predecessor JoinAll loop target could
// re-arm before a concurrent live predecessor delivers; (2) the body is reset via
// N separate row updates, not one transaction — a daemon crash mid-reset leaves a
// half-reset body until the next tick re-derives (bounded: the whole reset runs
// under the instance lock, so only a crash, not another op, can interleave).
func reenterLoop(instanceID int64, settledID, target string, tmpl *workflow.Template) {
	if tmpl == nil {
		return
	}
	body := tmpl.LoopBody(target, settledID)
	ready := db.WorkflowNodeStatusReady
	pending := db.WorkflowNodeStatusPending
	cleared := ""
	var zeroTime time.Time
	for id := range body {
		cur, _ := db.GetWorkflowNode(instanceID, id)
		if cur == nil {
			continue
		}
		if id == target {
			// The re-entry point itself becomes ready for the new iteration.
			_, _ = db.UpdateWorkflowNode(instanceID, id, db.WorkflowNodePatch{
				Status: &ready, Outcome: &cleared, FinishedAt: &zeroTime, Assignee: &cleared})
			_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: id,
				Kind: db.WorkflowEventNodeReentry, Message: "engine: loop re-entry via back-edge from " + settledID})
			continue
		}
		// Other body nodes: reset settled ones to pending so they re-run; leave a
		// still-live one (pending/ready/running/awaiting) alone — it has not yet
		// fired this iteration.
		switch cur.Status {
		case db.WorkflowNodeStatusDone, db.WorkflowNodeStatusFailed, db.WorkflowNodeStatusSkipped:
			_, _ = db.UpdateWorkflowNode(instanceID, id, db.WorkflowNodePatch{
				Status: &pending, Outcome: &cleared, FinishedAt: &zeroTime, Assignee: &cleared})
			_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: id,
				Kind: db.WorkflowEventNodeReentry, Message: "engine: reset for loop re-entry"})
		}
	}
}

// ----- group integration: start / attach / approve (Step 4) -----------

// workflowInstanceLocks serialises mutating operations on a single workflow
// instance. The PATCH / start / attach / approve / cancel / delete paths all do
// a read-modify-write over node + frontier state; without this, two concurrent
// drives (a double "start" click, or a start racing a settle) could both read
// the same `ready` node and both act on it — double-spawning, or advancing over
// stale state.
//
// Keyed by instance id (monotonic, never reused). An entry is created on first
// touch and removed only when the instance is DELETEd, so a terminal-but-kept
// instance retains its entry until deletion: the map grows by instances-ever-
// driven, never thrashes. Each entry is one pointer-sized mutex, so the
// footprint is negligible for a daemon's lifetime; instances are reclaimed when
// the human deletes them from the dashboard.
var workflowInstanceLocks sync.Map // int64 → *sync.Mutex

// lockWorkflowInstance acquires the per-instance mutex and returns its unlock
// func, for use as `defer lockWorkflowInstance(id)()`. Mutating handlers re-read
// instance/node state AFTER acquiring it, so the guards act on current state.
func lockWorkflowInstance(id int64) func() {
	actual, _ := workflowInstanceLocks.LoadOrStore(id, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// isManualDriveStatus reports whether a node status may be set via the manual
// PATCH endpoint. running/done/failed are the human-driving transitions;
// pending/ready/awaiting_verify are engine-internal frontier states, and
// "skipped" is reached only through a settle's Advance (or cancel) — never a
// direct manual hop, which would settle the node WITHOUT readying/skipping its
// successors and strand the sub-tree behind it.
func isManualDriveStatus(s string) bool {
	switch s {
	case db.WorkflowNodeStatusRunning, db.WorkflowNodeStatusDone, db.WorkflowNodeStatusFailed:
		return true
	default:
		return false
	}
}

// readyAINodeOrFail re-reads (instance, node) fresh and enforces the shared
// start/attach preconditions as TYPED failures (rather than writing HTTP): the
// instance is running, the node exists, is an ai-executor node, and is `ready`
// (on the frontier). The caller MUST already hold lockWorkflowInstance(instID).
// Both the dashboard wrapper (reloadReadyAINode) and the /v1 spawn-into-node core
// (spawnWorkerIntoNodeCore) wrap this so the precondition logic lives once.
func readyAINodeOrFail(instID int64, nodeID string) (*db.WorkflowInstance, *db.WorkflowNode, *spawnFailure) {
	inst, err := db.GetWorkflowInstance(instID)
	if err != nil {
		return nil, nil, &spawnFailure{http.StatusInternalServerError, "io", "lookup: " + err.Error()}
	}
	if inst == nil {
		return nil, nil, &spawnFailure{http.StatusNotFound, "not_found", "workflow not found"}
	}
	if inst.Status != db.WorkflowStatusRunning {
		return nil, nil, &spawnFailure{http.StatusConflict, "conflict", "instance is " + inst.Status + "; cannot start/attach a node"}
	}
	node, err := db.GetWorkflowNode(instID, nodeID)
	if err != nil {
		return nil, nil, &spawnFailure{http.StatusInternalServerError, "io", "lookup node: " + err.Error()}
	}
	if node == nil {
		return nil, nil, &spawnFailure{http.StatusNotFound, "not_found", "node " + nodeID + " not found"}
	}
	if node.ExecutorKind != string(workflow.ExecAI) {
		return nil, nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"start/attach apply only to ai-executor nodes (node " + nodeID + " is " + strconv.Quote(node.ExecutorKind) + ")"}
	}
	if node.Status != db.WorkflowNodeStatusReady {
		return nil, nil, &spawnFailure{http.StatusConflict, "conflict",
			"node " + nodeID + " is " + node.Status + "; only a ready node can be started/attached"}
	}
	return inst, node, nil
}

// reloadReadyAINode is the dashboard's HTTP wrapper over readyAINodeOrFail: it
// writes the matching 4xx/5xx and returns ok=false on any violation. The caller
// MUST already hold lockWorkflowInstance(instID).
func reloadReadyAINode(w http.ResponseWriter, instID int64, nodeID string) (*db.WorkflowInstance, *db.WorkflowNode, bool) {
	inst, node, fail := readyAINodeOrFail(instID, nodeID)
	if fail != nil {
		http.Error(w, fail.Msg, fail.Status)
		return nil, nil, false
	}
	return inst, node, true
}

// boundGroupOrFail resolves the instance's bound agent group as a TYPED failure
// when the instance is unbound / its group no longer exists.
func boundGroupOrFail(inst *db.WorkflowInstance) (*db.AgentGroup, *spawnFailure) {
	if inst.GroupID == 0 {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"instance has no bound group; bind one at create (POST /api/workflows {\"group\":\"<name>\"})"}
	}
	g, err := db.GetAgentGroupByID(inst.GroupID)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "lookup group: " + err.Error()}
	}
	if g == nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"bound group (id " + strconv.FormatInt(inst.GroupID, 10) + ") no longer exists"}
	}
	return g, nil
}

// boundGroup is the dashboard's HTTP wrapper over boundGroupOrFail.
func boundGroup(w http.ResponseWriter, inst *db.WorkflowInstance) (*db.AgentGroup, bool) {
	g, fail := boundGroupOrFail(inst)
	if fail != nil {
		http.Error(w, fail.Msg, fail.Status)
		return nil, false
	}
	return g, true
}

// spawnWorkerIntoNodeCore is the shared spawn-worker-into-node path behind both
// the dashboard POST .../nodes/{id}/start and the /v1 socket twin (the agent-engine
// driver's spawn verb, JOH-15). It takes the (already authorised) instance + node
// ids and an optional seedContext (the driver's per-spawn briefing addendum —
// JOH-15 B2a `--context`), and runs the SAME claim → spawn-off-lock → settle shape
// the engine's own auto-spawn path uses (claimNextAINode → executeSpawn →
// settleAISpawn), so guards, state, and the spawned-into-group result are
// byte-identical across all three surfaces. Returns the JSON result map on success
// or a typed *spawnFailure the caller maps onto its transport.
//
// Authorisation is the CALLER's job (the dashboard cookie gate, or the /v1
// group-owner check) — this core only enforces state correctness, mirroring
// applyWorkflowNodePatch.
//
// THE OFF-LOCK SHAPE (JOH-15 B2a, replacing B1's documented carry-over): the spawn
// runs in THREE phases so the slow part holds no lock —
//  1. CLAIM under the per-instance lock (claimManualAISpawn): re-read fresh, enforce
//     the start preconditions (running instance, ready ai node, bound group), mark
//     the node running + the engine sentinel, snapshot the spawn params.
//  2. SPAWN with the lock RELEASED: executeSpawn polls up to ~30s for the new
//     conv-id. Holding the per-instance lock across that poll (B1's behaviour) stalls
//     a concurrent engine tick on THIS instance — the tick takes the same lock every
//     pass — for the whole handshake. An agent-driver hammers this path, so the
//     stall is real; the engine's own ai path already spawns off-lock (goBackground).
//     This path stays SYNCHRONOUS (the caller wants the conv-id back) but no longer
//     holds the lock across the handshake.
//  3. SETTLE under the lock (settleAISpawn, shared with the engine): re-read fresh,
//     validate the claim is still ours, swap the sentinel for the conv-id on success
//     (else reset to ready), reporting a typed failure if the claim was invalidated
//     mid-spawn.
func spawnWorkerIntoNodeCore(instanceID int64, nodeID, seedContext string) (map[string]any, *spawnFailure) {
	claim, fail := claimManualAISpawn(instanceID, nodeID, seedContext)
	if fail != nil {
		return nil, fail
	}

	// Lock released — executeSpawn's ~30s conv-id handshake runs off-lock so a
	// concurrent engine tick on this instance is not stalled behind it.
	outcome, spawnFail := executeSpawn(claim.group, claim.params)

	// Settle under the lock (shared with the engine's auto-spawn path): a non-nil
	// failure is a spawn error or a claim invalidated mid-spawn; nil means the
	// sentinel→conv-id swap landed and the result map is safe to build from outcome.
	if sf := settleAISpawn(instanceID, claim, outcome, spawnFail); sf != nil {
		return nil, sf
	}

	return map[string]any{
		"ok": true, "node_id": nodeID, "status": db.WorkflowNodeStatusRunning,
		"assignee": outcome.ConvID, "conv_id": outcome.ConvID,
		"label": outcome.Label, "tmux_session": outcome.TmuxSession,
		"attach_cmd": "tclaude session attach " + outcome.Label,
	}, nil
}

// claimManualAISpawn is the manual/driver twin of the engine's claimNextAINode
// (JOH-15 B2a): it claims a SPECIFIC ready ai node for the dashboard-start / driver
// spawn-into-node path under the per-instance lock and returns the snapshot to spawn
// it OFF the lock. It differs from claimNextAINode in that it (a) targets an explicit
// nodeID rather than scanning for the next ready one, (b) reports preconditions as
// TYPED failures to the synchronous caller (the engine logs+skips; this path returns
// an error the dashboard/CLI surfaces), and (c) does NOT apply the autonomous opt-in
// / parallelism caps — a human or group-owner driver starting a node is an explicit,
// deliberate action, not the daemon auto-driving. It SHARES the engine sentinel +
// settleAISpawn so the two paths cannot drift, and folds the driver's seedContext
// into the worker brief.
func claimManualAISpawn(instanceID int64, nodeID, seedContext string) (*aiNodeClaim, *spawnFailure) {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, node, fail := readyAINodeOrFail(instanceID, nodeID)
	if fail != nil {
		return nil, fail
	}
	g, fail := boundGroupOrFail(inst)
	if fail != nil {
		return nil, fail
	}
	cwd, cwdErr := resolveSpawnCwd(g.DefaultCwd)
	if cwdErr != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "resolve group cwd: " + cwdErr.Error()}
	}

	var def workflow.Node
	if tmpl, _ := rebuildInstanceTemplate(inst); tmpl != nil && tmpl.Nodes[nodeID] != nil {
		def = *tmpl.Nodes[nodeID]
	}

	// Per-node max_visits (JOH-39) is a SUBSTRATE guarantee that must hold in BOTH
	// engine modes: the engine's claimNextAINode enforces it at claim time, and so
	// must this manual/driver path. Otherwise an agent-engine driver could re-spawn a
	// looping ai node past its cap — a back-edge settle re-arms the node to `ready`
	// with no cap check (same as system mode), and Visits would then climb unbounded
	// through this path. (This is the per-node execution cap, NOT the daemon-side
	// driver-iteration cap, which is separately deferred.) Unlike the engine — which
	// force-fails + halts an UNATTENDED runaway — a deliberate driver/human start gets
	// a typed refusal and the node is left `ready` for the caller to decide.
	if cap, unbounded := workflow.EffectiveMaxVisits(&def, workflowMaxVisits); !unbounded && node.Visits >= int64(cap) {
		return nil, &spawnFailure{http.StatusConflict, "conflict",
			"node " + nodeID + " has reached its max_visits cap (" + strconv.Itoa(cap) +
				"); refusing to spawn another worker into it"}
	}

	// Interpolate the task prompt against the instance scope (params + captures) so a
	// brief referencing {{param}} / {{upstream.output}} resolves — the same the
	// engine's auto-spawn does. Unresolved refs are left visible (logged), not
	// blanked: a prompt is not shell, so the risk is prompt-injection, not execution.
	initMsg, missing := instanceScope(inst).Interpolate(strings.TrimSpace(def.Executor.Prompt))
	if len(missing) > 0 {
		slog.Warn("workflow start: node prompt has unresolved refs",
			"instance", instanceID, "node", nodeID, "missing", missing)
	}
	// Fold the driver's --context seed in as an additive briefing section (JOH-15
	// B2a): the prompt is the node's task; the seed is the upstream outputs the
	// agent-engine driver routed in (additive to the worker's own `workflow
	// where`/`status` self-view).
	initMsg = foldSeedContext(initMsg, seedContext)
	if initMsg != "" && !isValidInitialMessage(initMsg) {
		slog.Warn("workflow start: node brief is not a valid initial message; spawning without it",
			"instance", instanceID, "node", nodeID)
		initMsg = ""
	}

	// Claim it: mark running + the engine sentinel (the SAME marker the engine uses),
	// so a crash mid-spawn is recoverable by reapOrphanedEngineNodes (a sentinel-
	// bearing running node resets to ready on daemon startup) and settleAISpawn can
	// verify the claim is still ours before swapping in the conv-id.
	running := db.WorkflowNodeStatusRunning
	startedAt := time.Now()
	owner := engineAssignee
	if _, err := db.UpdateWorkflowNode(instanceID, nodeID,
		db.WorkflowNodePatch{Status: &running, StartedAt: &startedAt, Assignee: &owner}); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "claim node: " + err.Error()}
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: nodeID,
		Kind: db.WorkflowEventNodeStarted, Message: "spawning agent for ai node"})

	return &aiNodeClaim{
		nodeID: nodeID,
		group:  g,
		params: spawnParams{
			Name:           def.Executor.Agent,
			Role:           def.Executor.Agent,
			Descr:          "workflow " + strconv.FormatInt(instanceID, 10) + " · " + node.Label,
			InitialMessage: initMsg,
			Cwd:            cwd,
			GroupContext:   g.DefaultContext,
		},
	}, nil
}

// foldSeedContext appends the workflow driver's --context seed (JOH-15 B2a) to a
// node's task prompt as an additive, clearly-delimited briefing section. The task
// prompt stays primary; the seed carries the upstream outputs the agent-engine
// driver routed in. Either piece may be empty.
func foldSeedContext(prompt, seed string) string {
	prompt = strings.TrimSpace(prompt)
	seed = strings.TrimSpace(seed)
	switch {
	case seed == "":
		return prompt
	case prompt == "":
		return "Context from the workflow driver:\n\n" + seed
	default:
		return prompt + "\n\n---\nContext from the workflow driver:\n\n" + seed
	}
}

// dashboardWorkflowNodeStart is the dashboard HTTP wrapper over
// spawnWorkerIntoNodeCore — it maps a typed failure onto http.Error (matching the
// dashboardWorkflowNodePatch twin) and writes the JSON result on success. The
// dashboard start button seeds no driver context (""); that channel is the
// agent-engine driver's `--context`, surfaced on the /v1 twin.
func dashboardWorkflowNodeStart(w http.ResponseWriter, inst *db.WorkflowInstance, nodeID string) {
	res, fail := spawnWorkerIntoNodeCore(inst.ID, nodeID, "")
	if fail != nil {
		http.Error(w, fail.Msg, fail.Status)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// dashboardWorkflowNodeAttach assigns an EXISTING member of the bound group to a
// ready node — the "I already have a worker, hand it this node" path — instead
// of spawning a fresh one. The attachee's conv-id must already be a group
// member (attach assigns, it does not enroll). The node goes running, and the
// node's task prompt is delivered to that agent's inbox (best-effort).
func dashboardWorkflowNodeAttach(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, nodeID string) {
	unlock := lockWorkflowInstance(inst.ID)
	defer unlock()

	var body struct {
		ConvID string `json:"conv_id"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	convID := strings.TrimSpace(body.ConvID)
	if convID == "" {
		http.Error(w, "conv_id is required (the existing group member to attach)", http.StatusBadRequest)
		return
	}

	inst, node, ok := reloadReadyAINode(w, inst.ID, nodeID)
	if !ok {
		return
	}
	g, ok := boundGroup(w, inst)
	if !ok {
		return
	}
	members, _ := db.ListAgentGroupMembers(g.ID)
	if !convIsMember(members, convID) {
		http.Error(w, "conv "+strconv.Quote(convID)+" is not a member of the bound group "+strconv.Quote(g.Name),
			http.StatusBadRequest)
		return
	}

	now := time.Now()
	st := db.WorkflowNodeStatusRunning
	asg := convID
	if _, err := db.UpdateWorkflowNode(inst.ID, nodeID,
		db.WorkflowNodePatch{Status: &st, Assignee: &asg, StartedAt: &now}); err != nil {
		http.Error(w, "update node: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
		InstanceID: inst.ID, NodeID: nodeID, Kind: db.WorkflowEventNodeStarted,
		Message: "attached " + convID,
	})

	// Hand the node's task to the attached agent's inbox so the existing worker
	// actually receives the work, not just the assignment. Best-effort, and
	// gated on the same control-char/length check start applies before handing a
	// prompt to executeSpawn — the inbox render must stay clean either way.
	if tmpl, _ := rebuildInstanceTemplate(inst); tmpl != nil && tmpl.Nodes[nodeID] != nil {
		prompt := strings.TrimSpace(tmpl.Nodes[nodeID].Executor.Prompt)
		switch {
		case prompt == "":
			// no task body to deliver
		case !isValidInitialMessage(prompt):
			slog.Warn("workflow attach: node prompt is not a valid inbox message; assigned without delivering it",
				"conv", convID, "node", nodeID)
		default:
			if _, err := db.InsertAgentMessage(&db.AgentMessage{
				GroupID:      g.ID,
				ToConv:       convID,
				Subject:      "Workflow task: " + node.Label,
				Body:         prompt,
				ToRecipients: []string{convID},
			}); err != nil {
				slog.Warn("workflow attach: failed to deliver task to inbox",
					"conv", convID, "node", nodeID, "error", err)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "node_id": nodeID, "status": db.WorkflowNodeStatusRunning, "assignee": convID,
	})
}

// dashboardWorkflowNodeApprove is the human-verify gate for a node whose
// verify.kind is human. approve settles the node done on the success outcome and
// advances the graph through the SAME helpers the manual settle uses
// (workflow.Advance + applyWorkflowAdvance — no duplicated frontier logic); a
// human-verified node's success continuation is its unlabeled edge, which
// OutcomePass takes. reject records the rejection (with optional note) and does
// NOT advance — the node stays as-is so it can be re-worked and re-approved.
func dashboardWorkflowNodeApprove(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, nodeID string) {
	unlock := lockWorkflowInstance(inst.ID)
	defer unlock()

	var body struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	decision := strings.TrimSpace(body.Decision)
	if decision != "approve" && decision != "reject" {
		http.Error(w, "decision must be \"approve\" or \"reject\"", http.StatusBadRequest)
		return
	}

	if fresh, _ := db.GetWorkflowInstance(inst.ID); fresh != nil {
		inst = fresh
	}
	node, err := db.GetWorkflowNode(inst.ID, nodeID)
	if err != nil {
		http.Error(w, "lookup node: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if node == nil {
		http.Error(w, "node "+nodeID+" not found", http.StatusNotFound)
		return
	}

	tmpl, _ := rebuildInstanceTemplate(inst)
	if tmpl == nil || tmpl.Nodes[nodeID] == nil || tmpl.Nodes[nodeID].Verify.Kind != workflow.VerifyHuman {
		http.Error(w, "node "+nodeID+" is not human-verified; the approve gate only applies to verify.kind: human",
			http.StatusBadRequest)
		return
	}
	if isSettledWorkflowNodeStatus(node.Status) {
		http.Error(w, "node "+nodeID+" is already "+node.Status+"; cannot approve/reject a settled node",
			http.StatusConflict)
		return
	}
	if node.Status != db.WorkflowNodeStatusRunning && node.Status != db.WorkflowNodeStatusAwaitingVerify {
		http.Error(w, "node "+nodeID+" is "+node.Status+
			"; it must be running (or awaiting_verify) before approval — start it first", http.StatusConflict)
		return
	}

	now := time.Now()
	note := strings.TrimSpace(body.Note)

	if decision == "reject" {
		msg := "rejected"
		if note != "" {
			msg += ": " + note
		}
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
			InstanceID: inst.ID, NodeID: nodeID, Kind: db.WorkflowEventNodeRejected, Message: msg,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "node_id": nodeID, "decision": "reject", "status": node.Status,
		})
		return
	}

	// approve → settle done on the success path, then advance.
	st := db.WorkflowNodeStatusDone
	out := workflow.OutcomePass
	fin := now
	patch := db.WorkflowNodePatch{Status: &st, Outcome: &out, FinishedAt: &fin}
	if node.StartedAt.IsZero() {
		patch.StartedAt = &now
	}
	if _, err := db.UpdateWorkflowNode(inst.ID, nodeID, patch); err != nil {
		http.Error(w, "update node: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msg := "approved"
	if note != "" {
		msg += ": " + note
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{
		InstanceID: inst.ID, NodeID: nodeID, Kind: db.WorkflowEventNodeApproved, Message: msg,
	})
	appendNodeStatusEvent(inst.ID, nodeID, db.WorkflowNodeStatusDone) // node_done

	instanceRunning := inst.Status == db.WorkflowStatusRunning
	advanced := workflow.AdvanceResult{Ready: []string{}, Skipped: []string{}}
	newInstStatus := inst.Status
	if instanceRunning {
		advanced = workflow.Advance(tmpl, nodeID, workflow.OutcomePass, nodeStateMap(inst.ID))
		applyWorkflowAdvance(inst.ID, nodeID, tmpl, advanced, now)
		nodes, _ := db.ListWorkflowNodes(inst.ID)
		newInstStatus = recomputeWorkflowInstanceStatus(tmpl, nodes)
		if newInstStatus != inst.Status {
			if _, err := db.UpdateWorkflowInstanceStatus(inst.ID, newInstStatus); err == nil {
				appendInstanceStatusEvent(inst.ID, newInstStatus)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "node_id": nodeID, "decision": "approve",
		"status": db.WorkflowNodeStatusDone, "instance_status": newInstStatus,
		"ready": advanced.Ready, "skipped": advanced.Skipped,
	})
}

// convIsMember reports whether convID is among the group's members.
func convIsMember(members []*db.AgentGroupMember, convID string) bool {
	for _, m := range members {
		if m.ConvID == convID {
			return true
		}
	}
	return false
}

// dashboardWorkflowCancel marks every non-terminal node skipped and the
// instance cancelled — a clean terminal snapshot for the dashboard.
func dashboardWorkflowCancel(w http.ResponseWriter, inst *db.WorkflowInstance) {
	if fail := cancelWorkflowInstance(inst.ID); fail != nil {
		http.Error(w, fail.Msg, fail.Status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_status": db.WorkflowStatusCancelled})
}

// cancelWorkflowInstance is the shared cancel core: under the per-instance lock,
// skip every non-terminal node and mark the instance cancelled. Shared by the
// dashboard POST and the /v1 socket twin. Returns nil on success.
func cancelWorkflowInstance(instanceID int64) *spawnFailure {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()
	nodes, _ := db.ListWorkflowNodes(instanceID)
	now := time.Now()
	for _, n := range nodes {
		if isSettledWorkflowNodeStatus(n.Status) {
			continue
		}
		st := db.WorkflowNodeStatusSkipped
		fin := now
		_, _ = db.UpdateWorkflowNode(instanceID, n.NodeID, db.WorkflowNodePatch{Status: &st, FinishedAt: &fin})
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: n.NodeID, Kind: db.WorkflowEventNodeSkipped})
	}
	if _, err := db.UpdateWorkflowInstanceStatus(instanceID, db.WorkflowStatusCancelled); err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "cancel: " + err.Error()}
	}
	appendInstanceStatusEvent(instanceID, db.WorkflowStatusCancelled)
	return nil
}

// ----- snapshot integration -------------------------------------------

// dashboardWorkflowInstance is the snapshot row for one running/finished
// instance — enough for the Workflows tab to render a status card with node
// progress without a per-row detail fetch.
type dashboardWorkflowInstance struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	TemplateRef  string `json:"template_ref"`
	TemplateName string `json:"template_name"`
	Status       string `json:"status"`
	GroupID      int64  `json:"group_id"`
	GroupName    string `json:"group_name,omitempty"`
	Total        int    `json:"total"`
	Done         int    `json:"done"`
	Failed       int    `json:"failed"`
	Running      int    `json:"running"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

// dashboardWorkflowTemplate is the snapshot row for one discoverable template —
// the "what can I instantiate" list. Err is set when the template failed to
// load, so the dashboard can surface a broken template instead of hiding it.
type dashboardWorkflowTemplate struct {
	Ref         string   `json:"ref"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	NodeCount   int      `json:"node_count"`
	Source      string   `json:"source"`
	Err         string   `json:"err,omitempty"`
	Warnings    []string `json:"warnings,omitempty"` // Step 2b topology smells; Step 5 renders a banner
}

// collectWorkflowsSnapshot builds the Workflows tab rows. Empty (non-nil) slice
// on error so the page's .map() is safe.
func collectWorkflowsSnapshot() []dashboardWorkflowInstance {
	out := []dashboardWorkflowInstance{}
	insts, err := db.ListWorkflowInstances()
	if err != nil {
		return out
	}
	groupNames := map[int64]string{}
	for _, inst := range insts {
		row := dashboardWorkflowInstance{
			ID:           inst.ID,
			Title:        inst.Title,
			TemplateRef:  inst.TemplateRef,
			TemplateName: inst.TemplateName,
			Status:       inst.Status,
			GroupID:      inst.GroupID,
		}
		nodes, _ := db.ListWorkflowNodes(inst.ID)
		row.Total = len(nodes)
		for _, n := range nodes {
			switch n.Status {
			case db.WorkflowNodeStatusDone:
				row.Done++
			case db.WorkflowNodeStatusFailed:
				row.Failed++
			case db.WorkflowNodeStatusRunning:
				row.Running++
			}
		}
		if inst.GroupID > 0 {
			name, ok := groupNames[inst.GroupID]
			if !ok {
				if g, gerr := db.GetAgentGroupByID(inst.GroupID); gerr == nil && g != nil {
					name = g.Name
				}
				groupNames[inst.GroupID] = name
			}
			row.GroupName = name
		}
		if !inst.CreatedAt.IsZero() {
			row.CreatedAt = inst.CreatedAt.Format(time.RFC3339)
		}
		if !inst.UpdatedAt.IsZero() {
			row.UpdatedAt = inst.UpdatedAt.Format(time.RFC3339)
		}
		if !inst.CompletedAt.IsZero() {
			row.CompletedAt = inst.CompletedAt.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// collectWorkflowTemplatesSnapshot lists every discoverable template (project /
// user / example). Empty (non-nil) slice when none are found.
func collectWorkflowTemplatesSnapshot() []dashboardWorkflowTemplate {
	out := []dashboardWorkflowTemplate{}
	for _, e := range workflow.List(workflowProjectDirsFn()...) {
		out = append(out, dashboardWorkflowTemplate{
			Ref:         e.Ref,
			Name:        e.Name,
			Description: e.Description,
			NodeCount:   e.NodeCount,
			Source:      string(e.Source),
			Err:         e.Err,
			Warnings:    e.Warnings,
		})
	}
	return out
}

// ----- wire shapes + helpers ------------------------------------------

type workflowNodeJSON struct {
	NodeID          string   `json:"node_id"`
	Label           string   `json:"label"`
	ExecutorKind    string   `json:"executor_kind"`
	Agent           string   `json:"agent,omitempty"`       // executor.Agent: intended ai agent/role hint (Step 5 overlays vitals on this)
	VerifyKind      string   `json:"verify_kind,omitempty"` // verify.Kind: drives the dashboard's approve-gate affordance
	Status          string   `json:"status"`
	Outcome         string   `json:"outcome,omitempty"`
	Assignee        string   `json:"assignee,omitempty"`
	Visits          int64    `json:"visits"`
	Output          string   `json:"output,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	FinishedAt      string   `json:"finished_at,omitempty"`
	AllowedOutcomes []string `json:"allowed_outcomes,omitempty"`
}

type workflowEventJSON struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	At      string `json:"at"`
}

func nodeToJSON(n *db.WorkflowNode, tmpl *workflow.Template) workflowNodeJSON {
	v := workflowNodeJSON{
		NodeID:       n.NodeID,
		Label:        n.Label,
		ExecutorKind: n.ExecutorKind,
		Status:       n.Status,
		Outcome:      n.Outcome,
		Assignee:     n.Assignee,
		Visits:       n.Visits,
		Output:       n.Output,
	}
	if !n.StartedAt.IsZero() {
		v.StartedAt = n.StartedAt.Format(time.RFC3339)
	}
	if !n.FinishedAt.IsZero() {
		v.FinishedAt = n.FinishedAt.Format(time.RFC3339)
	}
	if tmpl != nil {
		v.AllowedOutcomes = tmpl.AllowedOutcomes(n.NodeID)
		if def := tmpl.Nodes[n.NodeID]; def != nil {
			v.Agent = def.Executor.Agent
			v.VerifyKind = string(def.Verify.Kind)
		}
	}
	return v
}

func eventToJSON(e *db.WorkflowEvent) workflowEventJSON {
	v := workflowEventJSON{
		ID:      e.ID,
		NodeID:  e.NodeID,
		Kind:    e.Kind,
		Message: e.Message,
	}
	if !e.At.IsZero() {
		v.At = e.At.Format(time.RFC3339)
	}
	return v
}

func instanceDetailJSON(inst *db.WorkflowInstance) map[string]any {
	m := map[string]any{
		"id":            inst.ID,
		"title":         inst.Title,
		"template_ref":  inst.TemplateRef,
		"template_name": inst.TemplateName,
		"status":        inst.Status,
		"group_id":      inst.GroupID,
	}
	// Resolve the bound group's name so detail matches the `where` instance
	// view (and the CLI `status` can print a group line). omitempty on the
	// consumer side — absent/0 group leaves it off.
	if inst.GroupID > 0 {
		if g, err := db.GetAgentGroupByID(inst.GroupID); err == nil && g != nil {
			m["group_name"] = g.Name
		}
	}
	if !inst.CreatedAt.IsZero() {
		m["created_at"] = inst.CreatedAt.Format(time.RFC3339)
	}
	if !inst.UpdatedAt.IsZero() {
		m["updated_at"] = inst.UpdatedAt.Format(time.RFC3339)
	}
	if !inst.CompletedAt.IsZero() {
		m["completed_at"] = inst.CompletedAt.Format(time.RFC3339)
	}
	return m
}

// rebuildInstanceTemplate reconstructs the topology-relevant template from the
// instance snapshot: the stored mermaid plus each node's detail JSON. Used by
// the advance path and the detail view; returns an error only if the
// snapshotted mermaid no longer parses (it always should — it parsed at
// instantiation).
func rebuildInstanceTemplate(inst *db.WorkflowInstance) (*workflow.Template, error) {
	nodes, err := db.ListWorkflowNodes(inst.ID)
	if err != nil {
		return nil, err
	}
	defs := map[string]*workflow.Node{}
	for _, n := range nodes {
		var def workflow.Node
		if n.Detail != "" && n.Detail != "{}" {
			if jerr := json.Unmarshal([]byte(n.Detail), &def); jerr != nil {
				continue // a corrupt detail blob just loses that node's join/verify hints
			}
		}
		def.ID = n.NodeID
		defs[n.NodeID] = &def
	}
	return workflow.RebuildFromSnapshot(inst.Mermaid, defs)
}

// nodeStateMap reads the current node statuses and maps them onto the coarse
// run-states workflow.Advance reasons over.
func nodeStateMap(instanceID int64) map[string]workflow.NodeRunState {
	nodes, _ := db.ListWorkflowNodes(instanceID)
	m := make(map[string]workflow.NodeRunState, len(nodes))
	for _, n := range nodes {
		m[n.NodeID] = workflowNodeRunState(n.Status)
	}
	return m
}

func workflowNodeRunState(status string) workflow.NodeRunState {
	switch status {
	case db.WorkflowNodeStatusReady, db.WorkflowNodeStatusRunning, db.WorkflowNodeStatusAwaitingVerify:
		return workflow.NodeLive
	case db.WorkflowNodeStatusDone, db.WorkflowNodeStatusFailed, db.WorkflowNodeStatusSkipped:
		return workflow.NodeSettled
	default:
		return workflow.NodePending
	}
}

// recomputeWorkflowInstanceStatus derives the instance status from its nodes: a
// halting failure → failed; otherwise every node terminal → completed; else
// still running. A cancelled instance is set explicitly elsewhere and never
// recomputed here (its nodes are all skipped, which would read as "completed").
func recomputeWorkflowInstanceStatus(tmpl *workflow.Template, nodes []*db.WorkflowNode) string {
	halted := false
	allTerminal := true
	for _, n := range nodes {
		switch n.Status {
		case db.WorkflowNodeStatusDone, db.WorkflowNodeStatusSkipped:
			// terminal-ok
		case db.WorkflowNodeStatusFailed:
			if tmpl == nil || tmpl.FailHalts(n.NodeID) {
				halted = true
			}
			// a continue-fail node settled and the fail path was taken → terminal-ok
		default: // pending / ready / running / awaiting_verify
			allTerminal = false
		}
	}
	switch {
	case halted:
		return db.WorkflowStatusFailed
	case allTerminal:
		return db.WorkflowStatusCompleted
	default:
		return db.WorkflowStatusRunning
	}
}

func appendNodeStatusEvent(instanceID int64, nodeID, status string) {
	kind := ""
	switch status {
	case db.WorkflowNodeStatusRunning:
		kind = db.WorkflowEventNodeStarted
	case db.WorkflowNodeStatusDone:
		kind = db.WorkflowEventNodeDone
	case db.WorkflowNodeStatusFailed:
		kind = db.WorkflowEventNodeFailed
	case db.WorkflowNodeStatusSkipped:
		kind = db.WorkflowEventNodeSkipped
	case db.WorkflowNodeStatusReady:
		kind = db.WorkflowEventNodeReady
	default:
		return // pending / awaiting_verify carry no well-known event kind
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: nodeID, Kind: kind})
}

func appendInstanceStatusEvent(instanceID int64, status string) {
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, Kind: "instance_" + status})
}

func validWorkflowNodeStatus(s string) bool {
	switch s {
	case db.WorkflowNodeStatusPending, db.WorkflowNodeStatusReady, db.WorkflowNodeStatusRunning,
		db.WorkflowNodeStatusAwaitingVerify, db.WorkflowNodeStatusDone,
		db.WorkflowNodeStatusFailed, db.WorkflowNodeStatusSkipped:
		return true
	default:
		return false
	}
}

func isSettledWorkflowNodeStatus(s string) bool {
	switch s {
	case db.WorkflowNodeStatusDone, db.WorkflowNodeStatusFailed, db.WorkflowNodeStatusSkipped:
		return true
	default:
		return false
	}
}

func isEnumNode(tmpl *workflow.Template, nodeID string) bool {
	n := tmpl.Nodes[nodeID]
	return n != nil && n.Verify.Kind == workflow.VerifyEnum
}

func sortedTemplateNodeIDs(tmpl *workflow.Template) []string {
	ids := make([]string, 0, len(tmpl.Nodes))
	for id := range tmpl.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func rawJSONOrEmpty(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
