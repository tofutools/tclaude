package agentd

import (
	"context"
	"encoding/json"
	"fmt"
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
// Loops/retries SHIPPED (JOH-39): an in-place retry re-arms a failed-verify node
// to ready, and a back-edge loop-back resets the target + its body to ready/
// pending; both re-run on the next tick, bounded by max_visits (EffectiveMaxVisits
// + settleWorkflowNodeMaxVisits). Visits is bumped when the re-armed node is next
// CLAIMED (tool) / SPAWNED (ai), so it counts real executions.
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
// A `running` ai node (worker) and an `awaiting_verify` ai-verify node (judge)
// both count toward the global cap and clear only when settled. An agent that
// dies WITHOUT reporting would otherwise pin its node — and its cap slot —
// forever; sweepStuckNodes (the SLA sweep, below) is what releases such a slot at
// its terminal rung, so a few dead agents can no longer silently wedge auto-spawn.
const (
	defaultWorkflowAIPerInstanceCap = 1
	defaultWorkflowAIGlobalCap      = 8
)

var (
	workflowAIPerInstanceCap = defaultWorkflowAIPerInstanceCap
	workflowAIGlobalCap      = defaultWorkflowAIGlobalCap
)

// defaultWorkflowMaxVisits is the engine default cap on a node's total
// executions (loop iterations + in-place retries) when its own max_visits is
// unspecified (0). For an autonomous engine that auto-runs shell + spawns
// agents, an omitted loop bound must be fail-safe rather than unbounded — a
// runaway back-edge can never spin forever by mere omission. A node opts into a
// different finite cap with max_visits: N, or truly-unbounded with -1 (an
// explicit power-user choice). See workflow.EffectiveMaxVisits. runServe sets
// workflowMaxVisits from config (agent.workflow_max_visits); tests override via
// SetWorkflowMaxVisitsForTest. (JOH-39)
const defaultWorkflowMaxVisits = 20

var workflowMaxVisits = defaultWorkflowMaxVisits

// workflowNodeSLA / workflowHumanNodeSLA are the engine-default stuck thresholds
// T for the escalation sweep (JOH-41), resolved per node by class: a NON-human
// node (ai worker / ai-verify judge) defaults to workflowNodeSLA; a node a HUMAN
// must action (executor.kind:human, or a verify.kind:human approve-gate) defaults
// to the more patient workflowHumanNodeSLA. A node overrides its own T via the
// `sla:` field (workflow.EffectiveSLA). A node idle past fractions of T climbs the
// ladder: warn (ping the actor) -> escalate (notify the human) -> terminal. The
// terminal rung fails ONLY a non-human node with no live actor — the original
// MED-C backstop, releasing its cap slot (a worker whose agent crashed, a dead
// judge, a cap-starved-never-judged node); a live agent's node and any human node
// are never auto-failed, only notified. Non-human default exposes the previously
// hardcoded 15m (no behavior regression). runServe sets both from config
// (agent.workflow_node_sla / agent.workflow_human_node_sla); tests shrink them via
// SetWorkflowNodeSLAForTest / SetWorkflowHumanNodeSLAForTest.
const (
	defaultWorkflowNodeSLA      = 15 * time.Minute
	defaultWorkflowHumanNodeSLA = 60 * time.Minute
)

var (
	workflowNodeSLA      = defaultWorkflowNodeSLA
	workflowHumanNodeSLA = defaultWorkflowHumanNodeSLA
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
// This now covers ai nodes too — in BOTH sentinel-bearing windows, because the
// sweep deliberately skips sentinel nodes and the spawn passes only claim
// non-sentinel ones, so a crash-stranded sentinel is recoverable ONLY here:
//   - a `running` ai node (claimNextAINode marks it running+sentinel before
//     executeSpawn; settleAISpawn swaps in the conv-id on success): a crash
//     inside that window has no spawned agent yet, so reaping it back to `ready`
//     is correct — the next tick re-spawns.
//   - an `awaiting_verify` ai-verify node (claimNextVerifyJudge stamps the
//     sentinel before spawning the judge; settleJudgeSpawn swaps in the conv-id):
//     a crash inside THAT window would otherwise wedge the node forever (the
//     sweep skips sentinels, the judge pass only claims empty-assignee nodes) AND
//     keep eating a global cap slot. Clearing the sentinel back to empty (the
//     "ready to judge" marker) — status left awaiting_verify, the executor is
//     already done — lets the next tick re-spawn the judge.
//
// A LIVE ai node (a successfully-spawned worker/judge) carries its conv-id, not
// the sentinel, so it is left alone; a human node is dashboard-driven and
// likewise untouched. (Reaping a worker node WAS re-spawns: a crash after
// executeSpawn returned but before settle orphans the original agent and spawns a
// duplicate — a rare, bounded cost of the same idempotent-re-run model the tool
// path accepts.) Reaping runs regardless of the engine's opt-in gate: it only
// ever unsticks rows the engine itself created.
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
			if n.Assignee != engineAssignee {
				continue // not an engine corpse — leave manual/ai/human/live nodes alone
			}
			switch n.Status {
			case db.WorkflowNodeStatusRunning:
				// Mid tool-run / worker-spawn corpse → reset to ready (re-run/re-spawn).
				if _, err := db.UpdateWorkflowNode(inst.ID, n.NodeID,
					db.WorkflowNodePatch{Status: &ready, Assignee: &cleared}); err == nil {
					_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: n.NodeID,
						Kind: db.WorkflowEventNodeReady, Message: "engine: reset orphaned running node after restart"})
					slog.Info("workflow engine: reaped orphaned running node", "instance", inst.ID, "node", n.NodeID)
				}
			case db.WorkflowNodeStatusAwaitingVerify:
				// Mid judge-spawn corpse → clear the sentinel back to empty (ready to
				// judge); status stays awaiting_verify. Next tick re-spawns the judge.
				if _, err := db.UpdateWorkflowNode(inst.ID, n.NodeID,
					db.WorkflowNodePatch{Assignee: &cleared}); err == nil {
					_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: n.NodeID,
						Kind: db.WorkflowEventNodeAwaitingVerify, Message: "engine: reset orphaned judge-claim after restart"})
					slog.Info("workflow engine: reaped orphaned judge-claim", "instance", inst.ID, "node", n.NodeID)
				}
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
	// Passes per instance, ordered so a chain advances within one tick where it
	// can. (1) drain synchronous tool/program nodes (may ready downstream ai
	// nodes); (2) auto-spawn ready ai-executor nodes (workers); (3) spawn judges
	// for nodes parked in awaiting_verify by an ai verify.kind; (4) deliver
	// predecessor output to each bound successor's inbox (JOH-40); (5) sweep stuck
	// nodes — warn/escalate/notify, and fail a no-actor node to free its cap slot
	// (JOH-41).
	drainRunnableToolNodes(ctx, instanceID)
	spawnReadyAINodes(instanceID)
	spawnReadyVerifyJudges(instanceID)
	deliverReadyHandoffs(instanceID)
	sweepStuckNodes(instanceID)
}

