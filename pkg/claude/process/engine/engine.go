package engine

import (
	"fmt"
	"maps"
	"slices"
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

// Plan returns at most one deterministic program command using only the
// prepared definition and typed checkpoint state. It runs no whole-checkpoint
// validation: callers pass state already validated once at the load
// (DecodeCheckpoint) or creation (Initialize) boundary, and the planner only
// reads it. The cheap prepared-definition guard just avoids a nil dereference.
func Plan(checkpoint Checkpoint, definition *Definition) (*Command, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return nil, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	return plan(checkpoint, definition), nil
}

func plan(checkpoint Checkpoint, definition *Definition) *Command {
	if outstanding := checkpoint.OutstandingCommand(); outstanding != nil {
		command := cloneCommand(*outstanding)
		return &command
	}
	if checkpoint.Status != RunRunning {
		return nil
	}
	index, ok := activeNodeIndex(checkpoint, definition)
	if !ok || definition.nodes[index].kind != definitionTask {
		return nil
	}
	command := programCommand(checkpoint.RunID, definition.nodes[index])
	return &command
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
		index, ok := activeNodeIndex(next, definition)
		if !ok || next.Nodes[definition.nodes[index].id] != NodeReady {
			return invalid("advance requires one ready engine-owned node")
		}
		node := definition.nodes[index]
		switch node.kind {
		case definitionStart:
			if err := completeAndSettle(&next, definition, index, soleOutcome(definition, node)); err != nil {
				return Checkpoint{}, err
			}
		case definitionEnd:
			next.Nodes[node.id] = NodeDone
			next.Status = node.terminal
		default:
			return invalid("ready node %q requires a planned command or decision", node.id)
		}
	case TransitionCommandPlanned:
		if transition.Command == nil || transition.Observation != nil || transition.Decision != nil {
			return invalid("command_planned requires only a command payload")
		}
		index, ok := activeNodeIndex(next, definition)
		if !ok || next.Nodes[definition.nodes[index].id] != NodeReady || definition.nodes[index].kind != definitionTask {
			return invalid("command_planned requires one ready program task")
		}
		node := definition.nodes[index]
		expected := programCommand(next.RunID, node)
		if !commandsEqual(*transition.Command, expected) {
			return invalid("planned command does not match deterministic command for node %q", node.id)
		}
		next.Nodes[node.id] = NodeRunning
		next.Commands = []Command{cloneCommand(expected)}
	case TransitionProgramObserved:
		if transition.Observation == nil || transition.Command != nil || transition.Decision != nil {
			return invalid("program_observed requires only an observation payload")
		}
		observation := transition.Observation
		outstanding := next.OutstandingCommand()
		if outstanding == nil || observation.CommandID != outstanding.ID || observation.NodeID != outstanding.NodeID {
			return Checkpoint{}, fmt.Errorf("%w: observation does not match the outstanding command", ErrStaleObservation)
		}
		nodeID := outstanding.NodeID
		index, ok := definition.index[nodeID]
		if !ok || definition.nodes[index].kind != definitionTask {
			return invalid("outstanding command node %q is not prepared", nodeID)
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
			next.Commands = nil
			if err := completeAndSettle(&next, definition, index, soleOutcome(definition, node)); err != nil {
				return Checkpoint{}, err
			}
		case ProgramFailed:
			next.Commands = nil
			next.Nodes[nodeID] = NodeFailed
			next.Status = RunFailed
		default:
			return invalid("unknown program outcome %q", observation.Outcome)
		}
	case TransitionDecisionRecorded:
		if transition.Decision == nil || transition.Command != nil || transition.Observation != nil {
			return invalid("decision_recorded requires only a decision payload")
		}
		decision := transition.Decision
		if awaited := next.AwaitingDecision(); awaited == nil || awaited.NodeID != decision.NodeID {
			return Checkpoint{}, fmt.Errorf("%w: run is not awaiting a decision for node %q", ErrStaleDecision, decision.NodeID)
		}
		index := definition.index[decision.NodeID]
		node := definition.nodes[index]
		if !verdictAllowed(node, decision.Verdict) {
			return Checkpoint{}, fmt.Errorf("%w: verdict %q is not an authored outcome of decision %q", ErrInvalidDecisionVerdict, decision.Verdict, node.id)
		}
		if err := completeAndSettle(&next, definition, index, decision.Verdict); err != nil {
			return Checkpoint{}, err
		}
	default:
		return invalid("unknown transition kind %q", transition.Kind)
	}
	return next, nil
}

