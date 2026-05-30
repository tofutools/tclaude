package agentd

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"strconv"
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
// format/none), settle the node, and advance the graph.
//
// Slice B adds the AI path (spawnReadyAINodes): a ready `ai` node on a
// bound-group instance is auto-spawned an agent via executeSpawn (NON-BLOCKING —
// the engine spawns and moves on; the agent works async and signals done later
// via the node-PATCH the CLI wraps). An ai node on an UNBOUND instance, and any
// `human` node, is still left where it is (ready/awaiting) for the dashboard /
// Step-4 start path — the engine never force-spawns without a group, nor auto-
// runs a human node. ai-verify (a judge agent ruling pass/fail) is a deliberate
// carve-out, parked behind a clean seam: the engine accepts an ai node's
// self-reported done as today (equivalent to verify.kind:none), and the judge
// round-trip is a follow-up (it needs its own completion contract). human-verify
// is the dashboard approve gate, unchanged.
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

// workflowAIPerInstanceCap / workflowAIGlobalCap bound how many `ai` nodes the
// engine auto-spawns concurrently — per instance, and across all instances. A
// fan-out graph could otherwise spawn an agent per parallel branch (per
// instance) or many instances could collectively spawn a swarm (global); these
// caps keep the autonomous driver from launching agents faster than a human can
// follow. They gate ONLY the engine's auto-spawn — the dashboard start path is
// never blocked by them. runServe sets both from config (defaults below); tests
// override via SetWorkflowAICapsForTest.
//
// LIMITATION (tracked for slice C — JOH-8 #9, stuck/SLA detection): the cap
// counts every `running` ai node, and a running ai node clears only when its
// agent reports done/failed via the node-PATCH. An agent that dies WITHOUT
// reporting leaves its node `running` forever, so it counts against these caps
// indefinitely — enough dead agents can wedge the global cap and silently stop
// all future auto-spawns. There is no liveness/timeout reconciliation yet (the
// startup reaper only recovers the brief claim→spawn window, not a settled live
// node). Slice C adds the stuck-node sweep (idle/crashed-assignee detection +
// escalation) that resets or fails such nodes; until then a wedged cap is an
// operator-visible "stuck ai node" condition recovered by cancelling the
// instance from the dashboard.
const (
	defaultWorkflowAIPerInstanceCap = 1
	defaultWorkflowAIGlobalCap      = 8
)

var (
	workflowAIPerInstanceCap = defaultWorkflowAIPerInstanceCap
	workflowAIGlobalCap      = defaultWorkflowAIGlobalCap
)

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
// stamped by claimNextNode/claimNextAINode when THIS engine claims a node and
// cleared when it settles, so it pinpoints exactly an engine corpse. A
// tool/program node a HUMAN manually drove to running via the dashboard
// (allowed by the PATCH path, with an empty/human assignee) is therefore NOT
// reaped — its manual state is preserved across a restart.
//
// This now covers ai nodes too: an ai node carries the sentinel only in the
// brief claim→spawn→settle window (claimNextAINode marks it running+sentinel
// before executeSpawn; settleAISpawn swaps in the real conv-id on success). A
// crash inside that window leaves a sentinel-bearing running ai node with no
// spawned agent yet, so reaping it back to ready is correct — the next tick
// re-spawns. A LIVE ai node (a successfully-spawned agent) carries its conv-id
// as the assignee, not the sentinel, so it is left alone; a human node is
// dashboard-driven and likewise untouched. (Reaping a node WAS re-spawns: if
// the crash landed after executeSpawn returned but before settle, the original
// agent is orphaned in the group and a duplicate is spawned — a rare, bounded
// cost of the same idempotent-re-run model the tool path accepts.) Reaping runs
// regardless of the engine's opt-in gate: it only ever unsticks rows the engine
// itself created.
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
	// Two passes per instance. First drain the synchronous tool/program nodes
	// (run-to-completion, possibly readying downstream ai nodes); then do a
	// non-blocking sweep of ready ai nodes, auto-spawning agents for them. Order
	// matters: draining tools first means a tool node that hands off to an ai
	// node gets that ai node spawned in the SAME tick rather than the next.
	drainRunnableToolNodes(ctx, instanceID)
	spawnReadyAINodes(instanceID)
}

