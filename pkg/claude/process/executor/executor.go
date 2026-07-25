package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

type ActionKind string

const (
	ActionContinue       ActionKind = "continue"
	ActionDispatch       ActionKind = "dispatch"
	ActionAwaitDecision  ActionKind = "await_decision"
	ActionNeedsReconcile ActionKind = "needs_reconcile"
	// ActionBlocked reports a run whose remaining live work is parked on an
	// operator resolution. Without it a parked run has no honest coarse summary:
	// it is running, has nothing outstanding, and nothing an engine pass can
	// push, so a driver would have to call that state an error.
	ActionBlocked  ActionKind = "blocked"
	ActionTerminal ActionKind = "terminal"
)

// EngineActor attributes durable evidence the engine produced by itself — an
// advance, a prepared command, an observed program, or a decision obligation
// that became the run's own to hold. A human or agent caller is attributed by
// its authenticated identity instead; the distinction is what lets a reader of
// the evidence tell what somebody asked for from what the reducer then did.
// The daemon's creation boundary reuses it for an entry decision's obligation,
// which no advance was ever going to produce.
const EngineActor = "engine:program-executor"

var (
	ErrInvalidRun           = errors.New("invalid executable process run")
	ErrNeedsReconcile       = errors.New("process command needs explicit reconciliation")
	ErrNoReconciliation     = errors.New("process run has no command to reconcile")
	ErrAmbiguousReconcile   = errors.New("process run has more than one command to reconcile; name one")
	ErrStaleDispatch        = errors.New("stale or already-used process dispatch")
	ErrInvalidActor         = errors.New("reconciliation actor is invalid")
	ErrInvalidDecisionInput = errors.New("invalid process decision input")

	ErrInvalidResolutionInput = errors.New("invalid process blocked resolution input")
)

// Decision input mirrors the evidence-payload scale used elsewhere: verdicts
// are authored outcome labels and evidence is short human prose.
const (
	MaxDecisionNodeIDBytes   = 256
	MaxDecisionVerdictBytes  = 1024
	MaxDecisionEvidenceBytes = 4096
	// MaxResolutionNoteBytes bounds the optional operator note recorded with one
	// blocked resolution, at the same scale as decision evidence.
	MaxResolutionNoteBytes = 4096
)

// Run is one cold-reconstructed run. The immutable Definition is prepared
// exactly once here and reused by every transition until this value is dropped.
//
// A Run has exactly ONE owner: whichever goroutine drives it. Every field here,
// including the live dispatch table, is read and written only by that owner.
// Program workers never receive the Run — they are handed an immutable Dispatch
// and hand back a Result.
type Run struct {
	id           string
	stateVersion int64
	checkpoint   engine.Checkpoint
	definition   *engine.Definition
	authorized   map[string]struct{}
	// dispatches holds the live in-memory permission for each durable outbox
	// entry this process is still accounting for, keyed by node id (the outbox
	// key). An outbox entry with no live permission is exactly the ambiguous,
	// explicitly reconcilable case: a cold load, a worker that never reported,
	// or an observation that failed to commit.
	dispatches map[string]*Dispatch
}

type Action struct {
	Kind     ActionKind
	Status   engine.RunStatus
	Command  *engine.Command
	Decision *engine.DecisionObligation
}

// CommandState is one durable outbox entry plus whether this process still
// holds a live permission accounting for it.
type CommandState struct {
	Command engine.Command
	// Executing is true while the owner still holds a live permission for this
	// entry — it has been handed to a worker, or is about to be. False means
	// the entry is ambiguous and needs explicit reconciliation.
	Executing bool
}

// Dispatch is an in-memory permission to execute a command whose complete
// request has already committed. It cannot be constructed outside this package.
//
// It is one-shot, and enforced as such rather than by convention: `claimed` is
// taken by the owner in Claim, and `spent` is taken by Perform immediately
// before the external program starts. Both are atomic compare-and-swaps, so a
// second attempt — sequential or concurrent, correct caller or not — loses the
// race and is refused before any side effect. A failed or cancelled program
// does not give the permission back; reissuing is a durable operator decision.
//
// Everything else here is written once, before the permission reaches a worker,
// so a worker can read its command without synchronization and without ever
// touching the Run.
type Dispatch struct {
	owner   *Run
	runID   string
	command engine.Command
	claimed atomic.Bool
	spent   atomic.Bool
}

// Authorization is the concrete decision supplied by the daemon-owned caller.
// Policy and future sandbox selection deliberately remain outside this slice.
type Authorization struct {
	RunID   string
	Profile string
}

