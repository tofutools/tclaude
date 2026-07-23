package engine

import "fmt"

// Initialize creates the exact pre-execution v1 state from one prepared
// definition. It performs no implicit advancement: start is the sole ready node.
func Initialize(runID string, definition *Definition) (Checkpoint, error) {
	if definition == nil || len(definition.nodes) == 0 {
		return Checkpoint{}, fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	nodes := make(map[string]NodeStatus, len(definition.nodes))
	for _, node := range definition.nodes {
		nodes[node.id] = NodePending
	}
	nodes[definition.nodes[0].id] = NodeReady
	checkpoint := Checkpoint{
		Version: CheckpointVersion,
		RunID:   runID,
		Status:  RunRunning,
		Nodes:   nodes,
	}
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

// Plan returns at most one deterministic program command using only the
// prepared definition and typed checkpoint state.
func Plan(checkpoint Checkpoint, definition *Definition) (*Command, error) {
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return nil, err
	}
	return plan(checkpoint, definition), nil
}

func plan(checkpoint Checkpoint, definition *Definition) *Command {
	if checkpoint.OutstandingCommand != nil {
		command := cloneCommand(*checkpoint.OutstandingCommand)
		return &command
	}
	if checkpoint.Status != RunRunning {
		return nil
	}
	for _, node := range definition.nodes {
		if checkpoint.Nodes[node.id] != NodeReady {
			continue
		}
		if node.kind != definitionTask {
			return nil
		}
		command := programCommand(checkpoint.RunID, node)
		return &command
	}
	return nil
}

// Apply is the side-effect-free reducer. Input maps and slices are cloned, and
// both the loaded state and proposed output are checked against all invariants.
func Apply(checkpoint Checkpoint, definition *Definition, transition Transition) (Checkpoint, error) {
	return apply(checkpoint, definition, transition)
}

func apply(checkpoint Checkpoint, definition *Definition, transition Transition) (Checkpoint, error) {
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return Checkpoint{}, err
	}
	next := cloneCheckpoint(checkpoint)
	invalid := func(format string, args ...any) (Checkpoint, error) {
		return Checkpoint{}, fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
	}

	switch transition.Kind {
	case TransitionAdvance:
		if transition.Command != nil || transition.Observation != nil {
			return invalid("advance transition cannot carry a payload")
		}
		index, node, ok := readyNode(next, definition)
		if !ok {
			return invalid("advance requires one ready engine-owned node")
		}
		switch node.kind {
		case definitionStart:
			next.Nodes[node.id] = NodeDone
			next.Nodes[definition.nodes[index+1].id] = NodeReady
		case definitionEnd:
			next.Nodes[node.id] = NodeDone
			next.Status = definition.terminal
		default:
			return invalid("ready task %q requires a planned command", node.id)
		}
	case TransitionCommandPlanned:
		if transition.Command == nil || transition.Observation != nil {
			return invalid("command_planned requires only a command payload")
		}
		_, node, ok := readyNode(next, definition)
		if !ok || node.kind != definitionTask {
			return invalid("command_planned requires one ready program task")
		}
		expected := programCommand(next.RunID, node)
		if !commandsEqual(*transition.Command, expected) {
			return invalid("planned command does not match deterministic command for node %q", node.id)
		}
		next.Nodes[node.id] = NodeRunning
		command := cloneCommand(expected)
		next.OutstandingCommand = &command
	case TransitionProgramObserved:
		if transition.Observation == nil || transition.Command != nil {
			return invalid("program_observed requires only an observation payload")
		}
		observation := transition.Observation
		if next.OutstandingCommand == nil || observation.CommandID != next.OutstandingCommand.ID || observation.NodeID != next.OutstandingCommand.NodeID {
			return Checkpoint{}, fmt.Errorf("%w: observation does not match the outstanding command", ErrStaleObservation)
		}
		nodeID := next.OutstandingCommand.NodeID
		index, node, ok := definitionNodeIndex(definition, nodeID)
		if !ok || node.kind != definitionTask {
			return invalid("outstanding command node %q is not prepared", nodeID)
		}
		switch observation.Outcome {
		case ProgramSucceeded:
			if observation.ExitCode != 0 {
				return invalid("successful program observation must have exit code 0")
			}
			if observation.Error != "" {
				return invalid("successful program observation cannot carry an error")
			}
			next.OutstandingCommand = nil
			next.Nodes[nodeID] = NodeDone
			next.Nodes[definition.nodes[index+1].id] = NodeReady
		case ProgramFailed:
			next.OutstandingCommand = nil
			next.Nodes[nodeID] = NodeFailed
			next.Status = RunFailed
		default:
			return invalid("unknown program outcome %q", observation.Outcome)
		}
	default:
		return invalid("unknown transition kind %q", transition.Kind)
	}

	if err := validateCheckpoint(next, definition); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: proposed checkpoint: %v", ErrInvalidTransition, err)
	}
	return next, nil
}

// AdvanceUntilQuiescent commits only engine-owned transitions. It stops at an
// outstanding program command or terminal state, and never performs a side effect.
func AdvanceUntilQuiescent(checkpoint Checkpoint, definition *Definition) (Checkpoint, error) {
	return advanceUntilQuiescent(checkpoint, definition, MaxEngineTransitions)
}

func advanceUntilQuiescent(checkpoint Checkpoint, definition *Definition, budget int) (Checkpoint, error) {
	if err := validateCheckpoint(checkpoint, definition); err != nil {
		return Checkpoint{}, err
	}
	original := cloneCheckpoint(checkpoint)
	current := cloneCheckpoint(checkpoint)
	for transitions := 0; transitions < budget; transitions++ {
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
	if checkpoint.Status != RunRunning || checkpoint.OutstandingCommand != nil {
		return Transition{}, false
	}
	_, node, ok := readyNode(checkpoint, definition)
	if !ok {
		return Transition{}, false
	}
	if node.kind == definitionTask {
		command := programCommand(checkpoint.RunID, node)
		return Transition{Kind: TransitionCommandPlanned, Command: &command}, true
	}
	return Transition{Kind: TransitionAdvance}, true
}

func readyNode(checkpoint Checkpoint, definition *Definition) (int, definitionNode, bool) {
	for index, node := range definition.nodes {
		if checkpoint.Nodes[node.id] == NodeReady {
			return index, node, true
		}
	}
	return 0, definitionNode{}, false
}

func definitionNodeIndex(definition *Definition, nodeID string) (int, definitionNode, bool) {
	if definition == nil {
		return 0, definitionNode{}, false
	}
	index, ok := definition.index[nodeID]
	if !ok {
		return 0, definitionNode{}, false
	}
	return index, definition.nodes[index], true
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
	for nodeID, status := range checkpoint.Nodes {
		clone.Nodes[nodeID] = status
	}
	if checkpoint.OutstandingCommand != nil {
		command := cloneCommand(*checkpoint.OutstandingCommand)
		clone.OutstandingCommand = &command
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