// drainRunnableToolNodes is the synchronous tool/program pass: each ready
// tool/program node is claimed, run, verified, settled, and the graph advanced.
// It loops so a chain of instantly-completing tool nodes drains within one tick;
// bounded by a fuel counter so a misconfigured tight loop can't spin forever.
func drainRunnableToolNodes(ctx context.Context, instanceID int64) {
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

// ----- AI executor (non-blocking auto-spawn) ---------------------------------

// spawnReadyAINodes is the engine's AI path. Unlike the tool drain it does NOT
// run-and-settle the node: a ready `ai` node is auto-spawned an agent into the
// instance's bound group (reusing executeSpawn, the same core the dashboard
// start path uses), the node goes `running` with the spawned conv-id as its
// assignee, and the engine MOVES ON. The agent then works asynchronously and
// signals completion later via the node-PATCH the `tclaude workflow node … done`
// CLI wraps, which the engine observes on a subsequent tick (it never blocks on
// the agent's work).
//
// Three gates, all required:
//   - engineMayAutoRun — the opt-in + external-source guard (an externally-
//     sourced instance's ai nodes are left for the operator, same as tool nodes).
//   - a BOUND group — an unbound instance has nowhere to spawn into, so its ai
//     nodes stay `ready` for the dashboard start path; the engine never
//     force-spawns an agent without a group.
//   - the per-instance + global parallelism caps — so a fan-out graph (or many
//     instances) can't launch a swarm of agents at once.
//
// Each spawn is claim→spawn→settle: the node is claimed SYNCHRONOUSLY under the
// lock (marked running + the engine sentinel) — that claim is what enforces the
// caps and prevents a double-claim — but executeSpawn + settleAISpawn then run
// OFF the tick goroutine (goBackground). The conv-id handshake can take seconds
// and the engine ticks on a single goroutine, so running it inline would stall
// every OTHER instance's progress for the whole handshake. Spawning off-tick is
// what makes the AI path genuinely "spawn and move on". A claimed node already
// counts as running toward both caps, so the caps stay honest while the spawn is
// in flight, and the loop naturally stops once a cap is hit (claimNextAINode
// returns nil) — a per-instance cap of 1 claims exactly one node per tick.
func spawnReadyAINodes(instanceID int64) {
	// Backstop only — the per-instance cap normally bounds this far lower. A
	// raised cap on a wide fan-out could legitimately want several; this keeps a
	// pathological config from spinning the tick.
	const maxSpawnsPerTick = 32
	for range maxSpawnsPerTick {
		claim := claimNextAINode(instanceID)
		if claim == nil {
			return // no eligible ai node, or a cap/gate blocks spawning
		}
		// claim is a fresh per-iteration variable (declared in the loop body), so
		// capturing it in the goroutine is race-free. The node stays running +
		// sentinel until this goroutine settles it; a failed spawn resets it to
		// ready, so it is retried on a later TICK, not in a tight loop.
		goBackground(func() {
			outcome, fail := executeSpawn(claim.group, claim.params)
			settleAISpawn(instanceID, claim, outcome, fail)
		})
	}
}

// aiNodeClaim is a ready ai node the engine has claimed (marked running + the
// engine sentinel) and is about to spawn an agent for, carrying everything
// settleAISpawn needs without re-reading mid-spawn.
type aiNodeClaim struct {
	nodeID string
	group  *db.AgentGroup
	params spawnParams
}

// claimNextAINode picks the next ready ai node eligible for auto-spawn under the
// instance lock and claims it (running + engine sentinel), returning the snapshot
// to spawn it. Returns nil when nothing is eligible — unbound instance, gate
// closed, a cap is hit, or no ready ai node. Holds the lock only for the claim;
// the spawn itself runs lock-free in the caller.
func claimNextAINode(instanceID int64) *aiNodeClaim {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		return nil
	}
	// The engine never force-spawns without a group: an unbound instance's ai
	// nodes stay ready for the dashboard start path.
	if inst.GroupID == 0 {
		return nil
	}
	// Opt-in + external-source guard (the same chokepoint the tool path uses;
	// it ignores the node arg).
	if !engineMayAutoRun(inst, nil) {
		return nil
	}
	g, err := db.GetAgentGroupByID(inst.GroupID)
	if err != nil || g == nil {
		// Bound group vanished — leave the node ready; the dashboard surfaces the
		// dangling binding (boundGroup 400s there). Nothing for the engine to do.
		return nil
	}
	nodes, err := db.ListWorkflowNodes(instanceID)
	if err != nil {
		slog.Warn("workflow engine: list nodes failed (ai pass)", "instance", instanceID, "error", err)
		return nil
	}

	// Per-instance cap: count this instance's already-running ai nodes.
	perInstance := 0
	for _, n := range nodes {
		if n.ExecutorKind == string(workflow.ExecAI) && n.Status == db.WorkflowNodeStatusRunning {
			perInstance++
		}
	}
	if perInstance >= workflowAIPerInstanceCap {
		return nil
	}
	// Global cap: total running ai nodes across all instances. Re-queried each
	// pass so spawns committed earlier this tick count toward it.
	total, err := db.CountRunningWorkflowNodesByKind(string(workflow.ExecAI))
	if err != nil {
		slog.Warn("workflow engine: count running ai nodes failed", "instance", instanceID, "error", err)
		return nil
	}
	if total >= workflowAIGlobalCap {
		return nil
	}

	// First ready ai node in chart order.
	var node *db.WorkflowNode
	for _, n := range nodes {
		if n.Status == db.WorkflowNodeStatusReady && n.ExecutorKind == string(workflow.ExecAI) {
			node = n
			break
		}
	}
	if node == nil {
		return nil
	}

	tmpl, terr := rebuildInstanceTemplate(inst)
	if terr != nil || tmpl == nil || tmpl.Nodes[node.NodeID] == nil {
		slog.Warn("workflow engine: cannot rebuild ai node def; skipping", "instance", instanceID, "node", node.NodeID, "error", terr)
		return nil
	}
	def := tmpl.Nodes[node.NodeID]

	cwd, cwdErr := resolveSpawnCwd(g.DefaultCwd)
	if cwdErr != nil {
		// A bad group default cwd is an operator misconfiguration; leave the node
		// ready and log (the dashboard start path would 400 with the same error).
		slog.Warn("workflow engine: resolve group cwd failed; leaving ai node ready",
			"instance", instanceID, "node", node.NodeID, "group", g.Name, "error", cwdErr)
		return nil
	}

	// Interpolate the prompt against the instance scope (params + captures) so a
	// briefing referencing {{param}} / {{upstream.output}} resolves. Unresolved
	// refs are left visible (logged) rather than blanked — a prompt is not shell,
	// so the risk is prompt-injection, not command execution.
	initMsg, missing := instanceScope(inst).Interpolate(strings.TrimSpace(def.Executor.Prompt))
	if len(missing) > 0 {
		slog.Warn("workflow engine: ai node prompt has unresolved refs",
			"instance", instanceID, "node", node.NodeID, "missing", missing)
	}
	if initMsg != "" && !isValidInitialMessage(initMsg) {
		slog.Warn("workflow engine: ai node prompt is not a valid initial message; spawning without it",
			"instance", instanceID, "node", node.NodeID)
		initMsg = ""
	}

	// Claim it. Marking it running + the engine sentinel (the same marker the
	// tool path uses) is what makes a crash mid-spawn recoverable: the startup
	// reaper resets a sentinel-bearing running node back to ready, so the next
	// tick re-spawns. Once settleAISpawn swaps in the real conv-id assignee the
	// reaper leaves it alone (a live agent's node is not an engine corpse).
	running := db.WorkflowNodeStatusRunning
	startedAt := time.Now()
	owner := engineAssignee
	if _, err := db.UpdateWorkflowNode(instanceID, node.NodeID,
		db.WorkflowNodePatch{Status: &running, StartedAt: &startedAt, Assignee: &owner}); err != nil {
		slog.Warn("workflow engine: claim ai node (mark running) failed", "instance", instanceID, "node", node.NodeID, "error", err)
		return nil
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: node.NodeID,
		Kind: db.WorkflowEventNodeStarted, Message: "engine: spawning agent for ai node"})

	return &aiNodeClaim{
		nodeID: node.NodeID,
		group:  g,
		params: spawnParams{
			Name:           def.Executor.Agent,
			Role:           def.Executor.Agent,
			Descr:          "workflow " + strconv.FormatInt(instanceID, 10) + " · " + node.Label,
			InitialMessage: initMsg,
			Cwd:            cwd,
			GroupContext:   g.DefaultContext,
		},
	}
}

