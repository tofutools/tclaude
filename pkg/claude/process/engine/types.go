package engine

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const CheckpointVersion = 3

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
	// NodeBlocked marks a task whose author explicitly asked for retries and
	// whose attempt budget is now spent while the run is still live. It is a
	// PARKED branch, not a doom marker: unaffected siblings keep executing, the
	// run stays running, and only an operator resolution moves it again.
	NodeBlocked NodeStatus = "blocked"
	// NodeCanceled marks the blocked node an operator resolved with cancel. That
	// resolution dooms the run, so this is the local record of which node the
	// operator gave up on while already-dispatched sibling commands drain.
	NodeCanceled NodeStatus = "canceled"
)

// EdgeDisposition is the durable per-run state of one authored edge. An edge
// is identified by its authored (from node, outcome label) pair; the target is
// determined by the immutable definition.
type EdgeDisposition string

const (
	EdgeUnresolved EdgeDisposition = "unresolved"
	EdgeArrived    EdgeDisposition = "arrived"
	EdgeNotTaken   EdgeDisposition = "not_taken"
	// EdgeArrivedLate records a real arrival at a join: any reducer that had
	// already been won. The source genuinely routed here — that is why it is not
	// not_taken — but the join was decided before this branch got there.
	//
	// It is the durable winner fact, held in the edge state that already exists
	// rather than in a second copy somewhere else: a join: any node has AT MOST
	// ONE incoming edge with EdgeArrived, and that edge is the winner. Every
	// later arrival at the same join settles as EdgeArrivedLate. Only an incoming
	// edge of a join: any node may ever hold it, which the load boundary checks.
	EdgeArrivedLate EdgeDisposition = "arrived_late"
)

type CommandKind string

const CommandProgram CommandKind = "program"

// Checkpoint is the complete v3 reducer state. The pinned template and run
// parameters live beside it in the run record, rather than being copied into
// every transition.
type Checkpoint struct {
	Version int                   `json:"version"`
	RunID   string                `json:"runId"`
	Status  RunStatus             `json:"status"`
	Nodes   map[string]NodeStatus `json:"nodes"`
	// Attempts is the sparse, monotonic attempt counter of every node that has
	// actually executed. It holds one entry per node the reducer has planned a
	// command for, is never reset or reused, and is what makes attempt identity
	// outlive an attempt: the counter advances exactly once per planned command,
	// so a delayed attempt-N observation can never bind to attempt N+1.
	//
	// It is not attempt history. A node's entry is its CURRENT attempt number,
	// and the durable command for that node — if any — is the command of that
	// exact attempt. Nodes that never ran are simply absent, so a run without
	// authored retries and no dispatch yet encodes nothing at all.
	Attempts map[string]int `json:"attempts,omitempty"`
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
	// Blocked is the third durable outbox: one entry per branch parked on an
	// operator after an authored retry budget ran out. It follows the same
	// per-entry, sorted-by-node-id rules as the other two, and it is sparse —
	// a run that never exhausted a budget encodes nothing at all.
	//
	// It carries no owner, no time, and no attempt. Routing and worklists are
	// TCL-651's; the parking time is in the node_blocked evidence row; and the
	// exact blocked attempt is always Attempts[nodeId], which cannot move while
	// the node is blocked. Duplicating any of those here would create a second
	// copy of a fact that already has an authority.
	Blocked []BlockedObligation `json:"blocked,omitempty"`
	// AttemptCeilings is the sparse, monotonic per-node override of the authored
	// attempt budget. An absent entry means the prepared authored maxAttempts,
	// which is why a run with no operator retry encodes nothing here.
	//
	// It only ever rises, and only by one operator retry resolution: the ceiling
	// becomes Attempts[nodeId] + the authored maxAttempts, opening one fresh
	// authored-size window WITHOUT resetting or reusing an attempt number. That
	// is what keeps attempt identity monotonic across an operator retry.
	AttemptCeilings map[string]int `json:"attemptCeilings,omitempty"`
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

// MaxBlockedReasonBytes bounds the human-facing reason copied out of the
// exhausted observation into durable state. The observation's own error text is
// already bounded far more generously, so this is deliberately small: the
// obligation is a pointer to a parked branch, not a place to store output.
const MaxBlockedReasonBytes = 1024

// BlockedObligation names one branch parked on an operator, and why. Together
// with the run id and Attempts[NodeID] it is the exact identity a resolution
// binds to.
type BlockedObligation struct {
	NodeID string `json:"nodeId"`
	Reason string `json:"reason,omitempty"`
}

// ResolutionAction is what an operator decided to do about one blocked branch.
type ResolutionAction string

const (
	// ResolveRetry opens one fresh authored-size attempt window and re-readies
	// the node. Ordinary planning then mints attempt N+1; nothing here dispatches.
	ResolveRetry ResolutionAction = "retry"
	// ResolveSkip settles the task through its sole authored route, activating
	// downstream work exactly as a successful program would have. Only the
	// evidence distinguishes the two, which is why it is recorded.
	ResolveSkip ResolutionAction = "skip"
	// ResolveCancel gives up on the run: the node is canceled, every parked
	// obligation is dropped, and the run drains its in-flight commands before
	// finishing RunCanceled.
	ResolveCancel ResolutionAction = "cancel"
)

// BlockedResolution is the reducer payload for one operator resolution. It is
// bound to the exact blocked identity — the node and the attempt that exhausted
// the budget — so a stale, duplicated, or wrong-branch resolution is refused.
type BlockedResolution struct {
	NodeID  string           `json:"nodeId"`
	Attempt int              `json:"attempt"`
	Action  ResolutionAction `json:"action"`
}

// Command is the one durable outbox item this sequential slice can produce.
// Program contains the fully bound request so dispatch never has to reread
// mutable authoring input.
//
// Attempt is the node's attempt this request belongs to, counted from 1, and it
// is bound into the deterministic ID as well as carried as its own field. That
// is what stops a retried node from minting the same identity twice: exact
// outbox matching stays the stale-input authority, and the identity it matches
// on is now per attempt rather than per node.
type Command struct {
	ID      string         `json:"id"`
	Kind    CommandKind    `json:"kind"`
	NodeID  string         `json:"nodeId"`
	Attempt int            `json:"attempt"`
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
	// TransitionBlockedResolved is the ONE transition every operator resolution
	// takes. retry, skip, and cancel share their preconditions exactly, and
	// differ only in what they then do with the parked branch, so they are one
	// payload with an action rather than three near-identical transitions.
	TransitionBlockedResolved TransitionKind = "blocked_resolved"
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
	Resolution  *BlockedResolution
}

var (
	ErrTemplateIneligible        = errors.New("process template is not executable by this engine")
	ErrInvalidProgramBinding     = errors.New("invalid bound program command")
	ErrInvalidDefinition         = errors.New("invalid prepared process definition")
	ErrInvalidCheckpoint         = errors.New("invalid process checkpoint")
	ErrInvalidTransition         = errors.New("invalid process transition")
	ErrStaleObservation          = errors.New("stale process command observation")
	ErrStaleDecision             = errors.New("stale or duplicate process decision input")
	ErrStaleResolution           = errors.New("stale or duplicate process blocked resolution")
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
