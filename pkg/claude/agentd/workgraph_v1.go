package agentd

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/workgraph"
)

// workgraph_v1.go is the peer-cred /v1/workgraphs* socket surface — the CLI-facing
// twin of the cookie-gated dashboard handlers (dashboard_workgraphs.go). The CLI
// (`tclaude workgraph …`, JOH-13) and agents reach these over the Unix socket
// with peer-credential identity (authedCaller); the browser dashboard uses the
// cookie surface. Both share the same cores (createWorkgraphInstance,
// workgraphDetailJSON, applyWorkgraphNodePatch, cancelWorkgraphInstance,
// collectWorkgraphsSnapshot) so there is one implementation of the behaviour and
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

func registerWorkgraphV1Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/workgraphs", handleV1Workgraphs)
	mux.HandleFunc("/v1/workgraphs/", handleV1WorkgraphsByID)
}

// handleV1Workgraphs dispatches the collection: GET (list) and POST (create).
// /v1/workgraphs/where is a sibling literal path handled here too, since it does
// not carry an {id}.
func handleV1Workgraphs(w http.ResponseWriter, r *http.Request) {
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"instances": collectWorkgraphsSnapshot()})
	case http.MethodPost:
		// create — human/owner. An agent with no group it could own the binding
		// of still can't instantiate (matches the dashboard's human-consent gate).
		var body workgraphCreateBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "bad JSON: "+err.Error())
			return
		}
		if !isHuman && !callerOwnsCreateGroup(caller, body.Group) {
			writeError(w, http.StatusForbidden, "forbidden",
				"only the human operator or an owner of the bound group may instantiate a workgraph")
			return
		}
		res, fail := createWorkgraphInstance(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleV1WorkgraphsByID dispatches everything under /v1/workgraphs/{id}, plus the
// literal /v1/workgraphs/where path (which has no {id}).
func handleV1WorkgraphsByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/workgraphs/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "not_found", "expected /v1/workgraphs/{id} or /v1/workgraphs/where")
		return
	}
	// /v1/workgraphs/where — the reflection endpoint (no {id}).
	if rest == "where" {
		handleV1WorkgraphWhere(w, r)
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
	inst, err := db.GetWorkgraphInstance(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "not_found", "workgraph "+strconv.FormatInt(id, 10)+" not found")
		return
	}

	switch {
	case len(parts) == 1: // /v1/workgraphs/{id}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, workgraphDetailJSON(inst))
		case http.MethodDelete:
			if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
				writeError(w, http.StatusForbidden, "forbidden", "only the human or the bound-group owner may delete a workgraph")
				return
			}
			// Serialise the delete against any in-flight drive on this instance
			// (the engine's spawn/settle, a node-PATCH's advance) under the SAME
			// per-instance lock the mutating handlers take — exactly as the
			// dashboard DELETE twin does. Without it a delete could wipe the rows
			// mid read-modify-write, and dropping the map entry while another
			// goroutine still holds the mutex would let a fresh caller mint a
			// SECOND mutex for the same id, breaking the mutual exclusion.
			unlock := lockWorkgraphInstance(id)
			delErr := db.DeleteWorkgraphInstance(id)
			if delErr == nil {
				workgraphInstanceLocks.Delete(id) // row is gone; drop the now-unreachable mutex
			}
			unlock()
			if delErr != nil {
				writeError(w, http.StatusInternalServerError, "io", "delete: "+delErr.Error())
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method", "GET or DELETE on /v1/workgraphs/{id}")
		}
	case parts[1] == "events" && len(parts) == 2:
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
			return
		}
		handleV1WorkgraphEvents(w, r, id)
	case parts[1] == "cancel" && len(parts) == 2:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
			return
		}
		if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
			writeError(w, http.StatusForbidden, "forbidden", "only the human or the bound-group owner may cancel a workgraph")
			return
		}
		if fail := cancelWorkgraphInstance(id); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_status": db.WorkgraphStatusCancelled})
	case parts[1] == "drive" && len(parts) == 2:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST only on /v1/workgraphs/{id}/drive")
			return
		}
		handleV1WorkgraphDrive(w, inst, caller, isHuman)
	case parts[1] == "nodes" && len(parts) == 3 && parts[2] != "":
		// parts[2] is "{nodeId}" (PATCH) or "{nodeId}/start" (the driver's
		// spawn-into-node verb). Node ids are mermaid ids (no slashes), so the
		// first "/" cleanly separates the node from its sub-action.
		nodeID, sub, _ := strings.Cut(parts[2], "/")
		if nodeID == "" {
			writeError(w, http.StatusNotFound, "not_found", "expected /v1/workgraphs/{id}/nodes/{nodeId}")
			return
		}
		switch sub {
		case "":
			handleV1WorkgraphNodePatch(w, r, inst, caller, isHuman, nodeID)
		case "start":
			handleV1WorkgraphNodeStart(w, r, inst, caller, isHuman, nodeID)
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown node subpath "+strconv.Quote(sub))
		}
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown path under /v1/workgraphs/"+strconv.FormatInt(id, 10))
	}
}

