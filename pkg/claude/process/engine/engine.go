package engine

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// Initialize creates the exact pre-execution v1 state. It performs no implicit
// advancement: the explicit start node is the sole ready node.
func Initialize(runID string, tmpl *model.Template, params map[string]string) (Checkpoint, error) {
	def, err := newDefinition(tmpl)
	if err != nil {
		return Checkpoint{}, err
	}
	nodes := make(map[string]NodeStatus, len(def.sequence))
	for _, nodeID := range def.sequence {
		nodes[nodeID] = NodePending
	}
	nodes[def.sequence[0]] = NodeReady
	checkpoint := Checkpoint{
		Version: CheckpointVersion,
		RunID:   runID,
		Status:  RunRunning,
		Nodes:   nodes,
	}
	if err := validateCheckpoint(checkpoint, def, params); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

// Plan returns at most one deterministic program command. An already
// outstanding command is returned verbatim so recovery/replanning observes the
// same identity and bound payload.
func Plan(checkpoint Checkpoint, tmpl *model.Template, params map[string]string) (*Command, error) {
	def, err := newDefinition(tmpl)
	if err != nil {
		return nil, err
	}
	if err := validateCheckpoint(checkpoint, def, params); err != nil {
		return nil, err
	}
	return plan(checkpoint, def, params), nil
}

func plan(checkpoint Checkpoint, def definition, params map[string]string) *Command {
	if checkpoint.OutstandingCommand != nil {
		command := cloneCommand(*checkpoint.OutstandingCommand)
		return &command
	}
	if checkpoint.Status != RunRunning {
		return nil
	}
	for _, nodeID := range def.sequence {
		if checkpoint.Nodes[nodeID] != NodeReady {
			continue
		}
		node := def.template.Nodes[nodeID]
		if node.Type != model.NodeTypeTask {
			return nil
		}
		command := programCommand(checkpoint.RunID, nodeID, *node.Performer, params)
		return &command
	}
	return nil
}

// Apply is the side-effect-free reducer. Input maps and slices are cloned, and
// both the loaded state and proposed output are checked against all invariants.
func Apply(checkpoint Checkpoint, tmpl *model.Template, params map[string]string, transition Transition) (Checkpoint, error) {
	def, err := newDefinition(tmpl)
	if err != nil {
		return Checkpoint{}, err
	}
	return apply(checkpoint, def, params, transition)
}

func apply(checkpoint Checkpoint, def definition, params map[string]string, transition Transition) (Checkpoint, error) {
	if err := validateCheckpoint(checkpoint, def, params); err != nil {
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
		nodeID, node, ok := readyNode(next, def)
		if !ok {
			return invalid("advance requires one ready engine-owned node")
		}
		switch node.Type {
		case model.NodeTypeStart:
			next.Nodes[nodeID] = NodeDone
			next.Nodes[soleTarget(node.Next)] = NodeReady
		case model.NodeTypeEnd:
			next.Nodes[nodeID] = NodeDone
			next.Status = terminalStatus(node.Result)
		default:
			return invalid("ready task %q requires a planned command", nodeID)
		}
	case TransitionCommandPlanned:
		if transition.Command == nil || transition.Observation != nil {
			return invalid("command_planned requires only a command payload")
		}
		nodeID, node, ok := readyNode(next, def)
		if !ok || node.Type != model.NodeTypeTask {
			return invalid("command_planned requires one ready program task")
		}
		expected := programCommand(next.RunID, nodeID, *node.Performer, params)
		if !commandsEqual(*transition.Command, expected) {
			return invalid("planned command does not match deterministic command for node %q", nodeID)
		}
		next.Nodes[nodeID] = NodeRunning
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
			next.Nodes[soleTarget(def.template.Nodes[nodeID].Next)] = NodeReady
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

	if err := validateCheckpoint(next, def, params); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: proposed checkpoint: %v", ErrInvalidTransition, err)
	}
	return next, nil
}

// AdvanceUntilQuiescent commits only engine-owned transitions. It stops at an
// outstanding program command or terminal state, and never performs a side
// effect itself.
func AdvanceUntilQuiescent(checkpoint Checkpoint, tmpl *model.Template, params map[string]string) (Checkpoint, error) {
	def, err := newDefinition(tmpl)
	if err != nil {
		return Checkpoint{}, err
	}
	return advanceUntilQuiescent(checkpoint, def, params, MaxEngineTransitions)
}

func advanceUntilQuiescent(checkpoint Checkpoint, def definition, params map[string]string, budget int) (Checkpoint, error) {
	if err := validateCheckpoint(checkpoint, def, params); err != nil {
		return Checkpoint{}, err
	}
	original := cloneCheckpoint(checkpoint)
	current := cloneCheckpoint(checkpoint)
	for transitions := 0; transitions < budget; transitions++ {
		transition, ok := nextEngineTransition(current, def, params)
		if !ok {
			return current, nil
		}
		next, err := apply(current, def, params, transition)
		if err != nil {
			return Checkpoint{}, err
		}
		current = next
	}
	if _, ok := nextEngineTransition(current, def, params); ok {
		return original, fmt.Errorf("%w: limit %d", ErrTransitionBudgetExhausted, budget)
	}
	return current, nil
}

func nextEngineTransition(checkpoint Checkpoint, def definition, params map[string]string) (Transition, bool) {
	if checkpoint.Status != RunRunning || checkpoint.OutstandingCommand != nil {
		return Transition{}, false
	}
	nodeID, node, ok := readyNode(checkpoint, def)
	if !ok {
		return Transition{}, false
	}
	if node.Type == model.NodeTypeTask {
		command := programCommand(checkpoint.RunID, nodeID, *node.Performer, params)
		return Transition{Kind: TransitionCommandPlanned, Command: &command}, true
	}
	return Transition{Kind: TransitionAdvance}, true
}

func readyNode(checkpoint Checkpoint, def definition) (string, model.Node, bool) {
	for _, nodeID := range def.sequence {
		if checkpoint.Nodes[nodeID] == NodeReady {
			return nodeID, def.template.Nodes[nodeID], true
		}
	}
	return "", model.Node{}, false
}

func programCommand(runID, nodeID string, performer model.Performer, params map[string]string) Command {
	bound := model.InterpolatePerformer(performer, params)
	return Command{
		ID:     fmt.Sprintf("cmd_%d_%s_%d_%s_program", len(runID), runID, len(nodeID), nodeID),
		Kind:   CommandProgram,
		NodeID: nodeID,
		Program: ProgramCommand{
			Profile: bound.Profile,
			Run:     bound.Run,
			Args:    append([]string(nil), bound.Args...),
			Timeout: bound.Timeout,
		},
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
	clone.Program.Args = append([]string(nil), command.Program.Args...)
	return clone
}