type RecordedOutcome struct {
	Outcome  engine.ProgramOutcome
	ExitCode int
	Error    string
	Note     string
}

// DecisionInput is one attempt at resolving the run's awaited decision. It is
// bound to the durable obligation identity (run id + decision node id); the
// store's state-version compare-and-swap serializes concurrent attempts.
type DecisionInput struct {
	NodeID   string
	Verdict  string
	Evidence string
}

// ResolutionInput is one attempt at resolving a parked branch. It is bound to
// the exact blocked identity (run id + node id + the attempt that exhausted the
// budget); the store's state-version compare-and-swap serializes concurrent
// attempts, and the attempt match refuses input formed against a state the run
// has already left behind.
type ResolutionInput struct {
	NodeID  string
	Attempt int
	Action  engine.ResolutionAction
	Note    string
}

func (r *Run) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

func (r *Run) StateVersion() int64 {
	if r == nil {
		return 0
	}
	return r.stateVersion
}

// AuthorizationFor returns the concrete authorization token Execute expects
// only when that exact program profile was explicitly persisted for this run.
// The daemon uses this after every restart; template contents never mint the
// decision.
func (r *Run) AuthorizationFor(profile string) (Authorization, bool) {
	if r == nil {
		return Authorization{}, false
	}
	if _, ok := r.authorized[profile]; !ok {
		return Authorization{}, false
	}
	return Authorization{RunID: r.id, Profile: profile}, true
}

// Action reports the single coarse thing a driver should do with this run next.
//
// Under bounded concurrency a run can hold several commands and several awaited
// decisions at once, so this is a SUMMARY, never an inventory. Commands() and
// AwaitingDecisions() are the plural truth; a caller that acts on a specific
// branch must go through those, or it would silently pick one.
//
// The order is the order of operator relevance: an ambiguous command needs a
// human before anything else, live work outranks plannable work, and an awaited
// decision is only the headline once nothing is executing or plannable.
func (r *Run) Action() Action {
	if r == nil {
		return Action{}
	}
	action := Action{Status: r.checkpoint.Status}
	for _, state := range r.Commands() {
		if !state.Executing {
			command := state.Command
			action.Kind, action.Command = ActionNeedsReconcile, &command
			return action
		}
	}
	if len(r.checkpoint.Commands) > 0 {
		command := cloneCommand(r.checkpoint.Commands[0])
		action.Kind, action.Command = ActionDispatch, &command
		return action
	}
	if engine.Runnable(r.checkpoint, r.definition) {
		action.Kind = ActionContinue
		return action
	}
	if len(r.checkpoint.AwaitingDecisions) > 0 {
		obligation := r.checkpoint.AwaitingDecisions[0]
		action.Kind = ActionAwaitDecision
		action.Decision = &obligation
		return action
	}
	if len(r.checkpoint.Blocked) > 0 {
		action.Kind = ActionBlocked
		return action
	}
	if r.checkpoint.Status == engine.RunRunning {
		action.Kind = ActionContinue
	} else {
		action.Kind = ActionTerminal
	}
	return action
}

// Draining reports whether this run is already doomed — a task failed outright
// or an operator canceled one — so it can only drain. A driver reads it after
// committing an input to decide whether the run is over: with authored retries
// a failed program is ordinarily just one attempt ending, and an exhausted
// branch that parked on an operator has doomed nothing, so neither of those is
// a reason to tear the run's siblings down.
func (r *Run) Draining() bool {
	if r == nil {
		return false
	}
	return engine.Draining(r.checkpoint)
}

// Commands reports every durable outbox entry with whether this process still
// accounts for it. It is the plural read every presentation and reconciliation
// surface uses.
func (r *Run) Commands() []CommandState {
	if r == nil || len(r.checkpoint.Commands) == 0 {
		return nil
	}
	states := make([]CommandState, 0, len(r.checkpoint.Commands))
	for i := range r.checkpoint.Commands {
		command := cloneCommand(r.checkpoint.Commands[i])
		_, live := r.dispatches[command.NodeID]
		states = append(states, CommandState{Command: command, Executing: live})
	}
	return states
}

// AwaitingDecisions reports every branch currently parked on a human.
func (r *Run) AwaitingDecisions() []engine.DecisionObligation {
	if r == nil || len(r.checkpoint.AwaitingDecisions) == 0 {
		return nil
	}
	return append([]engine.DecisionObligation(nil), r.checkpoint.AwaitingDecisions...)
}

// Blocked reports every branch currently parked on an operator.
func (r *Run) Blocked() []engine.BlockedObligation {
	if r == nil || len(r.checkpoint.Blocked) == 0 {
		return nil
	}
	return append([]engine.BlockedObligation(nil), r.checkpoint.Blocked...)
}