// drainRunnableToolNodes is the synchronous tool/program pass: each ready
// tool/program node is claimed, run, verified, settled, and the graph advanced.
// It loops so a chain of instantly-completing tool nodes drains within one tick;
// bounded by a fuel counter so a misconfigured tight loop can't spin forever.
func drainRunnableToolNodes(ctx context.Context, instanceID int64) {
	const maxStepsPerTick = 100
	// attempted bounds each node to ONE execution per tick: a node re-armed to
	// ready this tick (an in-place retry, or a loop-back that re-readied a body
	// node) is skipped until the NEXT tick. This tick-paces retries (one attempt
	// per tick — JOH-39) so a fast-failing node can't spin the tick, and unifies
	// retry pacing with loop-back (both just "re-arm; the next tick runs it").
	attempted := map[string]bool{}
	for range maxStepsPerTick {
		claim := claimNextNode(instanceID, attempted)
		if claim == nil {
			return // nothing actionable this tick
		}
		attempted[claim.nodeID] = true
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
			// Node was cancelled out from under us, or the claim was invalidated —
			// stop draining so we don't spin; next tick revisits. (A retry re-arm
			// returns true and is skipped via `attempted`, so it does NOT stop the
			// drain — other ready nodes still run this tick.)
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

	// Visit cap (JOH-39): an ai node that has used its execution budget is
	// force-failed + halts rather than re-spawned, the same guard the tool claim
	// applies — so a back-edge loop through an ai node can't spawn agents forever.
	if cap, unbounded := workflow.EffectiveMaxVisits(def, workflowMaxVisits); !unbounded && node.Visits >= int64(cap) {
		settleWorkflowNodeMaxVisits(inst, node.NodeID, cap, time.Now())
		return nil
	}

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
	// Visits is bumped on spawn SUCCESS (settleAISpawn), NOT here: a claim that
	// fails to spawn (bad cwd, spawn error) resets to ready and must not burn a
	// visit, else a flaky group could force-fail a node that never actually ran.
	// So Visits counts agents actually launched, and the claim cap reads that count.
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
	// Bump Visits here (not at claim): this is the point an agent actually
	// launched, so a failed spawn never burns a visit and the cap counts real
	// executions (JOH-39).
	convID := outcome.ConvID
	visits := node.Visits + 1
	if _, err := db.UpdateWorkflowNode(instanceID, claim.nodeID,
		db.WorkflowNodePatch{Assignee: &convID, Visits: &visits}); err != nil {
		slog.Warn("workflow engine: record ai assignee failed", "instance", instanceID, "node", claim.nodeID, "error", err)
		return
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: claim.nodeID,
		Kind: db.WorkflowEventNodeStarted, Message: "engine: spawned " + convID + " into group " + claim.group.Name})
}

// ----- ai-verify judge round-trip (JOH-35) -----------------------------------

// spawnReadyVerifyJudges is the engine's ai-verify pass. A node reaches
// awaiting_verify when its executor is done but its definition-of-done is an AI
// judge (verify.kind: ai): an ai-executor node parks there on its worker's
// done-report (parkNodeForAIVerify), a tool/program node via the slice-A
// RunVerifier Defer. Both CLEAR the assignee, so an awaiting_verify node with an
// EMPTY assignee is "ready to judge". This pass spawns a short-lived judge agent
// into the bound group, hands it the interpolated Verify.Prompt + the node's
// reported output, and sets the node's assignee to the judge conv-id. The judge
// then reports its verdict through the SAME node-PATCH the worker used — `done`
// (pass) settles the node + advances, `fail` settles it failed — authorised
// because the judge is now the node's assignee ("assignee = currently-responsible
// actor": worker while running, judge while awaiting_verify). The worker can no
// longer settle it, so it can't self-approve.
//
// Same claim→spawn→settle shape as spawnReadyAINodes (locked claim, off-tick
// spawn). The claim only stamps the assignee — the node stays awaiting_verify —
// and the empty→sentinel→conv-id assignee progression both prevents a
// double-judge and counts the judge toward the global agent cap while in flight.
func spawnReadyVerifyJudges(instanceID int64) {
	const maxJudgesPerTick = 32 // backstop; the global cap normally bounds this lower
	for range maxJudgesPerTick {
		claim := claimNextVerifyJudge(instanceID)
		if claim == nil {
			return
		}
		// claim is a fresh per-iteration variable, race-free to capture. The node
		// stays awaiting_verify + sentinel until this goroutine settles it; a failed
		// spawn clears the sentinel so a later TICK retries (not a tight loop).
		goBackground(func() {
			outcome, fail := executeSpawn(claim.group, claim.params)
			settleJudgeSpawn(instanceID, claim, outcome, fail)
		})
	}
}

// verifyJudgeClaim is an awaiting_verify ai-verify node the engine has claimed
// (assignee = engine sentinel) and is about to spawn a judge for.
type verifyJudgeClaim struct {
	nodeID string
	group  *db.AgentGroup
	params spawnParams
}

