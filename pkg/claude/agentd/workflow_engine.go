package agentd

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/workflow"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

// workflow_engine.go is the autonomous runner (JOH-8 / Step 6): it advances
// running workflow instances without a human clicking, mirroring the cron
// scheduler's tick model (startCronScheduler / runCronTick).
//
// Slice A scope: drive tool/program nodes end to end — interpolate the command,
// run it, capture output into the instance vars, verify (tool/program/enum/
// format/none), settle the node, and advance the graph. ai and human nodes are
// left where they are (ready/awaiting) for the dashboard / Step-4 start path and
// Step-6 slice B; the engine never auto-runs them yet.
//
// Opt-in: the engine is OFF unless config sets agent.workflow_engine. Auto-
// running a template's shell commands is a real trust decision, so a fresh
// daemon never does it implicitly — when disabled the tick is a pure no-op.
//
// NOT yet in scope (deferred): loop RE-ENTRY. workflow.Advance is single-step
// and leaves an already-settled target alone, so a back-edge into a done node
// is a no-op here — retries / max_visits / loop iteration (resetting a node +
// bumping visits) are a later slice. A template with a retry loop won't loop
// yet; it just runs each node once.
//
// Concurrency: a node is CLAIMED (ready→running) under lockWorkflowInstance(id)
// — the SAME per-instance mutex the manual dashboard paths hold — the command
// then runs with NO lock held, and the result is settled under the lock again
// after re-reading fresh. So an engine run never blocks a dashboard cancel/
// delete behind a long command, and a settle that lost the race (cancel landed
// first) is discarded. The pure decision logic (executor dispatch, verification,
// advance) lives in pkg/claude/workflow; this file is the effects layer (DB +
// process exec).

// workflowEngineTickInterval is how often the engine sweeps running instances
// for actionable nodes. Matches the cron scheduler's cadence — fine-grained
// enough for a responsive runner without busy-spinning, and tool nodes complete
// within a tick so there is no latency benefit to going faster.
const workflowEngineTickInterval = 5 * time.Second

// engineAssignee is the sentinel stamped on a node's assignee while the engine
// is running its command, so the startup reaper can tell an engine corpse (a
// node the daemon died on mid-command) apart from a node a HUMAN manually drove
// to running via the dashboard PATCH path. Only nodes carrying this marker are
// reaped; a human-running tool node (empty/human assignee) is left alone. The
// angle brackets keep it out of the conv-id namespace (real assignees are UUIDs
// / handles), matching the "<human-dashboard>" convention elsewhere.
const engineAssignee = "<workflow-engine>"

// workflowNodeRunTimeout bounds a single tool/program node's command. A node
// that needs longer than this is the wrong shape for a synchronous executor
// (it should be a program/ai node the engine observes), and an unbounded
// command would wedge the whole tick. Generous enough for build/test commands.
var workflowNodeRunTimeout = 10 * time.Minute

// workflowEngineEnabled gates the engine loop. OFF by default: auto-executing a
// template's shell commands is an explicit operator trust decision, so a daemon
// never auto-runs workflow nodes until config opts in (agent.workflow_engine).
// When false the tick is a no-op, so a daemon that hasn't enabled the engine
// pays nothing and runs no third-party commands. runServe sets it from config;
// tests flip it via SetWorkflowEngineEnabledForTest.
var workflowEngineEnabled = false

// startWorkflowEngine spins up the engine in its own goroutine, ticking every
// workflowEngineTickInterval until stop closes. Mirrors startCronScheduler:
// a one-time reap of orphaned nodes + an immediate tick on startup (so a daemon
// restart resumes in-flight instances without waiting a full interval), then
// timer-driven.
func startWorkflowEngine(stop <-chan struct{}) {
	go func() {
		reapOrphanedEngineNodes()
		runWorkflowEngineTick(context.Background())
		t := time.NewTicker(workflowEngineTickInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				runWorkflowEngineTick(context.Background())
			}
		}
	}()
}