// BlockedAttempt reports the exact attempt one parked branch is blocked at, or
// zero when that node is not blocked. It is the attempt a resolution has to
// name, derived from the checkpoint's own counter rather than from evidence.
func (r *Run) BlockedAttempt(nodeID string) int {
	if r == nil || r.checkpoint.Nodes[nodeID] != engine.NodeBlocked {
		return 0
	}
	return r.checkpoint.Attempts[nodeID]
}

// VerdictsFor exposes the prepared verdict vocabulary of one awaited decision,
// or nil when that node is not a prepared decision.
func (r *Run) VerdictsFor(nodeID string) []string {
	if r == nil || r.definition == nil {
		return nil
	}
	verdicts, _ := r.definition.DecisionVerdicts(nodeID)
	return verdicts
}

// LoadRun is the cold reconstruction boundary. Evidence is not read. Any
// cold-loaded outstanding command is ambiguous and therefore needs reconcile.
func LoadRun(runID string) (*Run, error) {
	record, err := db.GetProcessRun(runID)
	if err != nil {
		return nil, err
	}
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		return nil, fmt.Errorf("%w: decode template snapshot: %v", ErrInvalidRun, err)
	}
	var params map[string]string
	if err := record.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRun, err)
	}
	definition, err := engine.Prepare(&tmpl, params)
	if err != nil {
		return nil, fmt.Errorf("%w: prepare definition: %v", ErrInvalidRun, err)
	}
	return LoadPreparedRun(record, definition)
}

// LoadPreparedRun reconstructs a newly committed run with the exact Definition
// already prepared for its creation transaction. It is intentionally narrow:
// cold recovery still goes through LoadRun and prepares from the persisted
// immutable snapshot.
func LoadPreparedRun(record *db.ProcessRun, definition *engine.Definition) (*Run, error) {
	if record == nil || definition == nil || record.StateVersion <= 0 {
		return nil, ErrInvalidRun
	}
	var authorizationProfiles []string
	if err := record.DecodeProgramAuthorizations(&authorizationProfiles); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRun, err)
	}
	authorized := make(map[string]struct{}, len(authorizationProfiles))
	for _, profile := range authorizationProfiles {
		authorized[profile] = struct{}{}
	}
	checkpoint, err := engine.DecodeCheckpoint(record.CheckpointJSON, definition)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRun, err)
	}
	if checkpoint.RunID != record.ID || string(checkpoint.Status) != record.Status {
		return nil, fmt.Errorf("%w: checkpoint identity or status disagrees with run row", ErrInvalidRun)
	}
	return &Run{
		id: record.ID, stateVersion: record.StateVersion,
		checkpoint: checkpoint, definition: definition, authorized: authorized,
	}, nil
}

// Prepare advances only reducer-owned state and atomically persists the fully
// bound command before returning a dispatch permission. It commits AT MOST ONE
// command per call: a bounded concurrent driver calls it repeatedly until it
// has filled its capacity or the call returns nil, and the ready branches it
// did not reach stay ready until a slot frees up.
//
// It refuses outright while any outbox entry is unaccounted for. Such an entry
// may name a program that really ran, so planning more work past it would build
// on a state nobody has confirmed.
func Prepare(run *Run) (*Dispatch, error) {
	if run == nil || run.definition == nil {
		return nil, ErrInvalidRun
	}
	if _, ok := run.firstUnaccountedCommand(); ok {
		return nil, ErrNeedsReconcile
	}
	if run.checkpoint.Status != engine.RunRunning {
		return nil, nil
	}
	next, command, advanced, err := engine.AdvanceAndPlan(run.checkpoint, run.definition)
	if err != nil {
		return nil, err
	}
	if !advanced {
		// Quiescent: every live branch is waiting on an external actor, so there
		// is nothing to commit and a resume must not bump the state version.
		return nil, nil
	}
	// An advance has no triggering input of its own: the settlement it commits
	// is what wins a join, and everything advanceEvidence reports — a parked
	// branch, a planned command — is downstream of that.
	events := commitEvidence(run.definition, run.checkpoint, next,
		nil, advanceEvidence(run.checkpoint, next, command))
	if err := persistEvents(run, next, events); err != nil {
		return nil, err
	}
	if command == nil {
		return nil, nil
	}
	return run.grant(*command), nil
}

