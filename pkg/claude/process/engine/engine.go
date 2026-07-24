package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Initialize creates the exact pre-execution v2 state from one prepared
// definition. It performs no implicit advancement: start is the sole ready
// node and every authored edge begins unresolved. As a creation boundary it
// runs the full structural ValidateCheckpoint once on the constructed state;
// the per-transition runtime cycle then trusts that boundary and does not.
func Initialize(runID string, definition *Definition) (Checkpoint, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	nodes := make(map[string]NodeStatus, len(definition.nodes))
	for _, node := range definition.nodes {
		nodes[node.id] = NodePending
	}
	nodes[definition.nodes[0].id] = NodeReady
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
// rather than a batch one: it commits one command per call, which is exactly
// what the current sequential executor consumes. PR2 refills up to its own
// concurrency capacity by calling AdvanceAndPlan repeatedly on the returned
// checkpoint — the previously planned task is Running by then, so each call
// picks the next ready one — and needs no change to the durable shape, because
// Commands is already a plural outbox.
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
	command := programCommand(next.RunID, node)
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
	if checkpoint.Status != RunRunning {
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
		expected := programCommand(next.RunID, node)
		if !commandsEqual(*transition.Command, expected) {
			return invalid("planned command does not match deterministic command for node %q", node.id)
		}
		next.Nodes[node.id] = NodeRunning
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
		case ProgramFailed:
			removeCommand(&next, outstanding.NodeID)
			next.Nodes[node.id] = NodeFailed
			next.Status = RunFailed
			// A failed task fails the whole run, exactly as it did sequentially.
			// This is terminal cleanup: the run is over, so sibling entries must
			// not survive into terminal state, where they would read as still
			// actionable and would be rejected by the load boundary. It is not
			// the per-transition cross-branch clearing the plural outboxes exist
			// to prevent — see abandonOutboxes.
			//
			// Under this slice's sequential consumption the observed command is
			// the only one in flight, so this only ever drops sibling decision
			// obligations. Once PR2 dispatches several commands at once, a
			// sibling command dropped here may name a program that is still
			// executing; deciding whether that branch is cancelled, awaited, or
			// reconciled is the cancellation protocol that ticket owns.
			abandonOutboxes(&next)
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
// live. Structured fan-out/join authoring already guarantees an end node sits
// outside every open parallel scope — a branch cannot escape its scope, and a
// join only activates once its whole candidate set settled — so on a valid
// prepared graph this never fires. It exists so that a future semantics bug
// fails loudly here instead of silently reporting a terminal run while branches
// are still in flight. It is one scan at the single terminating transition of a
// run, not a per-transition whole-graph proof.
func requireNoWorkRemains(checkpoint Checkpoint, definition *Definition, endNodeID string) error {
	if len(checkpoint.Commands) > 0 {
		return fmt.Errorf("%w: end %q cannot complete while node %q still has an outstanding command",
			ErrInvalidTransition, endNodeID, checkpoint.Commands[0].NodeID)
	}
	if len(checkpoint.AwaitingDecisions) > 0 {
		return fmt.Errorf("%w: end %q cannot complete while node %q is still awaiting a decision",
			ErrInvalidTransition, endNodeID, checkpoint.AwaitingDecisions[0].NodeID)
	}
	for _, other := range definition.nodes {
		if other.id == endNodeID {
			continue
		}
		if status := checkpoint.Nodes[other.id]; status == NodeReady || status == NodeRunning {
			return fmt.Errorf("%w: end %q cannot complete while node %q is %q",
				ErrInvalidTransition, endNodeID, other.id, status)
		}
	}
	return nil
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
// A target is only ever considered once its COMPLETE candidate input set has
// settled. At that point:
//   - zero arrivals: the target is skipped, and closing its own outgoing edges
//     recursively propagates the closure;
//   - a join: all reducer with one or more arrivals: ready;
//   - a non-join node with exactly one arrival: ready (the local-merge rule);
//   - a non-join node with more than one arrival: a local fail-closed
//     ErrInvalidTransition, since exclusive routing was violated.
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
		next.Edges[edge.from][edge.outcome] = disposition
		if counts := touched[edge.toIndex]; counts != nil {
			counts.unresolved--
			if disposition == EdgeArrived {
				counts.arrived++
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

// abandonOutboxes is TERMINAL CLEANUP, not ordinary cross-branch clearing.
//
// The distinction matters and is deliberate. Ordinary reducer mutations are
// strictly per-entry — planning, observing, and deciding each touch exactly one
// branch's entry — because a live run must never let one branch invalidate
// another's outstanding work. This function runs only on the transition that
// ends the run, where no branch will ever be dispatched or decided again, and
// where leaving entries behind would produce a checkpoint the load boundary
// rejects (obligations require a running run). It must never be called from a
// transition that leaves the run running.
func abandonOutboxes(checkpoint *Checkpoint) {
	checkpoint.Commands = nil
	checkpoint.AwaitingDecisions = nil
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
	if checkpoint.Status != RunRunning {
		return Transition{}, false
	}
	for _, node := range definition.nodes {
		if checkpoint.Nodes[node.id] != NodeReady {
			continue
		}
		switch node.kind {
		case definitionStart, definitionEnd, definitionParallel:
			return Transition{Kind: TransitionAdvance, NodeID: node.id}, true
		}
	}
	return Transition{}, false
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

func programCommand(runID string, node definitionNode) Command {
	return Command{
		ID:      fmt.Sprintf("cmd_%d_%s_%d_%s_program", len(runID), runID, len(node.id), node.id),
		Kind:    CommandProgram,
		NodeID:  node.id,
		Program: cloneProgramCommand(node.program),
	}
}

func cloneCheckpoint(checkpoint Checkpoint) Checkpoint {
	clone := checkpoint
	clone.Nodes = make(map[string]NodeStatus, len(checkpoint.Nodes))
	maps.Copy(clone.Nodes, checkpoint.Nodes)
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