// reapOrphanedEngineNodes runs once at startup to recover engine-driven
// tool/program nodes left `running` by a daemon that died mid-command. Such a
// node has no live runner — the engine runs tool/program commands synchronously
// in-process, so a `running` one after a restart can only be a corpse — and
// nothing would ever re-pick it (nextRunnableNode only takes `ready` nodes), so
// the instance would hang forever. Resetting them to `ready` makes the engine
// genuinely resumable: the next tick re-runs the command from the top (tool/
// program nodes are meant to be idempotent re-runs).
//
// Only a node carrying the engineAssignee sentinel is reaped — that marker is
// stamped by claimNextNode when THIS engine starts a tool/program command and
// cleared when it settles, so it pinpoints exactly an engine corpse. A
// tool/program node a HUMAN manually drove to running via the dashboard
// (allowed by the PATCH path, with an empty/human assignee) is therefore NOT
// reaped — its manual state is preserved across a restart. ai/human nodes are
// likewise untouched: an ai node may be legitimately running with a live agent,
// a human node is dashboard-driven. Reaping runs regardless of the engine's
// opt-in gate: it only ever unsticks rows the engine itself created.
func reapOrphanedEngineNodes() {
	insts, err := db.ListWorkflowInstances()
	if err != nil {
		slog.Warn("workflow engine: reap — list instances failed", "error", err)
		return
	}
	ready := db.WorkflowNodeStatusReady
	cleared := ""
	for _, inst := range insts {
		if inst.Status != db.WorkflowStatusRunning {
			continue
		}
		nodes, err := db.ListWorkflowNodes(inst.ID)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			if n.Status != db.WorkflowNodeStatusRunning || n.Assignee != engineAssignee {
				continue // not an engine corpse — leave manual/ai/human running nodes alone
			}
			if _, err := db.UpdateWorkflowNode(inst.ID, n.NodeID,
				db.WorkflowNodePatch{Status: &ready, Assignee: &cleared}); err == nil {
				_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: n.NodeID,
					Kind: db.WorkflowEventNodeReady, Message: "engine: reset orphaned running node after restart"})
				slog.Info("workflow engine: reaped orphaned running node", "instance", inst.ID, "node", n.NodeID)
			}
		}
	}
}

// runWorkflowEngineTick is a single sweep: find every running instance and
// process it. Resumability is implicit — the engine holds no in-memory run
// state, deriving everything from the SQLite node statuses each tick, so a
// killed daemon resumes mid-flight on its next startup tick. A node the daemon
// died on mid-command is recovered by reapOrphanedEngineNodes at startup (it is
// reset ready→re-run), so a crash doesn't strand it. One bad instance is logged
// and never aborts the sweep.
func runWorkflowEngineTick(ctx context.Context) {
	if !workflowEngineEnabled {
		return
	}
	insts, err := db.ListWorkflowInstances()
	if err != nil {
		slog.Warn("workflow engine: list instances failed", "error", err)
		return
	}
	for _, inst := range insts {
		if inst.Status != db.WorkflowStatusRunning {
			continue
		}
		// Isolate each instance: a panic processing one (a malformed snapshot,
		// a nil deref in a future executor) must not kill the engine goroutine
		// and freeze every OTHER instance. Recover, log, move on — the next
		// tick retries the instance from its persisted state.
		safeProcessWorkflowInstance(ctx, inst.ID)
	}
}

// processWorkflowInstance advances one instance by one tick's worth of work:
// each ready tool/program node is claimed, run, verified, settled, and the graph
// advanced. It loops so a chain of instantly-completing tool nodes drains within
// one tick rather than one node per interval; bounded by a fuel counter so a
// misconfigured tight loop can't spin the tick forever.
//
// The instance lock is NOT held across the command run. Each step is three
// phases: (1) claim a node ready→running under the lock, then release it;
// (2) run the executor+verifier with NO lock held — so a human cancel/delete on
// this instance stays responsive instead of blocking behind a long command;
// (3) re-acquire the lock, re-read fresh, and settle only if the node is still
// the running one we claimed and the instance is still running (a concurrent
// cancel/delete may have moved on). This is what makes cancellation work and
// keeps the per-instance lock short-held, matching the dashboard handlers.
// safeProcessWorkflowInstance runs processWorkflowInstance with a panic
// recover, so one bad instance can't take down the engine goroutine (which
// would freeze every other instance until the daemon restarts). The recovered
// instance is simply retried on the next tick from its persisted state.
func safeProcessWorkflowInstance(ctx context.Context, instanceID int64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("workflow engine: panic processing instance; skipping this tick",
				"instance", instanceID, "panic", r)
		}
	}()
	processWorkflowInstance(ctx, instanceID)
}