// completeAndSettle finishes one active node by marking its chosen outgoing
// edge arrived and every alternative not taken, then propagates closure and
// activation through the affected subgraph. A node whose entire candidate set
// settles without an arrival is skipped and closes its own outgoing edges; a
// node with exactly one arrival in a fully settled candidate set activates —
// the local-merge rule. Each affected node's incoming edge list is scanned at
// most once and each settled edge does constant bookkeeping afterwards, so a
// settlement pass is linear in the affected nodes and edges.
func completeAndSettle(next *Checkpoint, definition *Definition, index int, chosenOutcome string) error {
	node := definition.nodes[index]
	// Monotonic guard: only an active node may complete. Final states never
	// regress or reactivate, so a caller that lost track of node status fails
	// closed here rather than silently rewriting a settled node.
	if status := next.Nodes[node.id]; status != NodeReady && status != NodeRunning {
		return fmt.Errorf("%w: node %q is not active and cannot complete", ErrInvalidTransition, node.id)
	}
	next.Nodes[node.id] = NodeDone
	next.AwaitingDecisions = nil

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
		disposition := EdgeNotTaken
		if definition.edges[edgeIndex].outcome == chosenOutcome {
			disposition = EdgeArrived
		}
		if err := settleEdge(edgeIndex, disposition); err != nil {
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
		switch counts.arrived {
		case 0:
			next.Nodes[target.id] = NodeSkipped
			for _, edgeIndex := range target.outgoing {
				if err := settleEdge(edgeIndex, EdgeNotTaken); err != nil {
					return err
				}
			}
		case 1:
			next.Nodes[target.id] = NodeReady
			if target.kind == definitionDecision {
				next.AwaitingDecisions = []DecisionObligation{{NodeID: target.id}}
			}
		default:
			return fmt.Errorf("%w: node %q received more than one arrival", ErrInvalidTransition, target.id)
		}
	}
	return nil
}

// AdvanceUntilQuiescent commits only engine-owned transitions. It stops at an
// outstanding program command, an awaited decision, or terminal state, and
// never performs a side effect.
func AdvanceUntilQuiescent(checkpoint Checkpoint, definition *Definition) (Checkpoint, error) {
	return advanceUntilQuiescent(checkpoint, definition, MaxEngineTransitions)
}

func advanceUntilQuiescent(checkpoint Checkpoint, definition *Definition, budget int) (Checkpoint, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	original := cloneCheckpoint(checkpoint)
	current := cloneCheckpoint(checkpoint)
	for range budget {
		transition, ok := nextEngineTransition(current, definition)
		if !ok {
			return current, nil
		}
		next, err := apply(current, definition, transition)
		if err != nil {
			return Checkpoint{}, err
		}
		current = next
	}
	if _, ok := nextEngineTransition(current, definition); ok {
		return original, fmt.Errorf("%w: limit %d", ErrTransitionBudgetExhausted, budget)
	}
	return current, nil
}

func nextEngineTransition(checkpoint Checkpoint, definition *Definition) (Transition, bool) {
	if checkpoint.Status != RunRunning || len(checkpoint.Commands) > 0 || len(checkpoint.AwaitingDecisions) > 0 {
		return Transition{}, false
	}
	index, ok := activeNodeIndex(checkpoint, definition)
	if !ok || checkpoint.Nodes[definition.nodes[index].id] != NodeReady {
		return Transition{}, false
	}
	switch definition.nodes[index].kind {
	case definitionTask:
		command := programCommand(checkpoint.RunID, definition.nodes[index])
		return Transition{Kind: TransitionCommandPlanned, Command: &command}, true
	case definitionStart, definitionEnd:
		return Transition{Kind: TransitionAdvance}, true
	default:
		return Transition{}, false
	}
}

func activeNodeIndex(checkpoint Checkpoint, definition *Definition) (int, bool) {
	for index, node := range definition.nodes {
		if status := checkpoint.Nodes[node.id]; status == NodeReady || status == NodeRunning {
			return index, true
		}
	}
	return 0, false
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
