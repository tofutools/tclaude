package engine

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const CheckpointVersion = 1

const MaxEngineTransitions = 8

type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
)

type NodeStatus string

const (
	NodePending NodeStatus = "pending"
	NodeReady   NodeStatus = "ready"
	NodeRunning NodeStatus = "running"
	NodeDone    NodeStatus = "done"
	NodeFailed  NodeStatus = "failed"
)

type CommandKind string

const CommandProgram CommandKind = "program"

// Checkpoint is the complete v1 reducer state. The pinned template and run
// parameters live beside it in the run record, rather than being copied into
// every transition.
type Checkpoint struct {
	Version            int                   `json:"version"`
	RunID              string                `json:"runId"`
	Status             RunStatus             `json:"status"`
	Nodes              map[string]NodeStatus `json:"nodes"`
	OutstandingCommand *Command              `json:"outstandingCommand,omitempty"`
}

// Command is the one durable outbox item this sequential slice can produce.
// Program contains the fully bound request so dispatch never has to reread
// mutable authoring input.
type Command struct {
	ID      string         `json:"id"`
	Kind    CommandKind    `json:"kind"`
	NodeID  string         `json:"nodeId"`
	Program ProgramCommand `json:"program"`
}

type ProgramCommand struct {
	Profile string   `json:"profile,omitempty"`
	Run     string   `json:"run"`
	Args    []string `json:"args,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

type ProgramOutcome string

const (
	ProgramSucceeded ProgramOutcome = "succeeded"
	ProgramFailed    ProgramOutcome = "failed"
)

type ProgramObservation struct {
	CommandID string         `json:"commandId"`
	NodeID    string         `json:"nodeId"`
	Outcome   ProgramOutcome `json:"outcome"`
	ExitCode  int            `json:"exitCode"`
	Error     string         `json:"error,omitempty"`
}

type TransitionKind string

const (
	TransitionAdvance         TransitionKind = "advance"
	TransitionCommandPlanned  TransitionKind = "command_planned"
	TransitionProgramObserved TransitionKind = "program_observed"
)

// Transition is an explicit reducer input. Exactly one payload is allowed for
// payload-bearing kinds; plain advance carries neither payload.
type Transition struct {
	Kind        TransitionKind
	Command     *Command
	Observation *ProgramObservation
}

var (
	ErrTemplateIneligible        = errors.New("process template is not executable by the sequential engine")
	ErrInvalidProgramBinding     = errors.New("invalid bound program command")
	ErrInvalidCheckpoint         = errors.New("invalid process checkpoint")
	ErrInvalidTransition         = errors.New("invalid process transition")
	ErrStaleObservation          = errors.New("stale process command observation")
	ErrTransitionBudgetExhausted = errors.New("process engine transition budget exhausted")
)

type EligibilityError struct {
	Diagnostics model.Diagnostics
}

func (e *EligibilityError) Error() string {
	if e == nil || len(e.Diagnostics) == 0 {
		return ErrTemplateIneligible.Error()
	}
	first := e.Diagnostics[0]
	if first.Path == "" {
		return ErrTemplateIneligible.Error() + ": " + first.Message
	}
	return ErrTemplateIneligible.Error() + ": " + first.Path + ": " + first.Message
}

func (e *EligibilityError) Unwrap() error { return ErrTemplateIneligible }
