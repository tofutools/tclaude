package engine

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const CheckpointVersion = 2

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
	// AwaitingDecisions and Commands are the durable plural outboxes. With
	// fan-out they genuinely hold one entry per branch that is waiting on an
	// external actor. Every reducer mutation is per-entry: planning writes only
	// the planned node's command, an observation removes only its exact command,
	// and a verdict removes only its exact obligation, so branches never clear
	// each other. Entries are kept sorted by node id so one logical state has
	// one durable encoding.
	//
	// AwaitingDecisions names the ready decision nodes blocking a branch so
	// bounded store queries can exclude decision-waiting runs without loading
	// them; the verdict vocabulary stays in the prepared Definition. Commands is
	// the durable outbox of fully bound program requests.
	AwaitingDecisions []DecisionObligation `json:"awaitingDecisions"`
	Commands          []Command            `json:"commands"`
}

// FirstCommand returns the lowest-node-id entry of the command outbox, or nil.
//
// It is a presentation accessor for surfaces that show one item at a time. It
// is NOT a reducer input and must never be used to decide what to observe or
// dispatch: with fan-out the outbox can hold one command per branch, so acting
// on the first entry would silently pick a branch. Transitions bind to an exact
// (command id, node id) pair instead.
func (c Checkpoint) FirstCommand() *Command {
	if len(c.Commands) == 0 {
		return nil
	}
	command := c.Commands[0]
	return &command
}

// FirstAwaitingDecision returns the lowest-node-id awaited decision, or nil.
// Like FirstCommand it is a presentation accessor over a plural outbox that can
// hold one obligation per branch, never a reducer input; a verdict names the
// obligation it resolves.
func (c Checkpoint) FirstAwaitingDecision() *DecisionObligation {
	if len(c.AwaitingDecisions) == 0 {
		return nil
	}
	obligation := c.AwaitingDecisions[0]
	return &obligation
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
// payload-bearing kinds; a plain advance carries no payload and instead names
// the engine-owned node it advances.
//
// Every transition is node-addressed: NodeID for an advance, and the node named
// by the command, observation, or decision otherwise. Under fan-out more than
// one node can be ready at once, so the reducer must never infer which node a
// transition means.
type Transition struct {
	Kind        TransitionKind
	NodeID      string
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