// Reissue durably records the operator's explicit retry decision before it
// returns a fresh dispatch permission. It does not execute the program.
//
// selector names which outbox entry the operator meant, by node id. It may be
// empty only while exactly one entry needs reconciliation.
func Reissue(run *Run, actor, selector string) (*Dispatch, error) {
	if err := validateReconciliationActor(actor); err != nil {
		return nil, err
	}
	command, err := reconcileCommand(run, selector)
	if err != nil {
		return nil, err
	}
	payload := struct {
		Decision  string `json:"decision"`
		CommandID string `json:"commandId"`
	}{Decision: "reissue", CommandID: command.ID}
	if err := persist(run, run.checkpoint, event("program_reissued", &command, actor, payload)); err != nil {
		return nil, err
	}
	return run.grant(command), nil
}

// RecordOutcome durably applies an operator-supplied outcome to one ambiguous
// command, named by selector exactly as in Reissue. It is the only
// reconciliation path other than explicit reissue.
func RecordOutcome(run *Run, actor, selector string, outcome RecordedOutcome) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	command, err := reconcileCommand(run, selector)
	if err != nil {
		return err
	}
	observation := engine.ProgramObservation{
		CommandID: command.ID, NodeID: command.NodeID,
		Outcome: outcome.Outcome, ExitCode: outcome.ExitCode, Error: outcome.Error,
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionProgramObserved, Observation: &observation,
	})
	if err != nil {
		return err
	}
	payload := struct {
		Decision    string                    `json:"decision"`
		Observation engine.ProgramObservation `json:"observation"`
		Note        string                    `json:"note,omitempty"`
	}{Decision: "record_outcome", Observation: observation, Note: outcome.Note}
	events := commitEvidence(run.definition, run.checkpoint, next,
		[]db.ProcessRunEvent{event("program_outcome_recorded", &command, actor, payload)}, nil)
	return persistEvents(run, next, events)
}

// RecordDecision durably applies one manual decision verdict to the run's
// awaited decision node. The engine refuses stale, duplicate, or wrong-node
// input and verdicts outside the authored outcome vocabulary; the checkpoint,
// chosen/closed edge dispositions, and decision evidence commit in one
// state-version-guarded transaction. It never executes a program.
func RecordDecision(run *Run, actor string, input DecisionInput) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if err := validateDecisionInput(input); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind:     engine.TransitionDecisionRecorded,
		Decision: &engine.DecisionRecord{NodeID: input.NodeID, Verdict: input.Verdict},
	})
	if err != nil {
		return err
	}
	chosen, ok := run.definition.DecisionEdge(input.NodeID, input.Verdict)
	if !ok {
		return fmt.Errorf("%w: decision %q has no edge for verdict %q", ErrInvalidRun, input.NodeID, input.Verdict)
	}
	payload := struct {
		Verdict    string            `json:"verdict"`
		Evidence   string            `json:"evidence,omitempty"`
		ChosenEdge engine.ChosenEdge `json:"chosenEdge"`
	}{Verdict: input.Verdict, Evidence: input.Evidence, ChosenEdge: chosen}
	events := commitEvidence(run.definition, run.checkpoint, next,
		[]db.ProcessRunEvent{eventForNode("decision_recorded", input.NodeID, actor, payload)}, nil)
	return persistEvents(run, next, events)
}

// ResolveBlocked durably applies one operator resolution to a parked branch.
// The engine refuses stale, duplicate, wrong-node, wrong-attempt, and
// already-doomed input; the checkpoint and the resolution evidence commit in
// one state-version-guarded transaction. It never executes a program: a retry
// simply re-readies the node, and ordinary planning mints the next attempt.
func ResolveBlocked(run *Run, actor string, input ResolutionInput) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	if err := validateResolutionInput(input); err != nil {
		return err
	}
	if run == nil || run.definition == nil {
		return ErrInvalidRun
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionBlockedResolved,
		Resolution: &engine.BlockedResolution{
			NodeID: input.NodeID, Attempt: input.Attempt, Action: input.Action,
		},
	})
	if err != nil {
		return err
	}
	payload := struct {
		NodeID  string                  `json:"nodeId"`
		Attempt int                     `json:"attempt"`
		Action  engine.ResolutionAction `json:"action"`
		Note    string                  `json:"note,omitempty"`
	}{NodeID: input.NodeID, Attempt: input.Attempt, Action: input.Action, Note: input.Note}
	events := commitEvidence(run.definition, run.checkpoint, next,
		[]db.ProcessRunEvent{eventForNode("blocked_resolved", input.NodeID, actor, payload)}, nil)
	return persistEvents(run, next, events)
}

