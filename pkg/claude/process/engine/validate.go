package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// Definition is the immutable executable projection of one pinned template
// and its immutable run parameters. Its fields stay private so preparation is
// the only way to construct executable state and callers cannot mutate it.
type Definition struct {
	nodes    []definitionNode
	index    map[string]int
	terminal RunStatus
}

type definitionNodeKind uint8

const (
	definitionStart definitionNodeKind = iota + 1
	definitionTask
	definitionEnd
)

type definitionNode struct {
	id      string
	kind    definitionNodeKind
	program ProgramCommand
}

// Prepare performs all immutable work once: complete authoring validation,
// sequential-MVP eligibility, sequence derivation, parameter binding, and
// final bound-program validation. Plan, Apply, and Advance reuse the result.
func Prepare(tmpl *model.Template, params map[string]string) (*Definition, error) {
	if err := RequireEligible(tmpl); err != nil {
		return nil, err
	}
	definition := &Definition{
		nodes: make([]definitionNode, 0, len(tmpl.Nodes)),
		index: make(map[string]int, len(tmpl.Nodes)),
	}
	for current := tmpl.Start; ; current = soleTarget(tmpl.Nodes[current].Next) {
		node := tmpl.Nodes[current]
		prepared := definitionNode{id: current}
		switch node.Type {
		case model.NodeTypeStart:
			prepared.kind = definitionStart
		case model.NodeTypeTask:
			prepared.kind = definitionTask
			program, err := bindProgram(current, *node.Performer, params)
			if err != nil {
				return nil, err
			}
			prepared.program = program
		case model.NodeTypeEnd:
			prepared.kind = definitionEnd
			definition.terminal = terminalStatus(node.Result)
		}
		definition.index[current] = len(definition.nodes)
		definition.nodes = append(definition.nodes, prepared)
		if node.Type == model.NodeTypeEnd {
			break
		}
	}
	return definition, nil
}

func bindProgram(nodeID string, performer model.Performer, params map[string]string) (ProgramCommand, error) {
	for _, reference := range model.ParamReferences(performer.Run) {
		value, ok := params[reference]
		if !ok {
			return ProgramCommand{}, fmt.Errorf("%w: node %q run parameter %q is missing", ErrInvalidProgramBinding, nodeID, reference)
		}
		if strings.TrimSpace(value) == "" {
			return ProgramCommand{}, fmt.Errorf("%w: node %q run parameter %q is blank", ErrInvalidProgramBinding, nodeID, reference)
		}
	}
	for index, arg := range performer.Args {
		for _, reference := range model.ParamReferences(arg) {
			if _, ok := params[reference]; !ok {
				return ProgramCommand{}, fmt.Errorf("%w: node %q argument %d parameter %q is missing", ErrInvalidProgramBinding, nodeID, index, reference)
			}
		}
	}
	bound := model.InterpolatePerformer(performer, params)
	if strings.TrimSpace(bound.Run) == "" {
		return ProgramCommand{}, fmt.Errorf("%w: node %q run is blank after interpolation", ErrInvalidProgramBinding, nodeID)
	}
	return ProgramCommand{
		Profile: bound.Profile,
		Run:     bound.Run,
		Args:    append([]string(nil), bound.Args...),
		Timeout: bound.Timeout,
	}, nil
}

// DecodeCheckpoint is the persistence/load boundary: strict JSON shape
// decoding is followed by semantic validation against the prepared definition.
// Pure in-memory engine cycles operate on the typed Checkpoint instead.
func DecodeCheckpoint(data []byte, definition *Definition) (Checkpoint, error) {
	var checkpoint Checkpoint
	if err := strictjson.Decode(data, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: decode: %v", ErrInvalidCheckpoint, err)
	}
	if err := ValidateCheckpoint(checkpoint, definition); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

// ValidateCheckpoint checks dynamic semantic state against an immutable
// prepared definition. Reducer entry and exit paths use this same validator.
func ValidateCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	return validateCheckpoint(checkpoint, definition)
}