func processWorkflowInstance(ctx context.Context, instanceID int64) {
	const maxStepsPerTick = 100
	for range maxStepsPerTick {
		claim := claimNextNode(instanceID)
		if claim == nil {
			return // nothing actionable this tick
		}
		// Exec + verify phase — NO lock held. Both the executor command and the
		// verification command shell out, so both run lock-free; only the DB
		// writes (capture + settle + advance) happen under the lock in
		// settleClaimedNode. This is what keeps a long `go test` verify from
		// blocking a concurrent dashboard cancel/delete.
		runCtx, cancel := context.WithTimeout(ctx, workflowNodeRunTimeout)
		exec := workflow.RunExecutor(runCtx, claim.def, claim.scope, bashRunner{})
		var verdict workflow.VerifyDisposition
		if exec.Outcome == workflow.ExecRan {
			// Verify inspects the executor's output; it runs only when the
			// executor produced a result (an ExecError skips straight to fail).
			verdict = workflow.RunVerifier(runCtx, claim.def, claim.scope, exec.Output, exec.Success, bashRunner{})
		}
		cancel()
		settled := settleClaimedNode(instanceID, claim, exec, verdict)
		if !settled {
			// Node deferred / errored-without-progress, or was cancelled out from
			// under us — stop draining so we don't spin; next tick revisits.
			return
		}
	}
	slog.Warn("workflow engine: instance hit per-tick step cap; continuing next tick", "instance", instanceID)
}

// nodeClaim is a node the engine has moved ready→running and is about to execute,
// carrying the snapshot it needs to run + settle without re-reading mid-exec.
type nodeClaim struct {
	nodeID string
	def    *workflow.Node
	scope  workflow.Scope
}

// claimNextNode picks the next runnable node under the instance lock, marks it
// running (so a concurrent tick / dashboard sees it as taken), and returns the
// snapshot needed to run it. Returns nil when there is nothing to do. Holds the
// lock only for the claim — released before the command runs.
func claimNextNode(instanceID int64) *nodeClaim {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		return nil
	}
	nodes, err := db.ListWorkflowNodes(instanceID)
	if err != nil {
		slog.Warn("workflow engine: list nodes failed", "instance", instanceID, "error", err)
		return nil
	}
	node := nextRunnableNode(inst, nodes)
	if node == nil {
		return nil
	}
	tmpl, terr := rebuildInstanceTemplate(inst)
	if terr != nil || tmpl == nil || tmpl.Nodes[node.NodeID] == nil {
		slog.Warn("workflow engine: cannot rebuild node def; skipping", "instance", instanceID, "node", node.NodeID, "error", terr)
		return nil
	}
	def := tmpl.Nodes[node.NodeID]

	// Stamp the engine-owner sentinel alongside running, so the startup reaper
	// can recognise a node THIS engine claimed (vs one a human manually drove to
	// running) and only reap its own corpses after a crash. startedAt uses the
	// real wall clock at the claim, not the tick's start, so a drained chain
	// gets truthful per-node timestamps.
	running := db.WorkflowNodeStatusRunning
	startedAt := time.Now()
	owner := engineAssignee
	if _, err := db.UpdateWorkflowNode(instanceID, node.NodeID,
		db.WorkflowNodePatch{Status: &running, StartedAt: &startedAt, Assignee: &owner}); err != nil {
		slog.Warn("workflow engine: claim (mark running) failed", "instance", instanceID, "node", node.NodeID, "error", err)
		return nil
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: node.NodeID,
		Kind: db.WorkflowEventNodeStarted, Message: "engine: running " + def.Executor.Run})
	return &nodeClaim{nodeID: node.NodeID, def: def, scope: instanceScope(inst)}
}

