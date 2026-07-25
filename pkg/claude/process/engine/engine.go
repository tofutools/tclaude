package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Initialize creates the exact pre-execution v2 state from one prepared
// definition. It performs no implicit advancement: the entry node is the sole
// ready node and every authored edge begins unresolved. As a creation boundary
// it runs the full structural ValidateCheckpoint once on the constructed state;
// the per-transition runtime cycle then trusts that boundary and does not.
//
// The entry node is whichever node the template's start names — an explicit
// start node is optional. Its own kind decides what "ready" means, exactly as
// it does for any node the reducer later activates: a task becomes plannable, a
// parallel fork or end node becomes engine-owned, and a decision is parked on a
// human, so it takes its durable obligation here rather than only when a
// settlement pass activates it.
func Initialize(runID string, definition *Definition) (Checkpoint, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	nodes := make(map[string]NodeStatus, len(definition.nodes))
	for _, node := range definition.nodes {
		nodes[node.id] = NodePending
	}
	entry := definition.nodes[0]
	nodes[entry.id] = NodeReady
	edges := make(map[string]map[string]EdgeDisposition)
	for _, edge := range definition.edges {
		if edges[edge.from] == nil {
			edges[edge.from] = make(map[string]EdgeDisposition)
		}
		edges[edge.from][edge.outcome] = EdgeUnresolved
	}
	checkpoint := Checkpoint{
		Version: CheckpointVersion,
		RunID:   runID,
		Status:  RunRunning,
		Nodes:   nodes,
		Edges:   edges,
	}
	if entry.kind == definitionDecision {
		addObligation(&checkpoint, entry.id)
	}
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

// AdvanceAndPlan commits every engine-owned transition that is currently
// possible and then plans at most one program command: the deterministically
// first ready program task in prepared topological order. It is the single
// operation a driver needs, and the only planning entry point.
//
// With fan-out, several tasks can be ready at once, so a singular plan can no
// longer name "the" task. This deliberately stays a *next*-plan operation
// rather than a batch one: it commits one command per call. A bounded
// concurrent driver refills up to its own capacity by calling AdvanceAndPlan
// repeatedly on the returned checkpoint — the previously planned task is
// Running by then, so each call picks the next ready one — and needs no change
// to the durable shape, because Commands is already a plural outbox.
//
// advanced reports whether anything at all was committed, so a caller can skip
// a no-op persist on a resume. A non-nil command always implies advanced.
//
// It runs no whole-checkpoint validation: callers pass state already validated
// once at the load (DecodeCheckpoint) or creation (Initialize) boundary. The
// cheap prepared-definition guard just avoids a nil dereference.
func AdvanceAndPlan(checkpoint Checkpoint, definition *Definition) (Checkpoint, *Command, bool, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, nil, false, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	next, committed, err := advanceUntilQuiescent(checkpoint, definition, transitionBudget(definition))
	if err != nil {
		return Checkpoint{}, nil, false, err
	}
	node, ok := nextPlannableTask(next, definition)
	if !ok {
		return next, nil, committed > 0, nil
	}
	command := programCommand(next.RunID, node, nextAttempt(next, node.id))
	planned, err := apply(next, definition, Transition{Kind: TransitionCommandPlanned, Command: &command})
	if err != nil {
		return Checkpoint{}, nil, false, err
	}
	dispatched := cloneCommand(command)
	return planned, &dispatched, true, nil
}

// Runnable reports whether the engine can still commit a transition without new
// external input: an engine-owned advance, or a ready program task to plan.
//
// A driver needs this to decide whether a run has work it can push. Under
// fan-out that question is independent of whether some branch is awaiting a
// decision: one branch parked on a human must not stop another branch's task
// from being planned, or that branch would be stranded until an unrelated event
// resumed the run.
func Runnable(checkpoint Checkpoint, definition *Definition) bool {
	if definition == nil || len(definition.nodes) == 0 {
		return false
	}
	if _, ok := nextEngineTransition(checkpoint, definition); ok {
		return true
	}
	_, ok := nextPlannableTask(checkpoint, definition)
	return ok
}

// nextPlannableTask returns the deterministically first ready program task. A
// ready task never already carries a command — planning moves it to Running —
// so no outbox scan is needed here.
func nextPlannableTask(checkpoint Checkpoint, definition *Definition) (definitionNode, bool) {
	if checkpoint.Status != RunRunning || hasFailedNode(checkpoint) {
		return definitionNode{}, false
	}
	for _, node := range definition.nodes {
		if node.kind == definitionTask && checkpoint.Nodes[node.id] == NodeReady {
			return node, true
		}
	}
	return definitionNode{}, false
}

// Apply is the side-effect-free reducer. Input maps and slices are cloned, so a
// rejected transition never mutates the caller's state. It performs no
// whole-checkpoint validation on entry or exit: callers pass state validated
// once at the load (DecodeCheckpoint) or creation (Initialize) boundary, and
// each transition preserves validity through cheap local preconditions and
// monotonic guards. Engine-generated states are asserted against the strict
// ClassifyCheckpoint diagnostic in tests, not in this production hot path.
func Apply(checkpoint Checkpoint, definition *Definition, transition Transition) (Checkpoint, error) {
	return apply(checkpoint, definition, transition)
}

func apply(checkpoint Checkpoint, definition *Definition, transition Transition) (Checkpoint, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	next := cloneCheckpoint(checkpoint)
	invalid := func(format string, args ...any) (Checkpoint, error) {
		return Checkpoint{}, fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
	}

	switch transition.Kind {
	case TransitionAdvance:
		if transition.Command != nil || transition.Observation != nil || transition.Decision != nil {
			return invalid("advance transition cannot carry a payload")
		}
		// Advance is node-addressed. Several nodes can be ready at once under
		// fan-out, so the reducer can no longer infer "the" active node; the
		// caller names exactly which engine-owned node it is advancing.
		index, ok := definition.index[transition.NodeID]
		if !ok {
			return invalid("advance names unknown node %q", transition.NodeID)
		}
		node := definition.nodes[index]
		if status := next.Nodes[node.id]; status != NodeReady {
			return invalid("advance requires ready node %q; got %q", node.id, status)
		}
		switch node.kind {
		case definitionStart:
			if err := completeAndSettle(&next, definition, index, arrivesAt(soleOutcome(definition, node))); err != nil {
				return Checkpoint{}, err
			}
		case definitionParallel:
			// A ready parallel fork is engine-owned: completing it settles ALL of
			// its authored outgoing edges as arrived, so every branch activates
			// independently in this one pass.
			if err := completeAndSettle(&next, definition, index, arrivesAtEveryBranch()); err != nil {
				return Checkpoint{}, err
			}
		case definitionEnd:
			if err := requireNoWorkRemains(next, definition, node.id); err != nil {
				return Checkpoint{}, err
			}
			next.Nodes[node.id] = NodeDone
			next.Status = node.terminal
		default:
			return invalid("ready node %q requires a planned command or decision", node.id)
		}
	case TransitionCommandPlanned:
		if transition.Command == nil || transition.Observation != nil || transition.Decision != nil {
			return invalid("command_planned requires only a command payload")
		}
		// The command names its own node, so planning is node-addressed too.
		index, ok := definition.index[transition.Command.NodeID]
		if !ok || definition.nodes[index].kind != definitionTask {
			return invalid("command_planned requires a prepared program task; got %q", transition.Command.NodeID)
		}
		node := definition.nodes[index]
		if status := next.Nodes[node.id]; status != NodeReady {
			return invalid("command_planned requires ready task %q; got %q", node.id, status)
		}
		// The planned command must be the deterministic request for the node's
		// NEXT attempt. Binding the attempt into the comparison is what makes
		// replaying an already-committed plan fail closed instead of re-running
		// the same identity: the counter has moved on, so the old command no
		// longer matches anything the reducer would mint.
		attempt := nextAttempt(next, node.id)
		if attempt > node.maxAttempts {
			return invalid("node %q has no attempt %d within its authored budget of %d", node.id, attempt, node.maxAttempts)
		}
		expected := programCommand(next.RunID, node, attempt)
		if !commandsEqual(*transition.Command, expected) {
			return invalid("planned command does not match deterministic command for node %q", node.id)
		}
		next.Nodes[node.id] = NodeRunning
		// The counter advances with the outbox entry, in this one step: a durable
		// command always names the node's current attempt.
		recordAttempt(&next, node.id, attempt)
		// Plural outbox: only this node's entry is written. Sibling branches keep
		// whatever they had outstanding.
		putCommand(&next, cloneCommand(expected))
	case TransitionProgramObserved:
		if transition.Observation == nil || transition.Command != nil || transition.Decision != nil {
			return invalid("program_observed requires only an observation payload")
		}
		observation := transition.Observation
		// Exact identity: an observation binds to the one outbox entry carrying
		// both its command id and its node id. Anything else — a duplicate for an
		// already consumed command, a forged id, or another branch's command — is
		// refused without touching state.
		outstanding, ok := findCommand(next, observation.CommandID, observation.NodeID)
		if !ok {
			return Checkpoint{}, fmt.Errorf("%w: observation does not match an outstanding command", ErrStaleObservation)
		}
		index, ok := definition.index[outstanding.NodeID]
		if !ok || definition.nodes[index].kind != definitionTask {
			return invalid("outstanding command node %q is not prepared", outstanding.NodeID)
		}
		node := definition.nodes[index]
		switch observation.Outcome {
		case ProgramSucceeded:
			if observation.ExitCode != 0 {
				return invalid("successful program observation must have exit code 0")
			}
			if observation.Error != "" {
				return invalid("successful program observation cannot carry an error")
			}
			// Per-entry removal only: sibling branches' commands are untouched.
			removeCommand(&next, outstanding.NodeID)
			if err := completeAndSettle(&next, definition, index, arrivesAt(soleOutcome(definition, node))); err != nil {
				return Checkpoint{}, err
			}
			// A branch that succeeded after a sibling already failed still
			// settles its own node — that observation is real and is honest
			// evidence — but it cannot revive the run: planning is already shut
			// off, settling must not park the run on a new human decision, and
			// this is the transition that may empty the last outbox entry and
			// let the failed run finalize.
			abandonAfterFailure(&next)
		case ProgramFailed:
			// Per-entry removal only, exactly like a success: a failing branch
			// must never erase a sibling command whose program is still
			// running. The run therefore stays RunRunning until its durable
			// outbox drains, so those siblings remain individually observable
			// or reconcilable and a crash mid-drain cold-loads them honestly.
			removeCommand(&next, outstanding.NodeID)
			// Retry is authored policy, and it is branch-local: a failed attempt
			// still inside its budget re-readies only THIS node, so the next
			// planning pass mints a fresh attempt for it while every sibling
			// keeps exactly what it had. A task with no authored retry has a
			// budget of one and therefore stays fail-fast.
			if next.Attempts[node.id] < node.maxAttempts {
				next.Nodes[node.id] = NodeReady
				break
			}
			// Budget exhausted: today's failure disposition, unchanged. Planning
			// stops immediately — hasFailedNode shuts off both nextPlannableTask
			// and nextEngineTransition.
			next.Nodes[node.id] = NodeFailed
			abandonAfterFailure(&next)
		default:
			return invalid("unknown program outcome %q", observation.Outcome)
		}
	case TransitionDecisionRecorded:
		if transition.Decision == nil || transition.Command != nil || transition.Observation != nil {
			return invalid("decision_recorded requires only a decision payload")
		}
		decision := transition.Decision
		// Exact identity again: the verdict resolves one named obligation out of
		// the plural outbox, so a stale or duplicate verdict for an already
		// decided node is refused while other branches stay awaited.
		if !hasObligation(next, decision.NodeID) {
			return Checkpoint{}, fmt.Errorf("%w: run is not awaiting a decision for node %q", ErrStaleDecision, decision.NodeID)
		}
		index := definition.index[decision.NodeID]
		node := definition.nodes[index]
		if !verdictAllowed(node, decision.Verdict) {
			return Checkpoint{}, fmt.Errorf("%w: verdict %q is not an authored outcome of decision %q", ErrInvalidDecisionVerdict, decision.Verdict, node.id)
		}
		// Per-entry removal only: other branches keep awaiting their own decisions.
		removeObligation(&next, decision.NodeID)
		if err := completeAndSettle(&next, definition, index, arrivesAt(decision.Verdict)); err != nil {
			return Checkpoint{}, err
		}
	default:
		return invalid("unknown transition kind %q", transition.Kind)
	}
	return next, nil
}

// requireNoWorkRemains refuses to terminate a run while any other work is still
// live. With join: any a run really does reach its end node while losing
// branches are still executing, so the WAIT lives in nextEngineTransition,
// which simply does not offer such an end node yet. This stays as the
// fail-loud assertion at the single terminating transition of a run: a
// semantics bug that got past that gate stops here instead of silently
// reporting a terminal run with branches in flight. It is not a
// per-transition whole-graph proof.
func requireNoWorkRemains(checkpoint Checkpoint, definition *Definition, endNodeID string) error {
	kind, nodeID, remains := remainingWork(checkpoint, definition, endNodeID)
	if !remains {
		return nil
	}
	return fmt.Errorf("%w: end %q cannot complete while node %q %s",
		ErrInvalidTransition, endNodeID, nodeID, kind)
}

// remainingWork names one live unit of work other than the given end node: an
// outstanding command, an awaited decision, or an active node. It is a bounded
// scan of the durable outboxes and the node-status map — no edges, no graph
// walk — and it is consulted only when a ready end node is actually in hand.
func remainingWork(checkpoint Checkpoint, definition *Definition, endNodeID string) (kind, nodeID string, remains bool) {
	if len(checkpoint.Commands) > 0 {
		return "still has an outstanding command", checkpoint.Commands[0].NodeID, true
	}
	if len(checkpoint.AwaitingDecisions) > 0 {
		return "is still awaiting a decision", checkpoint.AwaitingDecisions[0].NodeID, true
	}
	for _, other := range definition.nodes {
		if other.id == endNodeID {
			continue
		}
		if status := checkpoint.Nodes[other.id]; status == NodeReady || status == NodeRunning {
			return "is " + string(status), other.id, true
		}
	}
	return "", "", false
}

// outgoingArrivals selects which authored outgoing edges of a completing node
// arrive. Routing nodes — start, program tasks, decisions — take exactly one
// authored outcome; an engine-owned parallel fork takes every branch at once.
type outgoingArrivals struct {
	everyBranch bool
	outcome     string
}

func arrivesAt(outcome string) outgoingArrivals { return outgoingArrivals{outcome: outcome} }

func arrivesAtEveryBranch() outgoingArrivals { return outgoingArrivals{everyBranch: true} }

func (a outgoingArrivals) disposition(outcome string) EdgeDisposition {
	if a.everyBranch || outcome == a.outcome {
		return EdgeArrived
	}
	return EdgeNotTaken
}

// completeAndSettle finishes one active node by settling its outgoing edges —
// one arrival for a routing node, every branch for a parallel fork — then
// propagates closure and activation through the affected subgraph.
//
// A join: any reducer activates on its FIRST arrival. Every other target is
// only ever considered once its COMPLETE candidate input set has settled, and
// at that point:
//   - zero arrivals: the target is skipped, and closing its own outgoing edges
//     recursively propagates the closure;
//   - a join: all reducer with one or more arrivals: ready;
//   - a non-join node with exactly one arrival: ready (the local-merge rule);
//   - a non-join node with more than one arrival: a local fail-closed
//     ErrInvalidTransition, since exclusive routing was violated.
//
// Only a join: any reducer can therefore still receive settlements after it
// activated. Those cannot reach it: a target that is no longer pending is
// skipped below, so the winner is never replaced and the reducer never
// activates its downstream route twice.
//
// Several targets can activate in one pass, so several nodes can be ready or
// running simultaneously. Each affected node's incoming edge list is scanned at
// most once and each settled edge does constant bookkeeping afterwards, so a
// settlement pass stays linear in the affected nodes and edges.
func completeAndSettle(next *Checkpoint, definition *Definition, index int, arrivals outgoingArrivals) error {
	node := definition.nodes[index]
	// Monotonic guard: only an active node may complete. Final states never
	// regress or reactivate, so a caller that lost track of node status fails
	// closed here rather than silently rewriting a settled node.
	if status := next.Nodes[node.id]; status != NodeReady && status != NodeRunning {
		return fmt.Errorf("%w: node %q is not active and cannot complete", ErrInvalidTransition, node.id)
	}
	next.Nodes[node.id] = NodeDone

	touched := make(map[int]*edgeCounts)
	queue := make([]int, 0, len(node.outgoing))
	// settleEdge enforces edge monotonicity: an authored edge settles exactly
	// once, so a re-settlement attempt fails closed instead of overwriting a
	// committed disposition.
	settleEdge := func(edgeIndex int, disposition EdgeDisposition) error {
		edge := definition.edges[edgeIndex]
		if next.Edges[edge.from][edge.outcome] != EdgeUnresolved {
			return fmt.Errorf("%w: edge %q/%q is already settled", ErrInvalidTransition, edge.from, edge.outcome)
		}
		// A join: any reducer is won by the first arrival that settles into it,
		// and only by that one. Every later arrival is real evidence that the
		// branch got there — so it is not not_taken — but it is late. Deciding
		// this from the target's own inbound edges rather than from its node
		// status is what makes it hold WITHIN a single settlement pass too: a
		// fork with two branch edges straight into its reducer settles both
		// before either is dequeued.
		if disposition == EdgeArrived && definition.nodes[edge.toIndex].joinAny &&
			joinAlreadyWon(*next, definition, edge.toIndex) {
			disposition = EdgeArrivedLate
		}
		next.Edges[edge.from][edge.outcome] = disposition
		if counts := touched[edge.toIndex]; counts != nil {
			counts.unresolved--
			switch disposition {
			case EdgeArrived:
				counts.arrived++
			case EdgeArrivedLate:
				counts.arrivedLate++
			}
		}
		queue = append(queue, edge.toIndex)
		return nil
	}
	for _, edgeIndex := range node.outgoing {
		if err := settleEdge(edgeIndex, arrivals.disposition(definition.edges[edgeIndex].outcome)); err != nil {
			return err
		}
	}
	for len(queue) > 0 {
		targetIndex := queue[0]
		queue = queue[1:]
		target := definition.nodes[targetIndex]
		if next.Nodes[target.id] != NodePending {
			continue
		}
		counts := touched[targetIndex]
		if counts == nil {
			scanned := countDispositions(*next, definition, target.incoming)
			counts = &scanned
			touched[targetIndex] = counts
		}
		// join: any is the one activation rule that does not wait for the
		// complete candidate set: the first arrival activates the reducer and
		// its downstream route, and the branches still in flight settle later
		// as late arrivals against a node that is no longer pending.
		if target.joinAny && counts.arrived > 0 {
			next.Nodes[target.id] = NodeReady
			if target.kind == definitionDecision {
				addObligation(next, target.id)
			}
			continue
		}
		if counts.unresolved > 0 {
			continue
		}
		switch {
		case counts.arrived == 0:
			next.Nodes[target.id] = NodeSkipped
			for _, edgeIndex := range target.outgoing {
				if err := settleEdge(edgeIndex, EdgeNotTaken); err != nil {
					return err
				}
			}
		case target.joinAll || counts.arrived == 1:
			next.Nodes[target.id] = NodeReady
			if target.kind == definitionDecision {
				addObligation(next, target.id)
			}
		default:
			return fmt.Errorf("%w: node %q received more than one arrival", ErrInvalidTransition, target.id)
		}
	}
	return nil
}

// The plural outboxes are kept sorted by node id so a given logical state has
// exactly one durable encoding, which keeps checkpoints and their evidence
// comparable regardless of the order branches happened to settle in. Both
// outboxes are bounded by the prepared node count, so these stay trivial.

func putCommand(checkpoint *Checkpoint, command Command) {
	for i := range checkpoint.Commands {
		if checkpoint.Commands[i].NodeID == command.NodeID {
			checkpoint.Commands[i] = command
			return
		}
	}
	checkpoint.Commands = append(checkpoint.Commands, command)
	slices.SortFunc(checkpoint.Commands, func(a, b Command) int { return strings.Compare(a.NodeID, b.NodeID) })
}

func findCommand(checkpoint Checkpoint, commandID, nodeID string) (Command, bool) {
	for _, command := range checkpoint.Commands {
		if command.ID == commandID && command.NodeID == nodeID {
			return command, true
		}
	}
	return Command{}, false
}

func removeCommand(checkpoint *Checkpoint, nodeID string) {
	checkpoint.Commands = slices.DeleteFunc(checkpoint.Commands, func(command Command) bool {
		return command.NodeID == nodeID
	})
	if len(checkpoint.Commands) == 0 {
		checkpoint.Commands = nil
	}
}

func addObligation(checkpoint *Checkpoint, nodeID string) {
	if hasObligation(*checkpoint, nodeID) {
		return
	}
	checkpoint.AwaitingDecisions = append(checkpoint.AwaitingDecisions, DecisionObligation{NodeID: nodeID})
	slices.SortFunc(checkpoint.AwaitingDecisions, func(a, b DecisionObligation) int {
		return strings.Compare(a.NodeID, b.NodeID)
	})
}

func hasObligation(checkpoint Checkpoint, nodeID string) bool {
	return slices.ContainsFunc(checkpoint.AwaitingDecisions, func(obligation DecisionObligation) bool {
		return obligation.NodeID == nodeID
	})
}

func removeObligation(checkpoint *Checkpoint, nodeID string) {
	checkpoint.AwaitingDecisions = slices.DeleteFunc(checkpoint.AwaitingDecisions,
		func(obligation DecisionObligation) bool { return obligation.NodeID == nodeID })
	if len(checkpoint.AwaitingDecisions) == 0 {
		checkpoint.AwaitingDecisions = nil
	}
}

// Failing reports whether this run has already exhausted a task's attempts and
// can therefore only drain: no further command will ever be planned for it.
//
// It is deliberately distinct from "an attempt failed". With authored retries a
// failed observation is routinely just the end of one attempt, so a caller that
// wants to act on a run being over — cancelling sibling programs, say — has to
// ask about the run rather than about the observation it just committed.
func Failing(checkpoint Checkpoint) bool { return hasFailedNode(checkpoint) }

// hasFailedNode reports whether any program task has already failed.
//
// A failed task fails the whole run, but with several commands in flight the
// run cannot become terminal at that instant — the siblings still owe
// observations. This predicate is what stops the engine from planning or
// advancing anything more in the meantime, so the failed run drains rather than
// grows. It is a bounded scan of the node-status map (one entry per prepared
// node), not a graph walk, and it reads no edges.
func hasFailedNode(checkpoint Checkpoint) bool {
	for _, status := range checkpoint.Nodes {
		if status == NodeFailed {
			return true
		}
	}
	return false
}

// abandonAfterFailure is TERMINAL CLEANUP, not ordinary cross-branch clearing.
//
// The distinction matters and is deliberate. Ordinary reducer mutations are
// strictly per-entry — planning, observing, and deciding each touch exactly one
// branch's entry — because a live run must never let one branch invalidate
// another's outstanding work. Nothing here removes a COMMAND: a command names a
// program that may still be running, and its outcome has to be accounted for
// before the run can be called over.
//
// Awaited decisions are the opposite case and are abandoned at once. The run is
// already destined to fail, so accepting further human work for it would be
// dishonest; dropping the obligations is also what makes a later verdict fail
// closed as stale, and what keeps a crash mid-drain from cold-loading a
// decision nobody should answer.
//
// The run itself becomes terminal only when the last command has drained.
func abandonAfterFailure(checkpoint *Checkpoint) {
	if !hasFailedNode(*checkpoint) {
		return
	}
	checkpoint.AwaitingDecisions = nil
	if len(checkpoint.Commands) == 0 {
		checkpoint.Status = RunFailed
	}
}

// AdvanceUntilQuiescent commits only engine-owned transitions — start, parallel
// fork, and end advances — and never performs a side effect. It stops when no
// ready node is engine-owned, which means the remaining ready work needs an
// external actor: a program to run or a human to decide.
//
// It deliberately does not stop merely because some command is outstanding or
// some decision is awaited: under fan-out those belong to specific branches,
// and an unrelated branch's engine-owned node must still be free to advance.
func AdvanceUntilQuiescent(checkpoint Checkpoint, definition *Definition) (Checkpoint, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	advanced, _, err := advanceUntilQuiescent(checkpoint, definition, transitionBudget(definition))
	return advanced, err
}

// transitionBudget bounds one settlement pass by the prepared graph size. Every
// engine-owned advance completes exactly one distinct node, and the monotonic
// guard forbids completing a node twice, so an acyclic prepared graph can never
// need more advances than it has nodes. Deriving the bound this way is what
// lets wide fan-out run at all — the old constant was sized for a single
// sequential token.
func transitionBudget(definition *Definition) int {
	return len(definition.nodes)
}

func advanceUntilQuiescent(checkpoint Checkpoint, definition *Definition, budget int) (Checkpoint, int, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, 0, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	original := cloneCheckpoint(checkpoint)
	current := cloneCheckpoint(checkpoint)
	committed := 0
	for range budget {
		transition, ok := nextEngineTransition(current, definition)
		if !ok {
			return current, committed, nil
		}
		next, err := apply(current, definition, transition)
		if err != nil {
			return Checkpoint{}, 0, err
		}
		current = next
		committed++
	}
	if _, ok := nextEngineTransition(current, definition); ok {
		return original, 0, fmt.Errorf("%w: limit %d", ErrTransitionBudgetExhausted, budget)
	}
	return current, committed, nil
}

// nextEngineTransition names the deterministically first ready engine-owned
// node in prepared topological order. The scan is over nodes only and reads no
// edges; it is a lookup for the next unit of work, not a validity proof.
func nextEngineTransition(checkpoint Checkpoint, definition *Definition) (Transition, bool) {
	if checkpoint.Status != RunRunning || hasFailedNode(checkpoint) {
		return Transition{}, false
	}
	for _, node := range definition.nodes {
		if checkpoint.Nodes[node.id] != NodeReady {
			continue
		}
		switch node.kind {
		case definitionStart, definitionParallel:
			return Transition{Kind: TransitionAdvance, NodeID: node.id}, true
		case definitionEnd:
			// A join: any winner reaches the end while its losing branches are
			// still live, and those branches were dispatched or queued for real.
			// The end is not refused — it is simply not the engine's to advance
			// yet, so the scan continues looking for engine-owned work on another
			// branch and the run stays running until the last loser settles.
			if _, _, remains := remainingWork(checkpoint, definition, node.id); remains {
				continue
			}
			return Transition{Kind: TransitionAdvance, NodeID: node.id}, true
		}
	}
	return Transition{}, false
}

// joinAlreadyWon reports whether a join: any reducer already holds its one
// winning arrival. It reads only that node's own inbound edges, which authoring
// bounds by the normalized inbound degree limit.
func joinAlreadyWon(checkpoint Checkpoint, definition *Definition, nodeIndex int) bool {
	for _, edgeIndex := range definition.nodes[nodeIndex].incoming {
		edge := definition.edges[edgeIndex]
		if checkpoint.Edges[edge.from][edge.outcome] == EdgeArrived {
			return true
		}
	}
	return false
}

// soleOutcome returns the single authored outcome of a start or task node.
// Eligibility guarantees exactly one outgoing edge for those kinds.
func soleOutcome(definition *Definition, node definitionNode) string {
	if len(node.outgoing) != 1 {
		return ""
	}
	return definition.edges[node.outgoing[0]].outcome
}

func verdictAllowed(node definitionNode, verdict string) bool {
	return slices.Contains(node.verdicts, verdict)
}

// programCommand mints the deterministic request for one attempt of one task.
// The attempt is part of the identity, so two attempts of the same node in the
// same run never share a command id and an observation of the older one can
// never match the newer outbox entry.
func programCommand(runID string, node definitionNode, attempt int) Command {
	return Command{
		ID:      fmt.Sprintf("cmd_%d_%s_%d_%s_%d_program", len(runID), runID, len(node.id), node.id, attempt),
		Kind:    CommandProgram,
		NodeID:  node.id,
		Attempt: attempt,
		Program: cloneProgramCommand(node.program),
	}
}

// nextAttempt is the attempt number a fresh command for this node would carry.
// The counter is sparse and monotonic: an absent entry means the node has never
// executed, so its first attempt is 1.
func nextAttempt(checkpoint Checkpoint, nodeID string) int {
	return checkpoint.Attempts[nodeID] + 1
}

// recordAttempt commits a node's new current attempt. It is called only from
// the planning transition, in the same reducer step that moves the node to
// running and writes its outbox entry, so the durable counter and the durable
// command are never separately observable.
func recordAttempt(checkpoint *Checkpoint, nodeID string, attempt int) {
	if checkpoint.Attempts == nil {
		checkpoint.Attempts = make(map[string]int, 1)
	}
	checkpoint.Attempts[nodeID] = attempt
}

func cloneCheckpoint(checkpoint Checkpoint) Checkpoint {
	clone := checkpoint
	clone.Nodes = make(map[string]NodeStatus, len(checkpoint.Nodes))
	maps.Copy(clone.Nodes, checkpoint.Nodes)
	// Sparse by construction: a run with nothing executed yet keeps a nil map,
	// so a rejected transition leaves the caller's checkpoint encoding untouched.
	if len(checkpoint.Attempts) > 0 {
		clone.Attempts = make(map[string]int, len(checkpoint.Attempts))
		maps.Copy(clone.Attempts, checkpoint.Attempts)
	}
	clone.Edges = make(map[string]map[string]EdgeDisposition, len(checkpoint.Edges))
	for from, dispositions := range checkpoint.Edges {
		copied := make(map[string]EdgeDisposition, len(dispositions))
		maps.Copy(copied, dispositions)
		clone.Edges[from] = copied
	}
	if len(checkpoint.AwaitingDecisions) > 0 {
		clone.AwaitingDecisions = append([]DecisionObligation(nil), checkpoint.AwaitingDecisions...)
	}
	if len(checkpoint.Commands) > 0 {
		clone.Commands = make([]Command, len(checkpoint.Commands))
		for i := range checkpoint.Commands {
			clone.Commands[i] = cloneCommand(checkpoint.Commands[i])
		}
	}
	return clone
}

func cloneCommand(command Command) Command {
	clone := command
	clone.Program = cloneProgramCommand(command.Program)
	return clone
}

func cloneProgramCommand(program ProgramCommand) ProgramCommand {
	clone := program
	clone.Args = append([]string(nil), program.Args...)
	return clone
}