// claimNextVerifyJudge picks the next awaiting_verify ai-verify node with no
// judge yet (empty assignee) under the instance lock and claims it (assignee =
// sentinel), returning the snapshot to spawn the judge. Returns nil when nothing
// is eligible — unbound/closed gate, the global agent cap is hit, or no such node.
func claimNextVerifyJudge(instanceID int64) *verifyJudgeClaim {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		return nil
	}
	// ai-verify only runs on a bound, auto-runnable instance — same gate as the
	// worker spawn. On an unbound/engine-off instance the node never parks here
	// (the worker's done settles directly), so this is belt-and-suspenders.
	if inst.GroupID == 0 || !engineMayAutoRun(inst, nil) {
		return nil
	}
	g, err := db.GetAgentGroupByID(inst.GroupID)
	if err != nil || g == nil {
		return nil
	}
	nodes, err := db.ListWorkflowNodes(instanceID)
	if err != nil {
		slog.Warn("workflow engine: list nodes failed (verify pass)", "instance", instanceID, "error", err)
		return nil
	}
	tmpl, terr := rebuildInstanceTemplate(inst)
	if terr != nil || tmpl == nil {
		slog.Warn("workflow engine: cannot rebuild template (verify pass); skipping", "instance", instanceID, "error", terr)
		return nil
	}

	// Global agent cap: workers (running ai) + judges (awaiting_verify with an
	// assignee) share the budget so the two passes can't collectively oversubscribe.
	workers, err := db.CountRunningWorkflowNodesByKind(string(workflow.ExecAI))
	if err != nil {
		slog.Warn("workflow engine: count running ai nodes failed (verify pass)", "instance", instanceID, "error", err)
		return nil
	}
	judges, err := db.CountAwaitingVerifyAssignedNodes()
	if err != nil {
		slog.Warn("workflow engine: count in-flight judges failed", "instance", instanceID, "error", err)
		return nil
	}
	if workers+judges >= workflowAIGlobalCap {
		return nil
	}

	// First awaiting_verify node wanting an ai judge with no judge yet: empty
	// assignee = ready to judge; sentinel = a judge is being spawned; a conv-id =
	// a judge is already assigned (waiting on its verdict).
	var node *db.WorkflowNode
	for _, n := range nodes {
		if n.Status == db.WorkflowNodeStatusAwaitingVerify && n.Assignee == "" && nodeWantsAIVerify(tmpl, n.NodeID) {
			node = n
			break
		}
	}
	if node == nil {
		return nil
	}
	def := tmpl.Nodes[node.NodeID]

	cwd, cwdErr := resolveSpawnCwd(g.DefaultCwd)
	if cwdErr != nil {
		slog.Warn("workflow engine: resolve group cwd failed; leaving node awaiting_verify",
			"instance", instanceID, "node", node.NodeID, "group", g.Name, "error", cwdErr)
		return nil
	}

	// Interpolate the judge's criteria against the instance scope, then assemble
	// the brief (criteria + the node's reported output + the exact report command).
	criteria, missing := instanceScope(inst).Interpolate(strings.TrimSpace(def.Verify.Prompt))
	if len(missing) > 0 {
		slog.Warn("workflow engine: verify prompt has unresolved refs",
			"instance", instanceID, "node", node.NodeID, "missing", missing)
	}
	prompt := buildVerifyJudgePrompt(instanceID, node.NodeID, node.Label, criteria, node.Output)
	if !isValidInitialMessage(prompt) {
		// Per-part capping keeps this rare, but never spawn a judge with a
		// malformed brief (it would have no criteria / no report command). Leave the
		// node for the stuck sweep or a dashboard action.
		slog.Warn("workflow engine: judge prompt is not a valid initial message; leaving node awaiting_verify",
			"instance", instanceID, "node", node.NodeID)
		return nil
	}

	// Claim: stamp the engine sentinel as the assignee (node stays
	// awaiting_verify). A crash mid-spawn leaves a sentinel-bearing awaiting_verify
	// node, recovered by reapOrphanedEngineNodes at startup — NOT the stuck sweep,
	// which deliberately skips sentinel nodes (an in-flight spawn). The reaper
	// clears the sentinel back to empty so the next tick re-spawns the judge.
	owner := engineAssignee
	if _, err := db.UpdateWorkflowNode(instanceID, node.NodeID, db.WorkflowNodePatch{Assignee: &owner}); err != nil {
		slog.Warn("workflow engine: claim verify node failed", "instance", instanceID, "node", node.NodeID, "error", err)
		return nil
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: node.NodeID,
		Kind: db.WorkflowEventNodeAwaitingVerify, Message: "engine: spawning ai-verify judge"})

	return &verifyJudgeClaim{
		nodeID: node.NodeID,
		group:  g,
		params: spawnParams{
			Name:           "verifier",
			Role:           "verifier",
			Descr:          "workflow " + strconv.FormatInt(instanceID, 10) + " · verify " + node.Label,
			InitialMessage: prompt,
			Cwd:            cwd,
			GroupContext:   g.DefaultContext,
		},
	}
}

// settleJudgeSpawn finishes a judge claim after executeSpawn ran off the tick
// goroutine. Re-reads fresh under the lock and applies the result only if the
// claim is still valid (instance running, node still awaiting_verify + sentinel).
// On success it records the judge conv-id as the assignee (the node stays
// awaiting_verify until the judge reports its verdict via node-PATCH). On spawn
// failure it clears the sentinel back to empty so a later tick retries. A claim
// invalidated by a concurrent cancel/delete orphans the spawned judge — logged,
// not torn down.
func settleJudgeSpawn(instanceID int64, claim *verifyJudgeClaim, outcome *spawnOutcome, fail *spawnFailure) {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		if outcome != nil {
			slog.Warn("workflow engine: judge spawn finished after instance left running; judge orphaned in group",
				"instance", instanceID, "node", claim.nodeID, "conv", outcome.ConvID)
		}
		return
	}
	node, err := db.GetWorkflowNode(instanceID, claim.nodeID)
	if err != nil || node == nil || node.Status != db.WorkflowNodeStatusAwaitingVerify || node.Assignee != engineAssignee {
		if outcome != nil {
			slog.Warn("workflow engine: verify node moved during judge spawn; judge orphaned in group",
				"instance", instanceID, "node", claim.nodeID, "conv", outcome.ConvID)
		}
		return
	}

	if fail != nil {
		// Spawn failed — clear the sentinel back to empty (node stays
		// awaiting_verify) so a later tick retries.
		cleared := ""
		_, _ = db.UpdateWorkflowNode(instanceID, claim.nodeID, db.WorkflowNodePatch{Assignee: &cleared})
		_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: claim.nodeID,
			Kind: db.WorkflowEventNodeAwaitingVerify, Message: "engine: judge spawn failed, will retry: " + fail.Msg})
		slog.Warn("workflow engine: judge spawn failed; node left awaiting_verify",
			"instance", instanceID, "node", claim.nodeID, "error", fail.Msg)
		return
	}

	// Success — the judge is now the node's responsible actor. node-PATCH's
	// assignee path authorises its done/fail verdict; the original worker can no
	// longer settle (it's no longer the assignee), so it can't self-approve.
	convID := outcome.ConvID
	if _, err := db.UpdateWorkflowNode(instanceID, claim.nodeID, db.WorkflowNodePatch{Assignee: &convID}); err != nil {
		slog.Warn("workflow engine: record judge assignee failed", "instance", instanceID, "node", claim.nodeID, "error", err)
		return
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: claim.nodeID,
		Kind: db.WorkflowEventNodeAwaitingVerify, Message: "engine: ai-verify judge " + convID + " assigned"})
}

// buildVerifyJudgePrompt assembles a judge agent's task brief: the (interpolated)
// acceptance criteria, the node's reported output, and the EXACT report command
// for each verdict. The embedded criteria + output are byte-capped so the brief
// always fits MaxInitialMessageBytes with the report commands intact — a judge
// must never be spawned without its instructions.
func buildVerifyJudgePrompt(instanceID int64, nodeID, label, criteria, output string) string {
	id := strconv.FormatInt(instanceID, 10)
	var b strings.Builder
	b.WriteString("You are an independent verification judge for workflow ")
	b.WriteString(id)
	b.WriteString(`, node "`)
	b.WriteString(label)
	b.WriteString("\".\n\n")
	if strings.TrimSpace(criteria) != "" {
		b.WriteString("Acceptance criteria:\n")
		b.WriteString(headCapBytes(criteria, 4096))
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(output) != "" {
		b.WriteString("The node's reported output:\n")
		b.WriteString(tailCapBytes(output, 8192))
		b.WriteString("\n\n")
	}
	b.WriteString("Judge whether the work meets the criteria — inspect the workspace if you need to confirm. ")
	b.WriteString("Then report your verdict by running EXACTLY ONE command:\n")
	fmt.Fprintf(&b, "  PASS -> tclaude workflow node %s %s done\n", id, nodeID)
	fmt.Fprintf(&b, "  FAIL -> tclaude workflow node %s %s fail\n", id, nodeID)
	b.WriteString("Run one of those, then stop. Do NOT do the node's work yourself — only verify it.")
	return b.String()
}

// headCapBytes keeps the first max bytes of s (criteria reads top-down), with a
// trailing marker when it truncated. tailCapBytes keeps the LAST max bytes (a
// command's verdict/result is at the end), line-clean, with a leading marker.
func headCapBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := s[:max]
	if nl := strings.LastIndexByte(head, '\n'); nl > 0 {
		head = head[:nl]
	}
	return head + "\n[…truncated…]"
}

func tailCapBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	tail := s[len(s)-max:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl+1 < len(tail) {
		tail = tail[nl+1:]
	}
	return "[…truncated…]\n" + tail
}

// ----- stuck-node sweep + escalation (JOH-41 / JOH-8 #9) ----------------------

// sweepStuckNodes is the engine's active stuck-detector. Each node that is
// waiting on someone — a running ai worker, an awaiting_verify ai-verify node, a
// human approve-gate, an idle human-executor node — is given an effective SLA T
// (workflow.EffectiveSLA: its own `sla:` field, else the class default), and as
// its idle time crosses fractions of T it climbs a ladder:
//
//	warn (0.5T)      nudge whoever can act — the assignee agent if one is live
//	                 (the JOH-40 inbox path), otherwise the human.
//	escalate (0.8T)  raise it to the human via the human.notify channel.
//	terminal (1.0T)  for a NON-human node with NO live actor: fail it (the
//	                 original JOH-35 behavior — release its parallelism-cap slot,
//	                 route on_fail / the |fail| edge); for a live agent's node or
//	                 ANY human node: one final urgent notice, never an auto-fail.
//
// This GENERALIZES the old single-tier sweepStuckAINodes: that flip-to-failed is
// now just the terminal rung, reached only after the two softer rungs. One
// detector, not two competing ones.
//
// Idempotency / restart-safety mirror the JOH-40 handoff marker: each fired rung
// writes a durable workflow_events(kind=node_escalation) row whose Message is an
// at-most-once marker keyed on the node's UpdatedAt (the activation key). Within
// one activation UpdatedAt is pinned (appending events / sending messages never
// touches the node row), so a marker is stable and a rung fires once; a retry /
// loop re-arm moves UpdatedAt, rotating the key so the next activation escalates
// fresh. Because idle only grows within an activation, the crossed tier is
// monotonic, so the sweep need only ever fire the single highest-crossed rung it
// has not yet recorded — intermediate rungs skipped after downtime are correctly
// subsumed by the higher one.
//
// Guards preserved from the old sweep: a node with a spawn/claim in flight
// (sentinel assignee) is skipped; the terminal FAIL is gated on no-live-actor
// (a hung-but-online agent stays an operator cancel). Runs under the per-instance
// lock; the gating liveness check is DB-only (convHasRunningSession), and the warn
// rung's assignee nudge shells out to tmux under the lock — the same lock-held
// nudge deliverReadyHandoffs already does, fast and bounded.
func sweepStuckNodes(instanceID int64) {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		return
	}
	nodes, err := db.ListWorkflowNodes(instanceID)
	if err != nil {
		return
	}
	tmpl, _ := rebuildInstanceTemplate(inst)
	if tmpl == nil {
		return
	}
	now := time.Now()
	for _, n := range nodes {
		isHuman, ok := stuckClass(n, tmpl)
		if !ok {
			continue
		}
		// A spawn/claim is in flight (sentinel) — leave it; the off-tick goroutine
		// settles it within executeSpawn's timeout.
		if n.Assignee == engineAssignee {
			continue
		}
		T := workflow.EffectiveSLA(tmpl.Nodes[n.NodeID], isHuman, workflowNodeSLA, workflowHumanNodeSLA)
		crossed := workflow.CrossedTier(now.Sub(n.UpdatedAt), T)
		if crossed == workflow.TierNone {
			continue
		}
		// A live responsible agent never gets its node auto-failed (only nudged);
		// an empty / dead / no-session assignee means no live actor.
		hasLiveAgent := n.Assignee != "" && n.Assignee != engineAssignee && convHasRunningSession(n.Assignee)

		// The terminal rung for a non-human node with no live actor FAILS it. That
		// effect is self-idempotent — settleWorkflowNodeFailed flips status to
		// failed, dropping the node from stuckClass next pass — so it deliberately
		// does NOT use the at-most-once marker: writing a marker first and then
		// crashing before the fail would suppress the fail FOREVER on restart (the
		// marker is permanent, idle only grows), re-wedging the cap slot this sweep
		// exists to free. So the fail just runs; a crash mid-fail simply retries it
		// next tick, exactly like the pre-escalation sweep.
		if crossed == workflow.TierTerminal &&
			workflow.TerminalActionFor(isHuman, hasLiveAgent) == workflow.TermFail {
			settleWorkflowNodeFailed(inst, n.NodeID, tmpl,
				"node idle past SLA with no live agent (status "+n.Status+")", now)
			slog.Warn("workflow engine: failed stuck node (terminal escalation)",
				"instance", instanceID, "node", n.NodeID, "status", n.Status, "assignee", n.Assignee)
			continue
		}

		// Every other rung is a NOTIFICATION (ping the assignee / notify the human),
		// which has no natural idempotency, so it is gated by a durable at-most-once
		// marker. The marker is keyed on BOTH the node's UpdatedAt (the activation
		// clock) AND its Visits counter: UpdatedAt alone is stored at second
		// granularity, so a sub-second per-node sla plus a same-second retry could
		// collide; Visits bumps on every retry / loop re-arm, so the combination
		// always rotates on a genuine re-activation while staying stable across a
		// single idle window.
		marker := escalationMarker(n.NodeID, n.UpdatedAt.UnixNano(), n.Visits, crossed)
		fired := firedEscalationMarkers(instanceID, n.NodeID)
		if fired == nil {
			continue // read error already logged; skip so we never double-fire
		}
		if fired[marker] {
			continue // this (activation, rung) already notified
		}
		// Marker first (at-most-once, like deliverReadyHandoffs): a crash between
		// this and the send costs a recoverable missed nudge, never a duplicate.
		if _, err := db.AppendWorkflowEvent(&db.WorkflowEvent{
			InstanceID: instanceID, NodeID: n.NodeID,
			Kind: db.WorkflowEventNodeEscalation, Message: marker,
		}); err != nil {
			slog.Warn("workflow engine: escalation marker write failed; skipping to avoid double-fire",
				"instance", instanceID, "node", n.NodeID, "tier", crossed.String(), "error", err)
			continue
		}
		notifyEscalation(inst, n, crossed, isHuman, hasLiveAgent, now)
	}
}