// settleClaimedNode finishes a claimed node after its command AND verification
// already ran lock-free (their disposition is passed in). It re-acquires the
// lock, re-reads fresh, and applies the result only if the claim is still valid
// (instance running, node still the running one we claimed). Returns true when
// the node settled (so the caller drains the next), false when it deferred or
// the claim was invalidated by a concurrent cancel/delete. ONLY DB writes —
// capture, status-settle, Advance, recompute — run under the lock; no shell-out
// happens here, so a long verify can't block a dashboard cancel/delete.
func settleClaimedNode(instanceID int64, claim *nodeClaim, exec workflow.ExecResult, verdict workflow.VerifyDisposition) bool {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	now := time.Now()
	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		return false // cancelled / deleted / completed while the command ran
	}
	node, err := db.GetWorkflowNode(instanceID, claim.nodeID)
	if err != nil || node == nil || node.Status != db.WorkflowNodeStatusRunning {
		return false // someone moved this node (manual settle, cancel) — discard our result
	}
	// Defensive: only settle a node WE claimed (carries our sentinel). A human
	// who grabbed it (re-assigned + still running) is not ours to settle.
	if node.Assignee != engineAssignee {
		return false
	}
	tmpl, terr := rebuildInstanceTemplate(inst)
	if terr != nil || tmpl == nil || tmpl.Nodes[claim.nodeID] == nil {
		return false
	}

	if exec.Outcome == workflow.ExecError {
		settleWorkflowNodeFailed(inst, claim.nodeID, tmpl, exec.Err, now)
		return true
	}

	// Capture output into the node + instance vars so downstream refs resolve.
	captureNodeOutput(inst, claim.def, claim.nodeID, exec.Output)

	if verdict.Defer {
		// tool/program never defer verification (only human/ai do); guard anyway.
		awaiting := db.WorkflowNodeStatusAwaitingVerify
		_, _ = db.UpdateWorkflowNode(instanceID, claim.nodeID, db.WorkflowNodePatch{Status: &awaiting})
		return false
	}
	if !verdict.Done {
		settleWorkflowNodeFailed(inst, claim.nodeID, tmpl, verdict.Err, now)
		return true
	}
	settleWorkflowNodeDone(inst, claim.nodeID, tmpl, verdict.Outcome, now)
	return true
}

// nextRunnableNode returns the first ready node whose executor the engine runs
// synchronously (tool/program) AND that the engine is allowed to auto-run.
// ai/human ready nodes are skipped — they are driven by start/attach + the
// dashboard, not the engine (slice A). Insertion order (chart order) gives a
// stable, predictable pick.
func nextRunnableNode(inst *db.WorkflowInstance, nodes []*db.WorkflowNode) *db.WorkflowNode {
	for _, n := range nodes {
		if n.Status != db.WorkflowNodeStatusReady {
			continue
		}
		switch n.ExecutorKind {
		case string(workflow.ExecTool), string(workflow.ExecProgram):
			if engineMayAutoRun(inst, n) {
				return n
			}
		}
	}
	return nil
}

// engineMayAutoRun is the single chokepoint deciding whether the engine may
// auto-execute a tool/program node's command without human consent. Today it
// allows every node from a first-party source (project/user/example) — the only
// sources that can be instantiated on this branch.
//
// It is the explicit seam for two pending cross-deps, so they plug in here
// rather than being retrofitted across the dispatch:
//   - JOH-12 (external dir:/git: sources): a node whose template was resolved
//     from an EXTERNAL source runs a third-party command, so it must NOT be
//     auto-executed — it is left for the operator. When workflow.Source grows an
//     IsExternal() predicate, gate on the instance's snapshotted source here.
//   - JOH-17 (per-source approval gate): once an operator can approve a source,
//     this consults that grant instead of a blanket allow.
//
// Returning false leaves the node ready (untouched) so the dashboard / a future
// approval flow can run it; the engine simply never picks it.
//
// It also re-asserts the engine opt-in gate. runWorkflowEngineTick already
// returns early when the engine is disabled, so in the normal path this is
// redundant — but as the single chokepoint that authorises auto-execution, it
// must not authorise anything when the engine is off. Belt-and-suspenders
// against a future caller that reaches the dispatch without the tick's guard.
func engineMayAutoRun(inst *db.WorkflowInstance, _ *db.WorkflowNode) bool {
	return workflowEngineEnabled && !workflowInstanceIsExternal(inst)
}

