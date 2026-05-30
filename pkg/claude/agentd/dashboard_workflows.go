package agentd

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
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
// cancel and delete. Spawning/attaching the per-node AI agent is Step 4 and is
// stubbed here with 501. The execution engine that auto-drives nodes is Step 6;
// the manual PATCH path and that engine share workflow.Advance.

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
	var body struct {
		TemplateRef string          `json:"template_ref"`
		Title       string          `json:"title"`
		Params      json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	ref := strings.TrimSpace(body.TemplateRef)
	if ref == "" {
		http.Error(w, "template_ref is required", http.StatusBadRequest)
		return
	}
	tmpl, err := workflow.Resolve(ref, workflowProjectDirsFn()...)
	if err != nil {
		http.Error(w, "resolve template: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Params: an opaque JSON object. We only validate that every required
	// param is present — interpolation into prompts/commands is the engine's
	// job (Step 6). Store a canonical re-marshal so the column is clean JSON.
	params := map[string]any{}
	if len(body.Params) > 0 {
		if err := json.Unmarshal(body.Params, &params); err != nil {
			http.Error(w, "params must be a JSON object: "+err.Error(), http.StatusBadRequest)
			return
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
		http.Error(w, "missing required params: "+strings.Join(missing, ", "), http.StatusBadRequest)
		return
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
	}
	id, err := db.InsertWorkflowInstance(inst)
	if err != nil {
		http.Error(w, "create instance: "+err.Error(), http.StatusInternalServerError)
		return
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
		http.Error(w, "create nodes: "+err.Error(), http.StatusInternalServerError)
		return
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

	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleDashboardWorkflowsAPI dispatches the /api/workflows/{id}... surface:
//
//	GET    /api/workflows/{id}                         → full detail
//	DELETE /api/workflows/{id}                         → delete (cascades nodes+events)
//	POST   /api/workflows/{id}/cancel                  → cancel
//	PATCH  /api/workflows/{id}/nodes/{nodeId}          → manual node update + advance
//	GET    /api/workflows/{id}/nodes/{nodeId}/audit    → that node's event timeline
//	POST   /api/workflows/{id}/nodes/{nodeId}/start    → 501 (Step 4)
//	POST   /api/workflows/{id}/nodes/{nodeId}/attach   → 501 (Step 4)
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
			if err := db.DeleteWorkflowInstance(id); err != nil {
				http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"instance": instanceDetailJSON(inst),
		"mermaid":  inst.Mermaid,
		"params":   rawJSONOrEmpty(inst.Params),
		"vars":     rawJSONOrEmpty(inst.Vars),
		"nodes":    nodeViews,
		"events":   eventViews,
	})
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
	case "start", "attach":
		// Step 4 (group integration) owns spawning/attaching the node's agent.
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"node "+sub+" lands in Step 4 (workflow group integration)")
	default:
		http.Error(w, "unknown node subpath "+sub, http.StatusNotFound)
	}
}

// dashboardWorkflowNodePatch applies a manual node update — the MVP driving
// path. A status of done/failed is a settle: it runs workflow.Advance to ready
// the taken successors and skip the abandoned branches, then recomputes the
// instance status. Events are appended for every transition.
func dashboardWorkflowNodePatch(w http.ResponseWriter, r *http.Request, inst *db.WorkflowInstance, node *db.WorkflowNode) {
	var body struct {
		Status   *string `json:"status"`
		Outcome  *string `json:"outcome"`
		Output   *string `json:"output"`
		Assignee *string `json:"assignee"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Re-settling an already-terminal node is rejected: it would append a
	// duplicate node_done/failed event and re-run Advance over now-stale state
	// (successors already moved past pending), so the second advance is a
	// no-op at best and a wrong skip at worst. A human who needs to redo a node
	// goes through the (future) re-open path, not a second settle. A patch that
	// only touches output/assignee on a settled node is still allowed.
	if isSettledWorkflowNodeStatus(node.Status) && body.Status != nil &&
		isSettledWorkflowNodeStatus(strings.TrimSpace(*body.Status)) {
		http.Error(w, "node "+node.NodeID+" is already "+node.Status+"; cannot re-settle", http.StatusConflict)
		return
	}

	tmpl, _ := rebuildInstanceTemplate(inst) // nil only on an unparsable snapshot

	now := time.Now()
	patch := db.WorkflowNodePatch{}
	newStatus := node.Status
	if body.Status != nil {
		s := strings.TrimSpace(*body.Status)
		if !validWorkflowNodeStatus(s) {
			http.Error(w, "invalid node status "+strconv.Quote(s), http.StatusBadRequest)
			return
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
				if tmpl != nil && isEnumNode(tmpl, node.NodeID) {
					http.Error(w, "node "+node.NodeID+" is enum-verified; an outcome is required", http.StatusBadRequest)
					return
				}
				settleOutcome = workflow.OutcomePass
			}
			if tmpl != nil && !slices.Contains(tmpl.AllowedOutcomes(node.NodeID), settleOutcome) {
				http.Error(w, "outcome "+strconv.Quote(settleOutcome)+" is not valid for node "+node.NodeID+
					" (allowed: "+strings.Join(tmpl.AllowedOutcomes(node.NodeID), ", ")+")", http.StatusBadRequest)
				return
			}
		}
		patch.Outcome = &settleOutcome
	} else if body.Outcome != nil {
		o := strings.TrimSpace(*body.Outcome)
		patch.Outcome = &o
	}

	if _, err := db.UpdateWorkflowNode(inst.ID, node.NodeID, patch); err != nil {
		http.Error(w, "update node: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if body.Status != nil {
		appendNodeStatusEvent(inst.ID, node.NodeID, newStatus)
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
		advanced = workflow.Advance(tmpl, node.NodeID, settleOutcome, nodeStateMap(inst.ID))
		applyWorkflowAdvance(inst.ID, advanced, now)
	}

	newInstStatus := inst.Status
	if instanceRunning {
		nodes, _ := db.ListWorkflowNodes(inst.ID)
		newInstStatus = recomputeWorkflowInstanceStatus(tmpl, nodes)
		if newInstStatus != inst.Status {
			if _, err := db.UpdateWorkflowInstanceStatus(inst.ID, newInstStatus); err == nil {
				appendInstanceStatusEvent(inst.ID, newInstStatus)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"node_id":         node.NodeID,
		"status":          newStatus,
		"instance_status": newInstStatus,
		"ready":           advanced.Ready,
		"skipped":         advanced.Skipped,
	})
}

// applyWorkflowAdvance writes the proposed transitions: pending → ready for each
// taken successor, pending → skipped for each abandoned branch. Only nodes still
// pending are touched, so a re-run (or a node a human already moved) is a no-op.
func applyWorkflowAdvance(instanceID int64, res workflow.AdvanceResult, now time.Time) {
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
}

// dashboardWorkflowCancel marks every non-terminal node skipped and the
// instance cancelled — a clean terminal snapshot for the dashboard.
func dashboardWorkflowCancel(w http.ResponseWriter, inst *db.WorkflowInstance) {
	nodes, _ := db.ListWorkflowNodes(inst.ID)
	now := time.Now()
	for _, n := range nodes {
		if isSettledWorkflowNodeStatus(n.Status) {
			continue
		}
		st := db.WorkflowNodeStatusSkipped
		fin := now
		_, _ = db.UpdateWorkflowNode(inst.ID, n.NodeID, db.WorkflowNodePatch{Status: &st, FinishedAt: &fin})
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: n.NodeID, Kind: db.WorkflowEventNodeSkipped})
	}
	if _, err := db.UpdateWorkflowInstanceStatus(inst.ID, db.WorkflowStatusCancelled); err != nil {
		http.Error(w, "cancel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	appendInstanceStatusEvent(inst.ID, db.WorkflowStatusCancelled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_status": db.WorkflowStatusCancelled})
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
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	NodeCount   int    `json:"node_count"`
	Source      string `json:"source"`
	Err         string `json:"err,omitempty"`
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
		})
	}
	return out
}

// ----- wire shapes + helpers ------------------------------------------

type workflowNodeJSON struct {
	NodeID          string   `json:"node_id"`
	Label           string   `json:"label"`
	ExecutorKind    string   `json:"executor_kind"`
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