// stuckClass reports whether a node is a stuck-escalation candidate and, if so,
// whether we are waiting on a HUMAN (which picks the human SLA default and, at
// the terminal rung, forbids auto-fail). Candidates, by what they wait on:
//
//	running ai worker             -> non-human (a crashed / slow agent)
//	awaiting_verify ai-verify     -> non-human (judge round-trip / cap-starved)
//	awaiting_verify human-verify  -> human     (dashboard approve gate left idle)
//	ready/running human executor  -> human     (a person hasn't done + reported it)
//
// Everything else is excluded: a ready ai/tool/program node is about to auto-run
// (not stuck), and a settled node has no actor to wait on.
func stuckClass(n *db.WorkflowNode, tmpl *workflow.Template) (isHuman, ok bool) {
	switch n.Status {
	case db.WorkflowNodeStatusRunning:
		switch n.ExecutorKind {
		case string(workflow.ExecAI):
			return false, true
		case string(workflow.ExecHuman):
			return true, true
		}
	case db.WorkflowNodeStatusReady:
		// The engine never auto-runs a human node, so a ready one is waiting on a
		// person. A ready ai/tool/program node is the engine's to run next tick.
		if n.ExecutorKind == string(workflow.ExecHuman) {
			return true, true
		}
	case db.WorkflowNodeStatusAwaitingVerify:
		if nodeWantsAIVerify(tmpl, n.NodeID) {
			return false, true
		}
		if nodeWantsHumanVerify(tmpl, n.NodeID) {
			return true, true
		}
	}
	return false, false
}

// nodeWantsHumanVerify reports whether a node's definition-of-done is a human
// approve gate (verify.kind: human). A nil template / missing node → false.
func nodeWantsHumanVerify(tmpl *workflow.Template, nodeID string) bool {
	if tmpl == nil || tmpl.Nodes[nodeID] == nil {
		return false
	}
	return tmpl.Nodes[nodeID].Verify.Kind == workflow.VerifyHuman
}

// escalationMarker is the at-most-once idempotency key for one (node, activation,
// rung) notification, stored as the Message of a workflow_events(node_escalation)
// row. The activation is identified by BOTH the node's UpdatedAt in nanoseconds
// (the idle clock) AND its Visits counter: within one idle window both are stable
// so a rung notifies once; a retry / loop re-arm bumps Visits (and usually
// UpdatedAt) so a genuine re-activation rotates the key and escalates fresh —
// robust even when a sub-second per-node sla and a same-second re-arm would leave
// the second-granular UpdatedAt unchanged.
func escalationMarker(nodeID string, activationNano int64, visits int64, tier workflow.EscalationTier) string {
	return fmt.Sprintf("%s:%d:%d:%s", nodeID, activationNano, visits, tier.String())
}

// firedEscalationMarkers returns the set of escalation-marker strings already
// recorded for one node, or nil on a read error (the caller then skips the node
// rather than risk a double-fire — a missed nudge is recoverable, a duplicate
// spams). A non-nil empty map means "nothing escalated yet".
func firedEscalationMarkers(instanceID int64, nodeID string) map[string]bool {
	events, err := db.ListWorkflowEvents(instanceID, nodeID)
	if err != nil {
		slog.Warn("workflow engine: escalation marker lookup failed; skipping to avoid double-fire",
			"instance", instanceID, "node", nodeID, "error", err)
		return nil
	}
	out := make(map[string]bool, len(events))
	for _, e := range events {
		if e.Kind == db.WorkflowEventNodeEscalation {
			out[e.Message] = true
		}
	}
	return out
}

// notifyEscalation performs the NOTIFICATION effect for one rung (the marker is
// already written; the terminal-FAIL case is handled inline in the sweep, before
// any marker, because it is self-idempotent). warn nudges the actor — the
// assignee agent if one is live, else the human; escalate always goes to the
// human; terminal here is only ever a live-agent or human node (the fail case
// never reaches this point), so it sends a final urgent notice.
func notifyEscalation(inst *db.WorkflowInstance, n *db.WorkflowNode,
	tier workflow.EscalationTier, isHuman, hasLiveAgent bool, now time.Time) {
	idle := now.Sub(n.UpdatedAt)
	switch tier {
	case workflow.TierWarn:
		if hasLiveAgent {
			pingAssigneeOverdue(inst, n, idle)
			return
		}
		notifyHumanStuck(inst, n, tier, isHuman, idle)
	default: // escalate, or terminal-for-a-live-agent/human (never the fail case)
		notifyHumanStuck(inst, n, tier, isHuman, idle)
	}
}

// pingAssigneeOverdue nudges a still-live assignee agent that its node has gone
// past the warn threshold, over the JOH-40 inbox path (agent_message + tmux
// nudge), sender = the <workflow-engine> sentinel.
func pingAssigneeOverdue(inst *db.WorkflowInstance, n *db.WorkflowNode, idle time.Duration) {
	label := nodeLabelOr(n)
	subject := fmt.Sprintf("[workflow] node %q is overdue (%s, no progress)", label, roundIdle(idle))
	body := fmt.Sprintf("Workflow %q (instance %d): you have been assigned node %q for %s without it settling. "+
		"If you are still working it, carry on — this is just a heads-up that it has passed its SLA warn "+
		"threshold. If you are stuck, report your result (done/fail or your verdict) or ask the group for help; "+
		"otherwise it will be escalated to the human.",
		inst.TemplateName, inst.ID, label, roundIdle(idle))
	msgID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:      inst.GroupID,
		FromConv:     workflowHandoffSender,
		ToConv:       n.Assignee,
		Subject:      subject,
		Body:         body,
		ToRecipients: []string{n.Assignee},
	})
	if err != nil {
		slog.Warn("workflow engine: overdue assignee ping failed",
			"instance", inst.ID, "node", n.NodeID, "conv", n.Assignee, "error", err)
		return
	}
	nudgeIfAlive(msgID, n.Assignee)
	slog.Info("workflow engine: pinged overdue assignee",
		"instance", inst.ID, "node", n.NodeID, "conv", n.Assignee, "msg", msgID)
}

// notifyHumanStuck posts a stuck-node notice to the human.notify channel
// (human_messages → the dashboard Messages tab), in-process — the engine is
// trusted daemon code, so it bypasses the HTTP permission gate exactly as
// deliverReadyHandoffs bypasses it for agent messages. Wording scales with the
// rung: warn is a heads-up, escalate raises it, terminal is the final urgent
// notice (used when the node is a human gate or still has a live agent — both
// cases the engine must not auto-fail).
func notifyHumanStuck(inst *db.WorkflowInstance, n *db.WorkflowNode,
	tier workflow.EscalationTier, isHuman bool, idle time.Duration) {
	label := nodeLabelOr(n)
	who := "an assigned agent"
	if isHuman {
		who = "a person"
	}
	var subject, lead string
	switch tier {
	case workflow.TierWarn:
		subject = fmt.Sprintf("[workflow] node %q is waiting (%s)", label, roundIdle(idle))
		lead = "is waiting and has passed its warn threshold"
	case workflow.TierEscalate:
		subject = fmt.Sprintf("[workflow] node %q needs attention (stuck %s)", label, roundIdle(idle))
		lead = "has made no progress and is now escalated to you"
	default: // terminal
		subject = fmt.Sprintf("[workflow] node %q still stuck after %s — your call", label, roundIdle(idle))
		lead = "is still stuck at its SLA limit; the engine will NOT auto-fail it (a human gate or a live agent owns it)"
	}
	body := fmt.Sprintf("Workflow %q (instance %d), node %q [%s], waited on by %s, %s "+
		"(idle %s). Act via the dashboard — approve/reject the gate, nudge or cancel the agent, "+
		"or let it continue.",
		inst.TemplateName, inst.ID, label, n.Status, who, lead, roundIdle(idle))
	if _, err := db.InsertHumanMessage(&db.HumanMessage{
		FromConv:  engineAssignee,
		FromTitle: "workflow engine",
		GroupName: engineInstanceGroupName(inst),
		Subject:   subject,
		Body:      body,
		CreatedAt: time.Now(),
	}); err != nil {
		slog.Warn("workflow engine: human.notify (stuck node) failed",
			"instance", inst.ID, "node", n.NodeID, "tier", tier.String(), "error", err)
		return
	}
	slog.Info("workflow engine: escalated stuck node to human",
		"instance", inst.ID, "node", n.NodeID, "tier", tier.String(), "status", n.Status)
}