func validateCheckpoint(checkpoint Checkpoint, definition *Definition) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	if definition == nil || len(definition.nodes) == 0 {
		return fmt.Errorf("%w: definition was not prepared", ErrInvalidDefinition)
	}
	if checkpoint.Version != CheckpointVersion {
		return invalid("version must be %d; got %d", CheckpointVersion, checkpoint.Version)
	}
	if !validRunID(checkpoint.RunID) {
		return invalid("runId must be a lowercase runtime identifier of at most 128 bytes")
	}
	if len(checkpoint.Nodes) != len(definition.nodes) {
		return invalid("nodes must contain exactly the %d prepared nodes", len(definition.nodes))
	}
	for nodeID := range checkpoint.Nodes {
		if _, ok := definitionNodeByID(definition, nodeID); !ok {
			return invalid("nodes contains unknown node %q", nodeID)
		}
	}

	if checkpoint.OutstandingCommand != nil {
		command := checkpoint.OutstandingCommand
		if command.Kind != CommandProgram {
			return invalid("outstanding command kind must be %q", CommandProgram)
		}
		node, ok := definitionNodeByID(definition, command.NodeID)
		if !ok || node.kind != definitionTask {
			return invalid("outstanding command node %q is not a program task", command.NodeID)
		}
		expected := programCommand(checkpoint.RunID, node)
		if !commandsEqual(*command, expected) {
			return invalid("outstanding command does not match the deterministic bound request for node %q", command.NodeID)
		}
	}

	switch checkpoint.Status {
	case RunRunning:
		active := -1
		for index, node := range definition.nodes {
			status := checkpoint.Nodes[node.id]
			if active < 0 {
				switch status {
				case NodeDone:
					continue
				case NodeReady, NodeRunning:
					active = index
				default:
					return invalid("running run has non-prefix status %q at node %q", status, node.id)
				}
				continue
			}
			if status != NodePending {
				return invalid("node %q after the active node must be pending; got %q", node.id, status)
			}
		}
		if active < 0 {
			return invalid("running run must have one ready or running node")
		}
		activeNode := definition.nodes[active]
		activeStatus := checkpoint.Nodes[activeNode.id]
		if activeStatus == NodeRunning {
			if checkpoint.OutstandingCommand == nil || checkpoint.OutstandingCommand.NodeID != activeNode.id {
				return invalid("running node %q requires its outstanding command", activeNode.id)
			}
			if activeNode.kind != definitionTask {
				return invalid("only a task node may be running; got %q", activeNode.id)
			}
		} else if checkpoint.OutstandingCommand != nil {
			return invalid("ready node %q cannot coexist with an outstanding command", activeNode.id)
		}
	case RunCompleted, RunCanceled:
		if checkpoint.OutstandingCommand != nil {
			return invalid("terminal run cannot have an outstanding command")
		}
		for _, node := range definition.nodes {
			if checkpoint.Nodes[node.id] != NodeDone {
				return invalid("terminal run requires node %q to be done", node.id)
			}
		}
		if checkpoint.Status != definition.terminal {
			return invalid("terminal run status %q disagrees with prepared end status %q", checkpoint.Status, definition.terminal)
		}
	case RunFailed:
		if checkpoint.OutstandingCommand != nil {
			return invalid("failed run cannot have an outstanding command")
		}
		allDone := true
		for _, node := range definition.nodes {
			allDone = allDone && checkpoint.Nodes[node.id] == NodeDone
		}
		if allDone {
			if definition.terminal != RunFailed {
				return invalid("all-done failed run requires a failed prepared end status")
			}
			break
		}
		failed := -1
		for index, node := range definition.nodes {
			status := checkpoint.Nodes[node.id]
			switch {
			case failed < 0 && status == NodeDone:
				continue
			case failed < 0 && status == NodeFailed:
				failed = index
				if node.kind != definitionTask {
					return invalid("only a program task may fail; got %q", node.id)
				}
			case failed >= 0 && status == NodePending:
				continue
			default:
				return invalid("failed run has inconsistent status %q at node %q", status, node.id)
			}
		}
		if failed < 0 {
			return invalid("failed run must contain one failed task")
		}
	default:
		return invalid("unknown run status %q", checkpoint.Status)
	}
	return nil
}

func definitionNodeByID(definition *Definition, nodeID string) (definitionNode, bool) {
	if definition == nil {
		return definitionNode{}, false
	}
	index, ok := definition.index[nodeID]
	if !ok {
		return definitionNode{}, false
	}
	return definition.nodes[index], true
}

func validRunID(runID string) bool {
	if len(runID) == 0 || len(runID) > 128 {
		return false
	}
	first := runID[0]
	if (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		return false
	}
	for _, value := range []byte(runID) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-' {
			continue
		}
		return false
	}
	return true
}

func terminalStatus(result string) RunStatus {
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "fail", "failed", "failure", "error":
		return RunFailed
	case "cancel", "canceled", "cancelled":
		return RunCanceled
	default:
		return RunCompleted
	}
}

func commandsEqual(left, right Command) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.NodeID == right.NodeID &&
		left.Program.Profile == right.Program.Profile && left.Program.Run == right.Program.Run &&
		left.Program.Timeout == right.Program.Timeout && slices.Equal(left.Program.Args, right.Program.Args)
}