func validateResolutionInput(input ResolutionInput) error {
	if input.NodeID == "" || len(input.NodeID) > MaxDecisionNodeIDBytes || !utf8.ValidString(input.NodeID) {
		return fmt.Errorf("%w: node id must be 1..%d bytes of UTF-8", ErrInvalidResolutionInput, MaxDecisionNodeIDBytes)
	}
	for _, value := range input.NodeID {
		if unicode.IsControl(value) {
			return fmt.Errorf("%w: node id cannot contain control characters", ErrInvalidResolutionInput)
		}
	}
	// The attempt is the other half of the blocked identity, so an unusable one
	// is refused here rather than being compared against the counter.
	if input.Attempt < 1 {
		return fmt.Errorf("%w: attempt must be the positive attempt the node is blocked at", ErrInvalidResolutionInput)
	}
	switch input.Action {
	case engine.ResolveRetry, engine.ResolveSkip, engine.ResolveCancel:
	default:
		return fmt.Errorf("%w: action must be %s, %s, or %s",
			ErrInvalidResolutionInput, engine.ResolveRetry, engine.ResolveSkip, engine.ResolveCancel)
	}
	if len(input.Note) > MaxResolutionNoteBytes || !utf8.ValidString(input.Note) {
		return fmt.Errorf("%w: note must be at most %d bytes of UTF-8", ErrInvalidResolutionInput, MaxResolutionNoteBytes)
	}
	for _, value := range input.Note {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return fmt.Errorf("%w: note cannot contain control characters other than line breaks and tabs", ErrInvalidResolutionInput)
		}
	}
	return nil
}

func validateDecisionInput(input DecisionInput) error {
	if input.NodeID == "" || len(input.NodeID) > MaxDecisionNodeIDBytes || !utf8.ValidString(input.NodeID) {
		return fmt.Errorf("%w: node id must be 1..%d bytes of UTF-8", ErrInvalidDecisionInput, MaxDecisionNodeIDBytes)
	}
	if input.Verdict == "" || len(input.Verdict) > MaxDecisionVerdictBytes || !utf8.ValidString(input.Verdict) {
		return fmt.Errorf("%w: verdict must be 1..%d bytes of UTF-8", ErrInvalidDecisionInput, MaxDecisionVerdictBytes)
	}
	for _, value := range input.NodeID + input.Verdict {
		if unicode.IsControl(value) {
			return fmt.Errorf("%w: node id and verdict cannot contain control characters", ErrInvalidDecisionInput)
		}
	}
	if len(input.Evidence) > MaxDecisionEvidenceBytes || !utf8.ValidString(input.Evidence) {
		return fmt.Errorf("%w: evidence must be at most %d bytes of UTF-8", ErrInvalidDecisionInput, MaxDecisionEvidenceBytes)
	}
	for _, value := range input.Evidence {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return fmt.Errorf("%w: evidence cannot contain control characters other than line breaks and tabs", ErrInvalidDecisionInput)
		}
	}
	return nil
}

// reconcileCommand returns the one ambiguous command an operator action names.
//
// Only unaccounted entries are reconcilable: a command whose program this
// process is still running is not ambiguous, and letting an operator record an
// outcome for it would race the real observation.
//
// An empty selector is the single-command convenience. With more than one
// candidate it fails closed rather than reconciling a branch at random — the
// operator has to say which one they mean.
func reconcileCommand(run *Run, selector string) (engine.Command, error) {
	if run == nil || run.definition == nil {
		return engine.Command{}, ErrInvalidRun
	}
	var candidates []engine.Command
	for _, state := range run.Commands() {
		if !state.Executing {
			candidates = append(candidates, state.Command)
		}
	}
	if len(candidates) == 0 {
		return engine.Command{}, ErrNoReconciliation
	}
	if selector == "" {
		if len(candidates) > 1 {
			return engine.Command{}, ErrAmbiguousReconcile
		}
		return candidates[0], nil
	}
	for _, command := range candidates {
		if command.NodeID == selector {
			return command, nil
		}
	}
	return engine.Command{}, fmt.Errorf("%w: no command awaiting reconciliation for node %q",
		ErrNoReconciliation, selector)
}

// firstUnaccountedCommand names the deterministically first outbox entry this
// process no longer holds a live permission for.
func (r *Run) firstUnaccountedCommand() (engine.Command, bool) {
	for i := range r.checkpoint.Commands {
		if _, live := r.dispatches[r.checkpoint.Commands[i].NodeID]; !live {
			return cloneCommand(r.checkpoint.Commands[i]), true
		}
	}
	return engine.Command{}, false
}

// grant records a live permission for one durable command. The caller must
// already have committed that command.
func (r *Run) grant(command engine.Command) *Dispatch {
	dispatch := &Dispatch{owner: r, runID: r.id, command: cloneCommand(command)}
	if r.dispatches == nil {
		r.dispatches = make(map[string]*Dispatch, 1)
	}
	r.dispatches[command.NodeID] = dispatch
	return dispatch
}