// settleAISpawn finishes an ai-node claim after executeSpawn ran (off the tick
// goroutine). It re-acquires the instance lock, re-reads fresh, and applies the
// result only if the claim is still valid (instance running, node still the
// running one WE claimed — sentinel-assigned). On success it swaps the engine
// sentinel for the spawned conv-id, leaving the node `running` for the live agent
// to drive to completion. On spawn failure it resets the node to `ready` so a
// later tick retries. If the claim was invalidated (a concurrent cancel/delete,
// or the node moved) the spawned agent — if any — is now an orphan member of the
// group: that is surfaced in a log, not torn down (we don't fight the human's
// cancel).
func settleAISpawn(instanceID int64, claim *aiNodeClaim, outcome *spawnOutcome, fail *spawnFailure) {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		if outcome != nil {
			slog.Warn("workflow engine: ai spawn finished after instance left running; agent orphaned in group",
				"instance", instanceID, "node", claim.nodeID, "conv", outcome.ConvID)
		}
		return
	}
	node, err := db.GetWorkflowNode(instanceID, claim.nodeID)
	if err != nil || node == nil || node.Status != db.WorkflowNodeStatusRunning || node.Assignee != engineAssignee {
		// A manual settle / cancel moved the node while we spawned — discard.
		if outcome != nil {
			slog.Warn("workflow engine: ai node moved during spawn; spawned agent orphaned in group",
				"instance", instanceID, "node", claim.nodeID, "conv", outcome.ConvID)
		}
		return
	}

	if fail != nil {
		// Spawn failed — reset to ready (drop the sentinel + the premature start
		// stamp) so a later tick retries rather than the node hanging running.
		ready := db.WorkflowNodeStatusReady
		cleared := ""
		var noStart time.Time
		_, _ = db.UpdateWorkflowNode(instanceID, claim.nodeID,
			db.WorkflowNodePatch{Status: &ready, Assignee: &cleared, StartedAt: &noStart})
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: claim.nodeID,
			Kind: db.WorkflowEventNodeReady, Message: "engine: ai spawn failed, will retry: " + fail.Msg})
		slog.Warn("workflow engine: ai spawn failed; node reset to ready",
			"instance", instanceID, "node", claim.nodeID, "error", fail.Msg)
		return
	}

	// Success — hand the node to the live agent: swap the sentinel for its
	// conv-id. The node stays `running`; the agent settles it later via the
	// node-PATCH the CLI wraps, which authorises an assignee to settle its own
	// node (so the conv-id MUST be in place before the agent can report done).
	convID := outcome.ConvID
	if _, err := db.UpdateWorkflowNode(instanceID, claim.nodeID,
		db.WorkflowNodePatch{Assignee: &convID}); err != nil {
		slog.Warn("workflow engine: record ai assignee failed", "instance", instanceID, "node", claim.nodeID, "error", err)
		return
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: claim.nodeID,
		Kind: db.WorkflowEventNodeStarted, Message: "engine: spawned " + convID + " into group " + claim.group.Name})
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