// engineInstanceGroupName resolves an instance's linked group name for human
// message attribution ("which project"), or "" when ungrouped / unresolvable.
func engineInstanceGroupName(inst *db.WorkflowInstance) string {
	if inst.GroupID == 0 {
		return ""
	}
	if g, err := db.GetAgentGroupByID(inst.GroupID); err == nil && g != nil {
		return g.Name
	}
	return ""
}

// nodeLabelOr returns a node's human label, falling back to its id.
func nodeLabelOr(n *db.WorkflowNode) string {
	if n.Label != "" {
		return n.Label
	}
	return n.NodeID
}

// roundIdle renders an idle duration for a message, rounded to the second so a
// notice reads "21m0s" rather than a noisy nanosecond tail.
func roundIdle(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}

// ----- handoffs as inbox messages (JOH-40) -----------------------------------

// workflowHandoffSender is the from_conv stamped on engine-delivered handoff
// messages. A predecessor node's agent conv is cleared the moment the node
// settles (settleWorkflowNodeDone/Failed), and tool/program/human predecessors
// never had an agent at all, so there is no live predecessor conv to attribute
// the handoff to — the synthetic sender names the engine as the deliverer. It
// resolves to no title (titleFor → ""), so the recipient's `inbox read` renders
// the raw id; the message subject + body name the actual predecessor node and
// workflow, which is the meaningful attribution. Reuses the engineAssignee
// sentinel so one "<workflow-engine>" identity covers both node.assignee and the
// handoff from_conv.
const workflowHandoffSender = engineAssignee