// Abandon drops one live permission without touching durable state, leaving its
// command explicitly reconcilable exactly as a cold load would. The owner uses
// it when a branch's program can no longer be accounted for in memory — the
// worker reported no result, or its observation would not commit.
func Abandon(run *Run, dispatch *Dispatch) {
	if run == nil || dispatch == nil || dispatch.owner != run {
		return
	}
	if run.dispatches[dispatch.command.NodeID] == dispatch {
		delete(run.dispatches, dispatch.command.NodeID)
	}
}

// Claim binds a permission to the authorization its owner resolved and marks it
// spent, so no second worker can ever be started for the same command. It must
// be called by the run's single state owner; afterwards the Dispatch is
// immutable and Perform can read it from a worker goroutine.
func Claim(run *Run, dispatch *Dispatch, authorization Authorization) error {
	if run == nil || dispatch == nil || dispatch.owner != run {
		return ErrStaleDispatch
	}
	if run.dispatches[dispatch.command.NodeID] != dispatch {
		return ErrStaleDispatch
	}
	// The permission must still name a live durable outbox entry. Exact
	// identity, the same rule the reducer binds observations with.
	if _, ok := findCommand(run.checkpoint, dispatch.command.ID, dispatch.command.NodeID); !ok {
		return ErrStaleDispatch
	}
	if authorization.RunID != run.id || authorization.Profile != dispatch.command.Program.Profile {
		return ErrUnauthorized
	}
	// Last, and atomically: an unauthorized attempt must leave the permission
	// usable, but a second authorized claim must not.
	if !dispatch.claimed.CompareAndSwap(false, true) {
		return ErrStaleDispatch
	}
	return nil
}

func findCommand(checkpoint engine.Checkpoint, commandID, nodeID string) (engine.Command, bool) {
	for _, command := range checkpoint.Commands {
		if command.ID == commandID && command.NodeID == nodeID {
			return command, true
		}
	}
	return engine.Command{}, false
}

func validateReconciliationActor(actor string) error {
	if actor == "" || strings.TrimSpace(actor) != actor || len(actor) > db.MaxProcessRunEventActor || !utf8.ValidString(actor) {
		return ErrInvalidActor
	}
	for _, value := range actor {
		if unicode.IsControl(value) {
			return ErrInvalidActor
		}
	}
	return nil
}

func persist(run *Run, checkpoint engine.Checkpoint, evidence db.ProcessRunEvent) error {
	return persistEvents(run, checkpoint, []db.ProcessRunEvent{evidence})
}

func persistEvents(run *Run, checkpoint engine.Checkpoint, events []db.ProcessRunEvent) error {
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("encode process checkpoint: %w", err)
	}
	version, err := db.TransitionProcessRun(run.id, db.ProcessRunTransition{
		ExpectedStateVersion: run.stateVersion,
		Status:               string(checkpoint.Status),
		CheckpointJSON:       encoded,
		Events:               events,
	})
	if err != nil {
		return err
	}
	run.stateVersion = version
	run.checkpoint = checkpoint
	// Live permissions survive a commit by another branch — that is the whole
	// point of the plural outbox — but a permission whose command has left the
	// outbox is spent and must not linger as an accounted-for entry.
	for nodeID, dispatch := range run.dispatches {
		if _, ok := findCommand(checkpoint, dispatch.command.ID, nodeID); !ok {
			delete(run.dispatches, nodeID)
		}
	}
	return nil
}

func event(kind string, command *engine.Command, actor string, payload any) db.ProcessRunEvent {
	nodeID := ""
	if command != nil {
		nodeID = command.NodeID
	}
	return eventForNode(kind, nodeID, actor, payload)
}

func eventForNode(kind, nodeID, actor string, payload any) db.ProcessRunEvent {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("executor evidence payload is not JSON encodable: " + err.Error())
	}
	return db.ProcessRunEvent{
		OccurredAt: time.Now().UTC(), NodeID: nodeID, Kind: kind,
		PayloadJSON: encoded, Actor: actor,
	}
}

