package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	ErrNewerSchemaVersion   = errors.New("process state schema version is newer than this binary supports")
	ErrInvalidSchemaVersion = errors.New("process state schema version is invalid")
)

func Decode(data []byte) (*State, error) {
	var st State
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("decode process state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode process state: trailing JSON: %w", err)
		}
		return nil, fmt.Errorf("decode process state: multiple JSON values")
	}
	if err := CheckSchemaVersion(st.StateSchemaVersion); err != nil {
		return nil, err
	}
	normalizeState(&st)
	return &st, nil
}

func Encode(st *State) ([]byte, error) {
	if st == nil {
		return nil, fmt.Errorf("nil process state")
	}
	clone := Clone(*st)
	normalizeState(&clone)
	data, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode process state: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}

func CheckSchemaVersion(version int) error {
	if version <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidSchemaVersion, version)
	}
	if version > StateSchemaVersion {
		return fmt.Errorf("%w: got %d, supported %d", ErrNewerSchemaVersion, version, StateSchemaVersion)
	}
	return nil
}

func New(runID, originalTemplateRef, currentTemplateRef string, nodes []NodeInit) State {
	st := State{
		StateSchemaVersion:  StateSchemaVersion,
		RunID:               runID,
		Status:              RunStatusPending,
		OriginalTemplateRef: originalTemplateRef,
		CurrentTemplateRef:  currentTemplateRef,
		Nodes:               map[string]NodeState{},
		OutstandingCommands: map[string]OutstandingCommand{},
		Waits:               map[string]WaitRecord{},
		Timers:              map[string]TimerRecord{},
	}
	for _, node := range nodes {
		status := node.Status
		if status == "" {
			status = NodeStatusPending
		}
		st.Nodes[node.ID] = NodeState{
			Type:     node.Type,
			Status:   status,
			Assignee: node.Assignee,
		}
	}
	return st
}

func normalizeState(st *State) {
	if st.StateSchemaVersion == 0 {
		st.StateSchemaVersion = StateSchemaVersion
	}
	if st.Status == "" {
		st.Status = RunStatusPending
	}
	if st.Nodes == nil {
		st.Nodes = map[string]NodeState{}
	}
	if st.OutstandingCommands == nil {
		st.OutstandingCommands = map[string]OutstandingCommand{}
	}
	if st.Waits == nil {
		st.Waits = map[string]WaitRecord{}
	}
	if st.Timers == nil {
		st.Timers = map[string]TimerRecord{}
	}
}
