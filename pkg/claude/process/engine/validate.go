package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

type definition struct {
	sequence []string
	template *model.Template
}

func newDefinition(tmpl *model.Template) (definition, error) {
	if err := RequireEligible(tmpl); err != nil {
		return definition{}, err
	}
	sequence := make([]string, 0, len(tmpl.Nodes))
	for current := tmpl.Start; ; current = soleTarget(tmpl.Nodes[current].Next) {
		sequence = append(sequence, current)
		if tmpl.Nodes[current].Type == model.NodeTypeEnd {
			break
		}
	}
	return definition{sequence: sequence, template: tmpl}, nil
}

// DecodeCheckpoint performs strict shape decoding followed by semantic
// validation against the pinned template and immutable run parameters.
func DecodeCheckpoint(data []byte, tmpl *model.Template, params map[string]string) (Checkpoint, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var checkpoint Checkpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: decode: %v", ErrInvalidCheckpoint, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Checkpoint{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidCheckpoint, err)
	}
	if err := ValidateCheckpoint(checkpoint, tmpl, params); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

// ValidateCheckpoint checks the complete semantic state. Reducer entry and
// exit paths call the same validator, so malformed loaded state cannot advance
// and a proposed transition cannot return an inconsistent checkpoint.
func ValidateCheckpoint(checkpoint Checkpoint, tmpl *model.Template, params map[string]string) error {
	def, err := newDefinition(tmpl)
	if err != nil {
		return err
	}
	return validateCheckpoint(checkpoint, def, params)
}

func validateCheckpoint(checkpoint Checkpoint, def definition, params map[string]string) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, fmt.Sprintf(format, args...))
	}
	if checkpoint.Version != CheckpointVersion {
		return invalid("version must be %d; got %d", CheckpointVersion, checkpoint.Version)
	}
	if !validRunID(checkpoint.RunID) {
		return invalid("runId must be a lowercase runtime identifier of at most 128 bytes")
	}
	if len(checkpoint.Nodes) != len(def.sequence) {
		return invalid("nodes must contain exactly the %d template nodes", len(def.sequence))
	}
	for nodeID := range checkpoint.Nodes {
		if _, ok := def.template.Nodes[nodeID]; !ok {
			return invalid("nodes contains unknown node %q", nodeID)
		}
	}

	if checkpoint.OutstandingCommand != nil {
		command := checkpoint.OutstandingCommand
		if command.Kind != CommandProgram {
			return invalid("outstanding command kind must be %q", CommandProgram)
		}
		node, ok := def.template.Nodes[command.NodeID]
		if !ok || node.Type != model.NodeTypeTask || node.Performer == nil || node.Performer.Kind != model.PerformerProgram {
			return invalid("outstanding command node %q is not a program task", command.NodeID)
		}
		expected := programCommand(checkpoint.RunID, command.NodeID, *node.Performer, params)
		if !commandsEqual(*command, expected) {
			return invalid("outstanding command does not match the deterministic bound request for node %q", command.NodeID)
		}
	}

	switch checkpoint.Status {
	case RunRunning:
		active := -1
		for index, nodeID := range def.sequence {
			status := checkpoint.Nodes[nodeID]
			if active < 0 {
				switch status {
				case NodeDone:
					continue
				case NodeReady, NodeRunning:
					active = index
				default:
					return invalid("running run has non-prefix status %q at node %q", status, nodeID)
				}
				continue
			}
			if status != NodePending {
				return invalid("node %q after the active node must be pending; got %q", nodeID, status)
			}
		}
		if active < 0 {
			return invalid("running run must have one ready or running node")
		}
		activeID := def.sequence[active]
		activeStatus := checkpoint.Nodes[activeID]
		if activeStatus == NodeRunning {
			if checkpoint.OutstandingCommand == nil || checkpoint.OutstandingCommand.NodeID != activeID {
				return invalid("running node %q requires its outstanding command", activeID)
			}
			if def.template.Nodes[activeID].Type != model.NodeTypeTask {
				return invalid("only a task node may be running; got %q", activeID)
			}
		} else if checkpoint.OutstandingCommand != nil {
			return invalid("ready node %q cannot coexist with an outstanding command", activeID)
		}
	case RunCompleted, RunCanceled:
		if checkpoint.OutstandingCommand != nil {
			return invalid("terminal run cannot have an outstanding command")
		}
		for _, nodeID := range def.sequence {
			if checkpoint.Nodes[nodeID] != NodeDone {
				return invalid("terminal run requires node %q to be done", nodeID)
			}
		}
		want := terminalStatus(def.template.Nodes[def.sequence[len(def.sequence)-1]].Result)
		if checkpoint.Status != want {
			return invalid("terminal run status %q disagrees with end result %q", checkpoint.Status, want)
		}
	case RunFailed:
		if checkpoint.OutstandingCommand != nil {
			return invalid("failed run cannot have an outstanding command")
		}
		allDone := true
		for _, nodeID := range def.sequence {
			allDone = allDone && checkpoint.Nodes[nodeID] == NodeDone
		}
		if allDone {
			if terminalStatus(def.template.Nodes[def.sequence[len(def.sequence)-1]].Result) != RunFailed {
				return invalid("all-done failed run requires a failed end result")
			}
			break
		}
		failed := -1
		for index, nodeID := range def.sequence {
			status := checkpoint.Nodes[nodeID]
			switch {
			case failed < 0 && status == NodeDone:
				continue
			case failed < 0 && status == NodeFailed:
				failed = index
				if def.template.Nodes[nodeID].Type != model.NodeTypeTask {
					return invalid("only a program task may fail; got %q", nodeID)
				}
			case failed >= 0 && status == NodePending:
				continue
			default:
				return invalid("failed run has inconsistent status %q at node %q", status, nodeID)
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