// advanceEvidence is the human-facing record of one engine-owned advance: the
// prepared command, plus one row per decision the advance newly parked a branch
// on. Fan-out can park several branches in a single advance, and each of those
// obligations is separately addressable, so each gets its own row rather than
// one row standing in for all of them. An advance that did neither leaves the
// plain status move.
func advanceEvidence(before, advanced engine.Checkpoint, command *engine.Command) []db.ProcessRunEvent {
	var events []db.ProcessRunEvent
	for _, obligation := range advanced.AwaitingDecisions {
		if slices.ContainsFunc(before.AwaitingDecisions, func(existing engine.DecisionObligation) bool {
			return existing.NodeID == obligation.NodeID
		}) {
			continue
		}
		events = append(events, eventForNode("decision_awaited", obligation.NodeID, EngineActor, struct {
			NodeID string `json:"nodeId"`
		}{NodeID: obligation.NodeID}))
	}
	if command != nil {
		events = append(events, event("program_prepared", command, EngineActor,
			preparedEvidence{Command: cloneCommand(*command)}))
	}
	if len(events) == 0 {
		events = append(events, event("engine_advanced", nil, EngineActor, struct {
			Status engine.RunStatus `json:"status"`
		}{Status: advanced.Status}))
	}
	return events
}

// commitEvidence assembles one transaction's evidence in the order a reader
// needs it to make sense, and keeps the whole batch inside the store's
// per-transition limit.
//
// Causal order: the input that caused the transition, then what that settled at
// the run's join: any reducers, then the downstream effects the engine advanced
// and planned. Emitting the join rows last would have the public history claim
// a downstream command was prepared before the join it depends on was won.
//
// The budget is what the rest of the commit leaves. Join history is optional —
// it must never be the reason an otherwise valid state transition is refused —
// so it is the part that yields, and it says so when it does.
func commitEvidence(definition *engine.Definition, before, after engine.Checkpoint,
	input, advance []db.ProcessRunEvent) []db.ProcessRunEvent {
	resets := stageResetEvidence(definition, before, after)
	blocked := blockedEvidence(before, after)
	joins := joinEvidence(definition, before, after,
		db.MaxProcessRunEventsPerTransition-len(input)-len(resets)-len(blocked)-len(advance))
	events := make([]db.ProcessRunEvent, 0, len(input)+len(resets)+len(blocked)+len(joins)+len(advance))
	events = append(events, input...)
	events = append(events, resets...)
	events = append(events, blocked...)
	events = append(events, joins...)
	events = append(events, advance...)
	// Evidence is constructed before its causal order is assembled: in
	// particular, downstream advance evidence exists before join evidence is
	// derived, even though the join must precede it in the public stream. Keep
	// occurredAt useful to human-facing readers by carrying a later timestamp
	// forward inside this committed batch. Sequence remains authoritative.
	for index := 1; index < len(events); index++ {
		if events[index].OccurredAt.Before(events[index-1].OccurredAt) {
			events[index].OccurredAt = events[index-1].OccurredAt
		}
	}
	return events
}

// blockedEvidence is the human-facing record of every branch this transition
// newly parked on an operator: which node, the exact attempt that exhausted the
// budget, and the reason carried out of that attempt's failed observation.
//
// It is derived from the before/after checkpoints — the same seam
// advanceEvidence uses for a newly parked decision — so the row is written in
// the SAME transaction that creates the obligation and neither can exist
// without the other. It is also where the parking TIME lives, which is why the
// durable obligation does not carry one.
//
// This is a bounded scan of the after-state's blocked outbox, which the
// prepared node count bounds; a transition that parked nobody returns on the
// first line. It is never read back: SQLite's checkpoint stays authoritative.
func blockedEvidence(before, after engine.Checkpoint) []db.ProcessRunEvent {
	if len(after.Blocked) == 0 {
		return nil
	}
	var events []db.ProcessRunEvent
	for _, obligation := range after.Blocked {
		if slices.ContainsFunc(before.Blocked, func(existing engine.BlockedObligation) bool {
			return existing.NodeID == obligation.NodeID
		}) {
			continue
		}
		events = append(events, eventForNode("node_blocked", obligation.NodeID, EngineActor, blockedEvidencePayload{
			NodeID: obligation.NodeID, Attempt: after.Attempts[obligation.NodeID], Reason: obligation.Reason,
		}))
	}
	return events
}

type blockedEvidencePayload struct {
	NodeID  string `json:"nodeId"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason,omitempty"`
}