// deliverReadyHandoffs is the engine's inbox-handoff pass (JOH-40). When a node
// settles and the graph advances, its captured output is delivered to each
// downstream successor's BOUND agent as a normal inbox message (+ a tmux nudge),
// so a workflow's data-flow is visible agent-to-agent traffic over tclaude's
// existing inbox rather than a hidden side channel.
//
// It is a reconciliation pass, NOT an inline hook on advance: each tick it
// re-derives the work from SQLite, so it holds no in-memory state and is correct
// across daemon restarts, late agent binding, and joins — the same stateless
// idiom as the rest of the engine. For every successor node that has a LIVE
// bound agent (status running, a real conv assignee — not empty, not the engine
// sentinel) it walks that node's settled direct predecessors (done/failed — a
// skipped not-taken branch has nothing to hand off) and delivers one handoff per
// predecessor not yet delivered to THIS agent.
//
// A handoff fires only along a TAKEN edge: predecessor P hands to successor S
// iff the P→S edge was followed given P's recorded outcome (workflow.EdgeTaken —
// the same rule the advance uses). So a node that settled `pass` but whose only
// edge into S is a `|fail|` branch never hands to S, even though it is `done` and
// S became ready via another (joinAny) predecessor — data flows only where the
// graph actually routed it.
//
// Idempotency is a per-(predNode→succNode@succConv) marker appended to
// workflow_events (kind "handoff"). The marker is written BEFORE the inbox row,
// so the failure modes collapse to at-most-once: a crash (or an insert error)
// between marker and message yields a missed handoff — recoverable, the agent
// can pull state via `tclaude workflow show` — never a duplicate, which is the
// invariant the design exists to protect. A re-derivation (next tick) or an
// engine restart (markers persist in SQLite) re-runs the pass and the marker
// suppresses the resend; a successor RE-bound under a NEW conv (a future loop
// iteration, a reaper respawn) keys on the new conv and correctly gets a fresh
// handoff.
//
// Joins fall out for free: a bound join-successor has N taken-edge predecessors,
// so it receives N independent handoff messages — one per upstream branch.
//
// Runs under the per-instance lock (consistent with the other passes); the nudge
// goes out via nudgeIfAlive — the same out-of-sandbox tmux delivery the
// cross-agent message path uses. A successor whose agent is offline still gets
// the durable inbox row (read on its next launch); only the live nudge is
// skipped. Gated by engineMayAutoRun, so an engine-off / external instance emits
// nothing.
func deliverReadyHandoffs(instanceID int64) {
	unlock := lockWorkflowInstance(instanceID)
	defer unlock()

	inst, err := db.GetWorkflowInstance(instanceID)
	if err != nil || inst == nil || inst.Status != db.WorkflowStatusRunning {
		return
	}
	if !engineMayAutoRun(inst, nil) {
		return
	}
	nodes, err := db.ListWorkflowNodes(instanceID)
	if err != nil {
		slog.Warn("workflow engine: list nodes failed (handoff pass)", "instance", instanceID, "error", err)
		return
	}
	tmpl, _ := rebuildInstanceTemplate(inst)
	if tmpl == nil {
		return
	}

	byID := make(map[string]*db.WorkflowNode, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	// Taken in-edges per successor: each is a (predecessor → this node) edge that
	// was actually followed given the predecessor's settled outcome. Deduped on
	// predecessor (a node could reach a successor by >1 edge; one handoff suffices).
	takenPreds := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, e := range tmpl.Edges {
		pred := byID[e.From]
		// Only a settled-with-output predecessor hands off: a still-live one has
		// yet to produce its result, a skipped not-taken branch never will.
		if pred == nil ||
			(pred.Status != db.WorkflowNodeStatusDone && pred.Status != db.WorkflowNodeStatusFailed) {
			continue
		}
		if !workflow.EdgeTaken(e, pred.Outcome) {
			continue // data did not flow along this edge
		}
		if seen[e.To] == nil {
			seen[e.To] = map[string]bool{}
		}
		if seen[e.To][e.From] {
			continue
		}
		seen[e.To][e.From] = true
		takenPreds[e.To] = append(takenPreds[e.To], e.From)
	}

	for _, succ := range nodes {
		// Recipient must be a live, bound agent actively working the node.
		if succ.Status != db.WorkflowNodeStatusRunning {
			continue
		}
		toConv := succ.Assignee
		if toConv == "" || toConv == engineAssignee {
			continue // unbound, or an engine claim still in flight
		}
		preds := takenPreds[succ.NodeID]
		if len(preds) == 0 {
			continue
		}
		// One read of this successor's audit events serves every predecessor's
		// idempotency check (instead of re-querying per predecessor).
		delivered := deliveredHandoffMarkers(instanceID, succ.NodeID)
		if delivered == nil {
			continue // read error already logged; skip to avoid a double-send
		}
		for _, predID := range preds {
			marker := handoffMarker(predID, succ.NodeID, toConv)
			if delivered[marker] {
				continue
			}
			// Marker first (see the at-most-once rationale on deliverReadyHandoffs):
			// a crash between this and the insert costs a recoverable miss, never a
			// duplicate. Record it locally too so a later predecessor this pass can't
			// double-send before the next read.
			if _, err := db.AppendWorkflowEvent(&db.WorkflowEvent{
				InstanceID: instanceID, NodeID: succ.NodeID, Kind: db.WorkflowEventHandoff,
				Message: marker,
			}); err != nil {
				slog.Warn("workflow engine: handoff marker write failed; skipping to avoid double-send",
					"instance", instanceID, "from", predID, "to", succ.NodeID, "error", err)
				continue
			}
			delivered[marker] = true

			subject, body := buildHandoffMessage(inst, byID[predID], succ)
			msgID, err := db.InsertAgentMessage(&db.AgentMessage{
				GroupID:      inst.GroupID,
				FromConv:     workflowHandoffSender,
				ToConv:       toConv,
				Subject:      subject,
				Body:         body,
				ToRecipients: []string{toConv},
			})
			if err != nil {
				// The marker is already written, so this handoff won't retry — a
				// recoverable miss, consistent with the at-most-once choice.
				slog.Warn("workflow engine: handoff insert failed (marked; will not retry)",
					"instance", instanceID, "from", predID, "to", succ.NodeID, "error", err)
				continue
			}
			nudgeIfAlive(msgID, toConv)
			slog.Info("workflow engine: delivered handoff",
				"instance", instanceID, "from", predID, "to", succ.NodeID, "conv", toConv, "msg", msgID)
		}
	}
}

// handoffMarker is the idempotency key for one predecessor→successor handoff to
// a specific bound agent, stored as the message of a workflow_events(kind=handoff)
// row. Including the successor conv-id means a successor re-bound to a NEW agent
// (a loop iteration, a respawn) gets a fresh handoff rather than being suppressed.
func handoffMarker(predID, succID, toConv string) string {
	return predID + "->" + succID + "@" + toConv
}

// deliveredHandoffMarkers returns the set of handoff-marker strings already
// recorded for one successor node, or nil on a read error (the caller skips the
// node rather than risk a double-send — a missed handoff is recoverable via
// `tclaude workflow show`, a duplicate spams the inbox). A non-nil empty map
// means "no handoff delivered yet".
func deliveredHandoffMarkers(instanceID int64, succID string) map[string]bool {
	events, err := db.ListWorkflowEvents(instanceID, succID)
	if err != nil {
		slog.Warn("workflow engine: handoff marker lookup failed; skipping to avoid double-send",
			"instance", instanceID, "node", succID, "error", err)
		return nil
	}
	out := make(map[string]bool, len(events))
	for _, e := range events {
		if e.Kind == db.WorkflowEventHandoff {
			out[e.Message] = true
		}
	}
	return out
}

// buildHandoffMessage assembles the subject + body of a predecessor→successor
// handoff: orientation (which workflow/instance, which upstream node finished
// with what outcome, which node the recipient owns) then the predecessor's
// captured output, tail-capped so a chatty build log can't blow past the inbox.
// Plain prose — the recipient is an agent reading its inbox, not a shell — so no
// interpolation/escaping is needed beyond the cap (the output is already the
// captured final value).
func buildHandoffMessage(inst *db.WorkflowInstance, pred, succ *db.WorkflowNode) (subject, body string) {
	id := strconv.FormatInt(inst.ID, 10)
	predLabel := pred.Label
	if predLabel == "" {
		predLabel = pred.NodeID
	}
	succLabel := succ.Label
	if succLabel == "" {
		succLabel = succ.NodeID
	}
	subject = fmt.Sprintf("[workflow %s] handoff: %s → %s", id, pred.NodeID, succ.NodeID)

	outcome := pred.Outcome
	if outcome == "" {
		outcome = "done"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow #%s", id)
	if name := strings.TrimSpace(inst.TemplateName); name != "" {
		fmt.Fprintf(&b, " (%s)", name)
	}
	b.WriteString(" handoff.\n\n")
	fmt.Fprintf(&b, "Upstream node %q (%s) finished with outcome %q.\n", predLabel, pred.NodeID, outcome)
	fmt.Fprintf(&b, "You are working node %q (%s).\n\n", succLabel, succ.NodeID)
	if out := strings.TrimSpace(pred.Output); out != "" {
		fmt.Fprintf(&b, "--- output of %s ---\n", pred.NodeID)
		b.WriteString(tailCapBytes(out, 8192))
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "(node %s reported no captured output.)\n", pred.NodeID)
	}
	fmt.Fprintf(&b, "\nFull instance state: tclaude workflow show %s", id)
	return subject, b.String()
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
// lock only for the claim — released before the command runs. attempted holds
// node ids already run THIS tick (so a node re-armed to ready mid-tick by an
// in-place retry / loop-back is skipped until the next tick — JOH-39 tick-pacing).
//
// Visit cap (JOH-39): before claiming, a node whose Visits has reached its
// effective MaxVisits is NOT run — it is force-failed ("max_visits exceeded")
// and the instance halts, so a runaway loop can't spin. Otherwise Visits is
// bumped as the node goes running, the single counter that bounds loop
// iterations AND in-place retries.
func claimNextNode(instanceID int64, attempted map[string]bool) *nodeClaim {
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
	node := nextRunnableNode(inst, nodes, attempted)
	if node == nil {
		return nil
	}
	tmpl, terr := rebuildInstanceTemplate(inst)
	if terr != nil || tmpl == nil || tmpl.Nodes[node.NodeID] == nil {
		slog.Warn("workflow engine: cannot rebuild node def; skipping", "instance", instanceID, "node", node.NodeID, "error", terr)
		return nil
	}
	def := tmpl.Nodes[node.NodeID]

	// Visit cap: refuse to run a node that has already used its execution budget.
	// Force-fail + halt rather than route on_fail (a loop node's |fail| edge would
	// just loop again — defeating the guard). This is the one rule that bounds
	// every re-execution path (loop-backs + in-place retries both go through here).
	if cap, unbounded := workflow.EffectiveMaxVisits(def, workflowMaxVisits); !unbounded && node.Visits >= int64(cap) {
		settleWorkflowNodeMaxVisits(inst, node.NodeID, cap, time.Now())
		return nil
	}

	// Stamp the engine-owner sentinel alongside running, so the startup reaper
	// can recognise a node THIS engine claimed (vs one a human manually drove to
	// running) and only reap its own corpses after a crash. startedAt uses the
	// real wall clock at the claim, not the tick's start, so a drained chain
	// gets truthful per-node timestamps. Visits++ counts this execution.
	running := db.WorkflowNodeStatusRunning
	startedAt := time.Now()
	owner := engineAssignee
	visits := node.Visits + 1
	if _, err := db.UpdateWorkflowNode(instanceID, node.NodeID,
		db.WorkflowNodePatch{Status: &running, StartedAt: &startedAt, Assignee: &owner, Visits: &visits}); err != nil {
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
		// A tool/program node whose verify.kind is ai or human defers its verdict
		// (RunVerifier returns Defer for those). Park it in awaiting_verify AND
		// clear the engine-owner sentinel the claim stamped: an empty assignee on
		// an awaiting_verify node is the "ready to verify" marker — the ai-verify
		// judge pass claims it, and a human-verify node is left for the dashboard
		// approve gate. (Leaving the sentinel would both miscount it as a judge in
		// flight and confuse the manual PATCH guard.)
		awaiting := db.WorkflowNodeStatusAwaitingVerify
		cleared := ""
		_, _ = db.UpdateWorkflowNode(instanceID, claim.nodeID, db.WorkflowNodePatch{Status: &awaiting, Assignee: &cleared})
		return false
	}
	if !verdict.Done {
		// In-place retry (JOH-39): a node that failed its OWN verify re-runs in
		// place, up to def.Retries times, BEFORE it settles failed and emits its
		// fail outcome (which a |fail| back-edge may then loop on — the outer loop).
		// Re-arm to ready (clear the sentinel) + record a node_retry; the next TICK
		// re-runs it (tick-paced — the attempted-this-tick set keeps it from
		// re-running now). Returns true so the drain keeps serving OTHER ready nodes.
		if retriesUsedThisActivation(inst.ID, claim.nodeID) < claim.def.Retries {
			rearmForRetry(inst.ID, claim.nodeID, verdict.Err)
			return true
		}
		settleWorkflowNodeFailed(inst, claim.nodeID, tmpl, verdict.Err, now)
		return true
	}
	settleWorkflowNodeDone(inst, claim.nodeID, tmpl, verdict.Outcome, now)
	return true
}

// retriesUsedThisActivation counts how many in-place retries a node has already
// used in its CURRENT activation — the number of node_retry events since the
// last fresh-activation boundary (node_ready from advance, or node_reentry from
// a loop-back). Events-derived so it needs no schema column and survives a daemon
// restart (mirrors JOH-40's event-as-durable-state idiom). A read error degrades
// to 0 (treat as fresh) — at worst one extra retry, never a stuck node.
func retriesUsedThisActivation(instanceID int64, nodeID string) int {
	events, err := db.ListWorkflowEvents(instanceID, nodeID)
	if err != nil {
		return 0
	}
	n := 0
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind {
		case db.WorkflowEventNodeRetry:
			n++
		case db.WorkflowEventNodeReady, db.WorkflowEventNodeReentry:
			return n // reached the start of this activation
		}
	}
	return n
}

// rearmForRetry re-arms a tool/program node for an in-place retry: status →
// ready, the engine sentinel cleared, and a node_retry event recorded (the
// counter retriesUsedThisActivation reads). The next tick re-runs it; the visit
// cap at claim still bounds the absolute count. The caller holds the instance lock.
func rearmForRetry(instanceID int64, nodeID, reason string) {
	ready := db.WorkflowNodeStatusReady
	cleared := ""
	_, _ = db.UpdateWorkflowNode(instanceID, nodeID, db.WorkflowNodePatch{Status: &ready, Assignee: &cleared})
	msg := "engine: verify failed, retrying in place"
	if reason != "" {
		msg += ": " + reason
	}
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: instanceID, NodeID: nodeID,
		Kind: db.WorkflowEventNodeRetry, Message: msg})
}