// handleV1WorkgraphEvents serves the per-instance (or per-node) timeline.
func handleV1WorkgraphEvents(w http.ResponseWriter, r *http.Request, instanceID int64) {
	var events []*db.WorkgraphEvent
	var err error
	if nodeID := strings.TrimSpace(r.URL.Query().Get("node")); nodeID != "" {
		events, err = db.ListWorkgraphEvents(instanceID, nodeID)
	} else {
		events, err = db.ListWorkgraphEvents(instanceID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list events: "+err.Error())
		return
	}
	out := make([]workgraphEventJSON, 0, len(events))
	for _, e := range events {
		out = append(out, eventToJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// handleV1WorkgraphNodePatch is the agent/CLI completion path: it authorises the
// caller against the node (assignee-or-human/owner), then applies the shared
// node-PATCH core. The CLI's `tclaude workgraph node <inst> <node> {start|done|
// fail}` wraps this with body {status: running|done|failed, outcome?, output?}.
func handleV1WorkgraphNodePatch(w http.ResponseWriter, r *http.Request, inst *db.WorkgraphInstance, caller string, isHuman bool, nodeID string) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method", "PATCH only on /v1/workgraphs/{id}/nodes/{nodeId}")
		return
	}
	node, err := db.GetWorkgraphNode(inst.ID, nodeID)
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
	var body workgraphNodePatchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "bad JSON: "+err.Error())
		return
	}
	res, fail := applyWorkgraphNodePatch(inst.ID, nodeID, body)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleV1WorkgraphNodeStart is the agent-engine driver's spawn-worker-into-node
// verb (JOH-15 B1): POST /v1/workgraphs/{id}/nodes/{nodeId}/start spawns a fresh
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
func handleV1WorkgraphNodeStart(w http.ResponseWriter, r *http.Request, inst *db.WorkgraphInstance, caller string, isHuman bool, nodeID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only on /v1/workgraphs/{id}/nodes/{nodeId}/start")
		return
	}
	if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
		writeError(w, http.StatusForbidden, "forbidden",
			"only the human or the bound-group owner may spawn a worker into node "+nodeID)
		return
	}
	// Optional driver seed context (JOH-15 B2a `--context`): the agent-engine driver
	// folds the upstream AI outputs it routed in into the spawned worker's brief
	// (additive to the worker's own `workgraph where`/`status` self-view). The body is
	// optional — an empty/absent body seeds nothing, matching the dashboard start.
	var body struct {
		Context string `json:"context"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "bad JSON: "+err.Error())
			return
		}
	}
	res, fail := spawnWorkerIntoNodeCore(inst.ID, nodeID, body.Context)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleV1WorkgraphDrive is the agent-engine anchoring verb (JOH-15 B2a): POST
// /v1/workgraphs/{id}/drive spawns a fresh driver agent into the instance's bound
// group, grants it group-OWNERSHIP (the F2 drive authority — the same authority
// that settles any node + spawns workers via /v1), and briefs it to run the
// `workgraph-engine` skill against this instance. On-demand, not at create (Q3): a
// driver is anchored only when a human/owner asks, and a dead driver falls out of
// the always-on JOH-41 stuck sweep for free.
//
// Schema-free (no driver_conv column): nothing structurally prevents anchoring a
// SECOND driver (a one-driver-per-instance v1 usage contract, not a hard guard —
// the shared node-PATCH guards bound the damage). We cheaply warn when the bound
// group already has a live agent-owner that might already be driving; the hard
// guard waits for the driver_conv designation (a later slice).
//
// Authorised for the human or the instance's bound-group owner — the same gate as
// spawn-into-node and cancel (zero new authz surface).
func handleV1WorkgraphDrive(w http.ResponseWriter, inst *db.WorkgraphInstance, caller string, isHuman bool) {
	if !isHuman && !callerOwnsInstanceGroup(caller, inst) {
		writeError(w, http.StatusForbidden, "forbidden",
			"only the human or the bound-group owner may anchor a driver for this workgraph")
		return
	}
	if inst.Status != db.WorkgraphStatusRunning {
		writeError(w, http.StatusConflict, "conflict",
			"instance is "+inst.Status+"; only a running instance can be driven")
		return
	}
	// Driving a system-mode instance is a no-op-to-harmful confusion: the daemon
	// already advances it, and a second driver racing the engine would double-spawn.
	// Refuse loudly — `engine: agent` is what hands judgment to a driver.
	if inst.EngineMode != string(workgraph.EngineAgent) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"instance engine is "+strconv.Quote(inst.EngineMode)+"; `workgraph drive` applies only to engine:agent "+
				"instances (the deterministic system engine drives system-mode instances itself)")
		return
	}
	g, fail := boundGroupOrFail(inst)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	cwd, cwdErr := resolveSpawnCwd(g.DefaultCwd)
	if cwdErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "resolve group cwd: "+cwdErr.Error())
		return
	}

	// Cheap existing-driver heuristic (Q3 — warn, don't block): a bound-group owner
	// with a live CC session is a candidate driver. It can't tell a driver from any
	// other owner-agent (the human owner has no running conv-session, so it won't
	// match), so it's advisory, surfaced in the response — not a hard guard.
	var warning string
	if owners, oerr := db.ListAgentGroupOwners(g.ID); oerr == nil {
		live := 0
		for _, o := range owners {
			if o.ConvID != "" && convHasRunningSession(o.ConvID) {
				live++
			}
		}
		if live > 0 {
			warning = "the bound group already has " + strconv.Itoa(live) + " live agent-owner(s); " +
				"if one is already driving this instance, anchoring another violates the one-driver-per-instance v1 contract"
		}
	}

	// The driver's identity = the granter of its ownership. A human caller grants as
	// "human"; an owner-agent caller grants under its own conv-id.
	granter := caller
	if granter == "" {
		granter = "human"
	}

	outcome, sfail := executeSpawn(g, spawnParams{
		Name:           "workgraph-engine",
		Role:           "workgraph-engine",
		Descr:          "engine driver · workgraph " + strconv.FormatInt(inst.ID, 10),
		InitialMessage: buildDriverBrief(inst.ID),
		Cwd:            cwd,
		GroupContext:   g.DefaultContext,
		ReplyToConv:    caller,
		SpawnedByConv:  caller,
	})
	if sfail != nil {
		writeError(w, sfail.Status, sfail.Kind, sfail.Msg)
		return
	}

	// Grant the spawned driver group-ownership — its drive authority (F2). Best is
	// to fail loudly if this can't be recorded: a member-only driver would 403 on
	// every /v1 drive call, so a silent skip would leave a useless agent running.
	if err := db.AddAgentGroupOwner(g.ID, outcome.ConvID, granter); err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"spawned driver "+outcome.ConvID+" but failed to grant it group-ownership: "+err.Error())
		return
	}
	_, _ = db.AppendWorkgraphEvent(&db.WorkgraphEvent{
		InstanceID: inst.ID, Kind: db.WorkgraphEventNodeStarted,
		Message: "anchored engine driver " + outcome.ConvID + " (group owner) for engine:agent instance",
	})

	resp := map[string]any{
		"ok": true, "instance": inst.ID, "driver_conv": outcome.ConvID,
		"group": g.Name, "label": outcome.Label, "tmux_session": outcome.TmuxSession,
		"attach_cmd": "tclaude session attach " + outcome.Label,
	}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildDriverBrief composes the kickoff inbox briefing handed to a freshly-anchored
// engine driver (JOH-15 B2a). It points the driver at the `workgraph-engine` skill
// for the full reflect→decide→spawn/settle→repeat loop, and inlines the essential
// contract so the driver can act even before opening the skill: it owns data
// routing (the daemon does NOT auto-handoff in agent mode), it spawns workers with
// `workgraph spawn --context`, it settles via the node-PATCH gate, and it holds no
// authoritative state so it can reincarnate and resume from `workgraph status`.
func buildDriverBrief(instanceID int64) string {
	id := strconv.FormatInt(instanceID, 10)
	return "You are the ENGINE (driver) for workgraph instance " + id + ", which runs in " +
		"engine:agent mode: the daemon will NOT auto-spawn workers or auto-advance — YOU supply " +
		"the judgment and drive it to a terminal state.\n\n" +
		"Use the `workgraph-engine` skill for the full loop. In short, repeat until the instance " +
		"is completed or failed:\n" +
		"1. Read the whole graph: `tclaude workgraph status " + id + " --json` (node statuses, " +
		"outcomes, captured outputs, vars, events).\n" +
		"2. For each ready ai node, spawn a worker and seed it the upstream outputs it needs — YOU " +
		"own data routing (the daemon does not auto-handoff in agent mode):\n" +
		"   `tclaude workgraph spawn " + id + " <node> --context \"<concise upstream summary>\"`.\n" +
		"3. When a worker reports done (watch your inbox), settle + advance via the node gate:\n" +
		"   `tclaude workgraph node " + id + " <node> done --outcome <outcome>` (or `fail`).\n" +
		"4. The daemon still runs mechanical tool/program nodes and enforces guards (max_visits, " +
		"approval gates) — you cannot bypass them; let them work.\n" +
		"5. Re-read status each loop and hold no authoritative state, so you can reincarnate and " +
		"resume mid-flight.\n\n" +
		"You are a group OWNER of this instance's bound group — that ownership is your authority " +
		"for every drive call above. One driver per instance: do not anchor a second."
}