// workflowInstanceIsExternal reports whether an instance was created from an
// external template source, read off its snapshotted template_ref (e.g.
// "git:url@ref#path" / "dir:/path" → external; "project:x" / "user:y" /
// "example:z" / a bare unqualified name → first-party). Deriving it from the
// already-snapshotted ref needs no schema change: the ref IS the source spec.
//
// Classification defers to workflow.Source.IsExternal() (the single source of
// truth, JOH-12) rather than re-enumerating dir/git here, so a future external
// scheme it learns about is honored automatically. A bare ref (no scheme) is
// first-party. An unrecognised scheme is NOT a known first-party source, so it
// is treated as external — fail-closed on the security gate, a new/unknown
// source can never silently slip past and get auto-run.
func workflowInstanceIsExternal(inst *db.WorkflowInstance) bool {
	scheme, _, found := strings.Cut(inst.TemplateRef, ":")
	if !found {
		return false // bare unqualified name → first-party
	}
	switch src := workflow.Source(scheme); src {
	case workflow.SourceProject, workflow.SourceUser, workflow.SourceExample:
		return false
	case workflow.SourceDir, workflow.SourceGit:
		return src.IsExternal() // single source of truth (true)
	default:
		return true // unknown scheme → fail-closed external
	}
}

// settleWorkflowNodeDone marks a node done with its branch outcome, advances the
// graph through the shared helpers (the SAME ones the manual PATCH path uses),
// and recomputes the instance status. Holding the instance lock is required.
func settleWorkflowNodeDone(inst *db.WorkflowInstance, nodeID string, tmpl *workflow.Template, outcome string, now time.Time) {
	done := db.WorkflowNodeStatusDone
	fin := now
	oc := outcome
	cleared := "" // drop the engine-owner sentinel now the run is over
	_, _ = db.UpdateWorkflowNode(inst.ID, nodeID, db.WorkflowNodePatch{Status: &done, Outcome: &oc, FinishedAt: &fin, Assignee: &cleared})
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: nodeID, Kind: db.WorkflowEventNodeDone, Message: "engine: outcome " + outcome})

	advanced := workflow.Advance(tmpl, nodeID, outcome, nodeStateMap(inst.ID))
	applyWorkflowAdvance(inst.ID, advanced, now)
	recomputeAndPersistInstanceStatus(inst, tmpl)
}

// settleWorkflowNodeFailed marks a node failed and advances: a node with
// on_fail: continue + a |fail| edge follows it; otherwise the failure halts the
// instance (recompute → failed). Holding the instance lock is required.
func settleWorkflowNodeFailed(inst *db.WorkflowInstance, nodeID string, tmpl *workflow.Template, reason string, now time.Time) {
	failed := db.WorkflowNodeStatusFailed
	fin := now
	oc := workflow.OutcomeFail
	cleared := "" // drop the engine-owner sentinel now the run is over
	_, _ = db.UpdateWorkflowNode(inst.ID, nodeID, db.WorkflowNodePatch{Status: &failed, Outcome: &oc, FinishedAt: &fin, Assignee: &cleared})
	msg := "engine: node failed"
	if reason != "" {
		msg += ": " + reason
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: nodeID, Kind: db.WorkflowEventNodeFailed, Message: msg})

	advanced := workflow.Advance(tmpl, nodeID, workflow.OutcomeFail, nodeStateMap(inst.ID))
	applyWorkflowAdvance(inst.ID, advanced, now)
	recomputeAndPersistInstanceStatus(inst, tmpl)
}

// recomputeAndPersistInstanceStatus recomputes the instance status from its
// current nodes and persists + event-logs any change. Mirrors the manual PATCH
// path's recompute block.
func recomputeAndPersistInstanceStatus(inst *db.WorkflowInstance, tmpl *workflow.Template) {
	nodes, _ := db.ListWorkflowNodes(inst.ID)
	newStatus := recomputeWorkflowInstanceStatus(tmpl, nodes)
	if newStatus != inst.Status {
		if _, err := db.UpdateWorkflowInstanceStatus(inst.ID, newStatus); err == nil {
			appendInstanceStatusEvent(inst.ID, newStatus)
			inst.Status = newStatus
		}
	}
}

// instanceScope assembles the interpolation scope from an instance's params and
// captured vars. Vars shadow params on a name clash (a capture is more specific
// than an instantiation param). Malformed JSON degrades to an empty layer rather
// than failing the node — interpolation then reports the unresolved ref.
func instanceScope(inst *db.WorkflowInstance) workflow.Scope {
	scope := workflow.Scope{}
	mergeJSONObject(scope, inst.Params)
	mergeJSONObject(scope, inst.Vars)
	return scope
}

