package engine

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const CheckpointVersion = 2

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
	// NodeSkipped marks a node every path to which was closed by an exclusive
	// decision. Skipped nodes can never activate for the rest of the run.
	NodeSkipped NodeStatus = "skipped"
)

// EdgeDisposition is the durable per-run state of one authored edge. An edge
// is identified by its authored (from node, outcome label) pair; the target is
// determined by the immutable definition.
type EdgeDisposition string

const (
	EdgeUnresolved EdgeDisposition = "unresolved"
	EdgeArrived    EdgeDisposition = "arrived"
	EdgeNotTaken   EdgeDisposition = "not_taken"
)

type CommandKind string

const CommandProgram CommandKind = "program"

// Checkpoint is the complete v2 reducer state. The pinned template and run
// parameters live beside it in the run record, rather than being copied into
// every transition.
type Checkpoint struct {
	Version int                   `json:"version"`
	RunID   string                `json:"runId"`
	Status  RunStatus             `json:"status"`
	Nodes   map[string]NodeStatus `json:"nodes"`
	// Edges holds one disposition per authored edge, keyed by source node and
	// then outcome label — the same (from, outcome) identity the authoring
	// model and editor layout already use. Nesting avoids inventing an escaped
	// composite key for free-form outcome labels.
	Edges map[string]map[string]EdgeDisposition `json:"edges"`
	// AwaitingDecision is the one durable manual obligation this slice can
	// produce: the ready decision node blocking the run. It exists so bounded
	// store queries can exclude decision-waiting runs without loading them;
	// the verdict vocabulary stays in the prepared Definition.
	AwaitingDecision   *DecisionObligation `json:"awaitingDecision,omitempty"`
	OutstandingCommand *Command            `json:"outstandingCommand,omitempty"`
}

// DecisionObligation names the decision node the run is durably waiting on.
// Together with the run id it is the narrow identity decision input binds to.
type DecisionObligation struct {
	NodeID string `json:"nodeId"`
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

// DecisionRecord is the reducer payload for one authored exclusive decision:
// the deciding verdict must name exactly one authored outcome edge of the
// awaited decision node.
type DecisionRecord struct {
	NodeID  string `json:"nodeId"`
	Verdict string `json:"verdict"`
}

// ChosenEdge is the structural authored edge a decision selected, kept as its
// (from, outcome, to) parts for evidence rather than a concatenated label.
type ChosenEdge struct {
	From    string `json:"from"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

type TransitionKind string

const (
	TransitionAdvance          TransitionKind = "advance"
	TransitionCommandPlanned   TransitionKind = "command_planned"
	TransitionProgramObserved  TransitionKind = "program_observed"
	TransitionDecisionRecorded TransitionKind = "decision_recorded"
)

// Transition is an explicit reducer input. Exactly one payload is allowed for
// payload-bearing kinds; plain advance carries neither payload.
type Transition struct {
	Kind        TransitionKind
	Command     *Command
	Observation *ProgramObservation
	Decision    *DecisionRecord
}

var (
	ErrTemplateIneligible        = errors.New("process template is not executable by the exclusive-decision engine")
	ErrInvalidProgramBinding     = errors.New("invalid bound program command")
	ErrInvalidDefinition         = errors.New("invalid prepared process definition")
	ErrInvalidCheckpoint         = errors.New("invalid process checkpoint")
	ErrInvalidTransition         = errors.New("invalid process transition")
	ErrStaleObservation          = errors.New("stale process command observation")
	ErrStaleDecision             = errors.New("stale or duplicate process decision input")
	ErrInvalidDecisionVerdict    = errors.New("process decision verdict does not name an authored outcome")
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