// handleV1WorkgraphWhere is the first-person reflection endpoint: "which
// workgraph/node am I (the caller) assigned to?". An agent caller sees only its
// own assignments (matched reincarnation-correctly via ResolveLatestConv); a
// human caller has no conv-id and gets an empty list (humans use list/detail for
// the global view). Optional ?instance=<id> scopes to one instance.
func handleV1WorkgraphWhere(w http.ResponseWriter, r *http.Request) {
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
		var nodes []*db.WorkgraphNode
		if instStr := strings.TrimSpace(r.URL.Query().Get("instance")); instStr != "" {
			id, perr := strconv.ParseInt(instStr, 10, 64)
			if perr != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", "instance must be an integer")
				return
			}
			inst, ierr := db.GetWorkgraphInstance(id)
			if ierr != nil {
				writeError(w, http.StatusInternalServerError, "io", "lookup: "+ierr.Error())
				return
			}
			if inst == nil {
				writeError(w, http.StatusNotFound, "not_found", "workgraph "+instStr+" not found")
				return
			}
			nodes, _ = db.ListWorkgraphNodes(id)
		} else {
			nodes, _ = db.ListAssignedWorkgraphNodes()
		}

		// Cache rebuilt templates + instance lookups per instance across the
		// (already instance-ordered) node list.
		instCache := map[int64]*db.WorkgraphInstance{}
		for _, n := range nodes {
			if n.Assignee == "" || db.ResolveLatestConv(n.Assignee) != caller {
				continue
			}
			inst, oki := instCache[n.InstanceID]
			if !oki {
				inst, _ = db.GetWorkgraphInstance(n.InstanceID)
				instCache[n.InstanceID] = inst
			}
			if inst == nil {
				continue
			}
			tmpl, _ := rebuildInstanceTemplate(inst)
			assignments = append(assignments, map[string]any{
				"instance":  instanceDetailJSON(inst),
				"node":      nodeToJSON(n, tmpl),
				"self_view": workgraphNodeSelfView(inst, n, tmpl),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"caller":      caller,
		"assignments": assignments,
	})
}