func mergeJSONObject(dst workflow.Scope, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return
	}
	maps.Copy(dst, m)
}

// maxCapturedOutputBytes caps how much of a node's output is captured into the
// node row and the instance vars. A chatty build log would otherwise grow vars
// without bound — and vars is re-marshalled per capturing node and re-parsed on
// every claim, so unbounded output is O(nodes × output) work plus a row that
// balloons. The TAIL is kept (a command's verdict / error is at the end, and
// enum verify reads the last line), with a truncation marker prepended.
const maxCapturedOutputBytes = 64 * 1024

// capCapturedOutput trims s to its last maxCapturedOutputBytes bytes, prefixing
// a marker when it truncated. Byte-based (not rune-based) since it bounds
// storage; it trims to the next line boundary inside the window so the kept tail
// stays line-clean (important for enum verify's last-non-empty-line read).
func capCapturedOutput(s string) string {
	if len(s) <= maxCapturedOutputBytes {
		return s
	}
	tail := s[len(s)-maxCapturedOutputBytes:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl+1 < len(tail) {
		tail = tail[nl+1:] // drop the partial leading line
	}
	return "[…output truncated to last 64KiB…]\n" + tail
}

// captureNodeOutput stores a node's (capped) output on the node row, and — when
// the node declares a capture name — into the instance vars under that name AND
// under "<nodeID>.output" so both {{capture}} and {{node.output}} references
// resolve. Best-effort: a failed vars write just means a later ref goes
// unresolved.
func captureNodeOutput(inst *db.WorkflowInstance, def *workflow.Node, nodeID, rawOutput string) {
	output := capCapturedOutput(rawOutput)
	out := output
	_, _ = db.UpdateWorkflowNode(inst.ID, nodeID, db.WorkflowNodePatch{Output: &out})

	vars := map[string]any{}
	mergeJSONObject(vars, inst.Vars)

	// Always expose {{<nodeID>.output}} via a per-node map. Preserve any other
	// keys already under this node id (a node could capture multiple values in
	// future); only the "output" sub-key is (re)set here.
	nodeEntry, _ := vars[nodeID].(map[string]any)
	if nodeEntry == nil {
		nodeEntry = map[string]any{}
	}
	nodeEntry["output"] = output
	vars[nodeID] = nodeEntry

	// Expose {{<capture>}} when named — UNLESS the capture name would clobber a
	// per-node output map (its name collides with some node id that already has
	// an {"output":...} entry). In that collision the structured {{id.output}}
	// access wins; we skip the bare-string capture rather than silently break
	// the map. A capture named after its OWN node is the common, harmless case
	// of this and is likewise skipped (the value is already at {{id.output}}).
	if capName := strings.TrimSpace(def.Capture); capName != "" {
		if existing, ok := vars[capName].(map[string]any); ok {
			if _, isNodeMap := existing["output"]; isNodeMap {
				slog.Warn("workflow engine: capture name collides with a node-output map; "+
					"use {{"+capName+".output}} instead of {{"+capName+"}}",
					"instance", inst.ID, "node", nodeID, "capture", capName)
			} else {
				vars[capName] = output
			}
		} else {
			vars[capName] = output
		}
	}

	// Persist to the DB — this is what makes the capture visible downstream:
	// the next drain step's claimNextNode re-reads the instance fresh and
	// rebuilds the scope from these persisted vars. (We also refresh the local
	// inst.Vars to keep this in-memory copy self-consistent, but nothing in the
	// current flow reads it after this point — the DB is the source of truth.)
	if b, err := json.Marshal(vars); err == nil {
		if _, err := db.UpdateWorkflowInstanceVars(inst.ID, string(b)); err == nil {
			inst.Vars = string(b)
		}
	}
}

// bashRunner is the production workflow.Runner: it runs a command via `bash -c`
// (matching how Claude Code and the old task runner execute commands) with the
// context's timeout + graceful kill, returning combined output and exit-0.
type bashRunner struct{}

func (bashRunner) Run(ctx context.Context, command, workdir string) (string, bool, error) {
	cmd := executil.CommandContext(ctx, "bash", "-c", command)
	if strings.TrimSpace(workdir) != "" {
		cmd.Dir = workdir
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil, err
}