// nextRunnableNode returns the first ready node whose executor the engine runs
// synchronously (tool/program) AND that the engine is allowed to auto-run.
// ai/human ready nodes are skipped — they are driven by start/attach + the
// dashboard, not the engine (slice A). Insertion order (chart order) gives a
// stable, predictable pick.
func nextRunnableNode(inst *db.WorkflowInstance, nodes []*db.WorkflowNode, attempted map[string]bool) *db.WorkflowNode {
	for _, n := range nodes {
		if n.Status != db.WorkflowNodeStatusReady || attempted[n.NodeID] {
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

// nodeWantsAIVerify reports whether a node's definition-of-done is an AI judge
// (verify.kind: ai). Used by the node-PATCH path to decide whether a worker's
// done-report parks for ai-verify, and by the engine's judge pass / stuck sweep
// to recognise verify nodes. A nil template / missing node → false.
func nodeWantsAIVerify(tmpl *workflow.Template, nodeID string) bool {
	if tmpl == nil || tmpl.Nodes[nodeID] == nil {
		return false
	}
	return tmpl.Nodes[nodeID].Verify.Kind == workflow.VerifyAI
}

// aiVerifyCanRun reports whether the engine is in a position to run the ai-verify
// judge round-trip for an instance: the engine must be allowed to auto-run on it
// (opt-in + not external) AND it must have a bound group to spawn the judge into.
// When false, an ai-verify node does NOT park — the worker's done-report settles
// the node directly (the slice-B self-report fallback), so a dashboard-only /
// engine-off / unbound instance keeps completing instead of stranding the node
// in awaiting_verify with no judge ever coming.
func aiVerifyCanRun(inst *db.WorkflowInstance) bool {
	return inst != nil && inst.GroupID != 0 && engineMayAutoRun(inst, nil)
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
	applyWorkflowAdvance(inst.ID, nodeID, tmpl, advanced, now)
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
	applyWorkflowAdvance(inst.ID, nodeID, tmpl, advanced, now)
	recomputeAndPersistInstanceStatus(inst, tmpl)
}

// settleWorkflowNodeMaxVisits fails a node that has exhausted its visit budget
// and HALTS the instance — it deliberately does NOT advance. A loop node carries
// on_fail: continue + a |fail| back-edge, so routing the failure normally would
// just re-enter the loop, defeating the guard. The visit cap is the hard stop on
// a runaway loop, so it forces the instance to failed directly rather than
// letting FailHalts (false for a loop node) keep it running. (JOH-39)
func settleWorkflowNodeMaxVisits(inst *db.WorkflowInstance, nodeID string, cap int, now time.Time) {
	failed := db.WorkflowNodeStatusFailed
	fin := now
	oc := workflow.OutcomeFail
	cleared := ""
	_, _ = db.UpdateWorkflowNode(inst.ID, nodeID, db.WorkflowNodePatch{Status: &failed, Outcome: &oc, FinishedAt: &fin, Assignee: &cleared})
	_, _ = db.AppendWorkflowEvent(&db.WorkflowEvent{InstanceID: inst.ID, NodeID: nodeID,
		Kind: db.WorkflowEventNodeFailed, Message: fmt.Sprintf("engine: max_visits exceeded (cap %d) — halting instance", cap)})
	// Force the instance to failed: do NOT advance (no loop re-entry), and bypass
	// recompute's FailHalts (which is false for an on_fail:continue loop node).
	if inst.Status == db.WorkflowStatusRunning {
		if _, err := db.UpdateWorkflowInstanceStatus(inst.ID, db.WorkflowStatusFailed); err == nil {
			appendInstanceStatusEvent(inst.ID, db.WorkflowStatusFailed)
			inst.Status = db.WorkflowStatusFailed
		}
	}
	slog.Warn("workflow engine: node hit max_visits; instance failed", "instance", inst.ID, "node", nodeID, "cap", cap)
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