// workgraphSelfView is the JOH-15 Slice-A self-view: everything an agent embedded
// in a node needs to do its node WITHOUT parsing the mermaid chart or
// re-resolving interpolation by hand. It is computed server-side from the
// instance's live scope (params + captured vars) and the snapshotted template,
// and attached to each `tclaude workgraph where` assignment.
type workgraphSelfView struct {
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
	Successors []workgraphSuccessor `json:"successors"`
}

// workgraphSuccessor is one outcome→target edge in the self-view.
type workgraphSuccessor struct {
	Outcome string `json:"outcome"`  // the outcome that takes this edge ("pass" for an unlabeled/default edge)
	To      string `json:"to"`       // successor node id
	ToLabel string `json:"to_label"` // successor's display label (falls back to its id)
}

// workgraphNodeSelfView builds the self-view for one assigned node. A nil template
// (a corrupt snapshot) degrades to just the raw task, so `where` never fails.
func workgraphNodeSelfView(inst *db.WorkgraphInstance, n *db.WorkgraphNode, tmpl *workgraph.Template) workgraphSelfView {
	sv := workgraphSelfView{MissingRefs: []string{}, AllowedOutcomes: []string{}, Successors: []workgraphSuccessor{}}
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
			outcome = workgraph.OutcomePass // an unlabeled edge is followed on pass
		}
		sv.Successors = append(sv.Successors, workgraphSuccessor{
			Outcome: outcome, To: e.To, ToLabel: tmpl.DisplayLabel(e.To),
		})
	}
	return sv
}

// nodeTaskText returns a node's raw instruction by executor kind: the ai prompt,
// the human instructions, or the tool/program run command.
func nodeTaskText(def *workgraph.Node) string {
	if def == nil {
		return ""
	}
	switch def.Executor.Kind {
	case workgraph.ExecAI:
		return def.Executor.Prompt
	case workgraph.ExecHuman:
		return def.Executor.Instructions
	case workgraph.ExecTool, workgraph.ExecProgram:
		return def.Executor.Run
	}
	return ""
}

// ----- authz helpers --------------------------------------------------

// callerMaySettleNode reports whether a (non-human) agent caller may settle a
// node: it is the node's assignee (resolved forward through succession so a
// reincarnated assignee still matches), OR it owns the instance's bound group.
func callerMaySettleNode(caller string, inst *db.WorkgraphInstance, node *db.WorkgraphNode) bool {
	if caller == "" {
		return false
	}
	if node.Assignee != "" && db.ResolveLatestConv(node.Assignee) == caller {
		return true
	}
	return callerOwnsInstanceGroup(caller, inst)
}

// callerOwnsInstanceGroup reports whether caller owns the instance's bound group.
func callerOwnsInstanceGroup(caller string, inst *db.WorkgraphInstance) bool {
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