// stageResetEvidence is the human-facing record of every compound rework this
// transition performed: a failed check or review gate — or an operator retrying
// a parked one — sent the work back, so the do stage runs again and the stages
// from there through that gate run again with it.
//
// It is derived from the before/after checkpoints, the same seam blocked and
// join evidence use, so the row commits in the SAME transaction as the
// observation or resolution that caused it and neither can exist without the
// other. It is deliberately COMPACT: four scalars naming the parent, the gate,
// the work, and the attempt the work will next carry. It does not enumerate
// which children were reset — the prepared child list already says that, and
// re-deriving it into evidence would make history look like a replay source —
// and it copies no program feedback, which stays in the gate attempt's own
// bounded program_observed row.
//
// The row is attributed to the compound PARENT: the reset is a fact about that
// compound's stage sequence, while the gate's own failure already has its
// program_observed row on the gate.
func stageResetEvidence(definition *engine.Definition, before, after engine.Checkpoint) []db.ProcessRunEvent {
	resets := definition.StageResets(before, after)
	if len(resets) == 0 {
		return nil
	}
	events := make([]db.ProcessRunEvent, 0, len(resets))
	for _, reset := range resets {
		events = append(events, eventForNode("stage_reset", reset.ParentNodeID, EngineActor, stageResetEvidencePayload{
			ParentNodeID: reset.ParentNodeID, GateNodeID: reset.GateNodeID,
			WorkNodeID: reset.WorkNodeID, NextWorkAttempt: reset.NextWorkAttempt,
		}))
	}
	return events
}

type stageResetEvidencePayload struct {
	ParentNodeID    string `json:"parentNodeId"`
	GateNodeID      string `json:"gateNodeId"`
	WorkNodeID      string `json:"workNodeId"`
	NextWorkAttempt int    `json:"nextWorkAttempt"`
}

// joinEvidence is the human-facing record of what one committed transition did
// to the run's join: any reducers: which branch won a reducer, and which
// branches got there once it was already won.
//
// It is history, not authority — nothing reads it back, and the durable edge
// dispositions remain the only winner fact. Deriving it from before/after
// checkpoints is the same seam advanceEvidence already uses for a newly parked
// decision; the walk itself is the definition's prepared join: any edge index,
// so a template without one costs a nil check.
//
// budget is how many rows the rest of the commit left. A settlement pass can
// settle an authored fork's whole branch set at once — up to the normalized
// degree ceiling — beside one row per branch the same pass parked on a human,
// so the arrivals are what gets truncated rather than what pushes the
// transaction over db.MaxProcessRunEventsPerTransition and refuses it. As many
// deterministic arrival rows as fit are recorded; when any are omitted, the last
// remaining slot carries the exact counts, so a truncated history says so
// instead of quietly looking complete.
func joinEvidence(definition *engine.Definition, before, after engine.Checkpoint, budget int) []db.ProcessRunEvent {
	arrivals := definition.JoinArrivals(before, after)
	if len(arrivals) == 0 || budget <= 0 {
		return nil
	}
	recorded := len(arrivals)
	if recorded > budget {
		// The last slot goes to the summary, so a budget of one buys either one
		// arrival or the honest statement that there were more.
		recorded = budget - 1
	}
	events := make([]db.ProcessRunEvent, 0, min(len(arrivals), budget))
	won, late := 0, 0
	for index, arrival := range arrivals {
		if arrival.Winner {
			won++
		} else {
			late++
		}
		if index >= recorded {
			continue
		}
		kind := "join_arrival_late"
		if arrival.Winner {
			kind = "join_won"
		}
		events = append(events, eventForNode(kind, arrival.JoinNodeID, EngineActor, joinArrivalEvidence{
			JoinNodeID: arrival.JoinNodeID, From: arrival.From, Outcome: arrival.Outcome,
		}))
	}
	if len(arrivals) > recorded {
		events = append(events, eventForNode("join_arrivals_truncated", "", EngineActor, struct {
			Settled  int `json:"settled"`
			Recorded int `json:"recorded"`
			Won      int `json:"won"`
			Late     int `json:"late"`
		}{Settled: len(arrivals), Recorded: recorded, Won: won, Late: late}))
	}
	return events
}

type joinArrivalEvidence struct {
	JoinNodeID string `json:"joinNodeId"`
	From       string `json:"from"`
	Outcome    string `json:"outcome"`
}

type preparedEvidence struct {
	Command engine.Command `json:"command"`
}

func cloneCommand(command engine.Command) engine.Command {
	command.Program.Args = append([]string(nil), command.Program.Args...)
	return command
}

// Command returns the exact command this permission will execute. Callers that
// need to act on the command being dispatched — selecting its authorization,
// for instance — must read it from here rather than from a run-level view: a
// view surfaces one entry out of a plural outbox, so acting on that entry would
// silently pick a branch.
func (d *Dispatch) Command() engine.Command {
	if d == nil {
		return engine.Command{}
	}
	return cloneCommand(d.command)
}

// RunID is the run this permission belongs to. Perform reads it to bind the
// program's environment without reaching into the owner's Run.
func (d *Dispatch) RunID() string {
	if d == nil {
		return ""
	}
	return d.runID
}
