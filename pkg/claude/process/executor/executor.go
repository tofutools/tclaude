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
	ActionTerminal       ActionKind = "terminal"
)

const executorActor = "engine:program-executor"

var (
	ErrInvalidRun           = errors.New("invalid executable process run")
	ErrNeedsReconcile       = errors.New("process command needs explicit reconciliation")
	ErrNoReconciliation     = errors.New("process run has no command to reconcile")
	ErrAmbiguousReconcile   = errors.New("process run has more than one command to reconcile; name one")
	ErrStaleDispatch        = errors.New("stale or already-used process dispatch")
	ErrInvalidActor         = errors.New("reconciliation actor is invalid")
	ErrInvalidDecisionInput = errors.New("invalid process decision input")
)

// Decision input mirrors the evidence-payload scale used elsewhere: verdicts
// are authored outcome labels and evidence is short human prose.
const (
	MaxDecisionNodeIDBytes   = 256
	MaxDecisionVerdictBytes  = 1024
	MaxDecisionEvidenceBytes = 4096
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
	if r.checkpoint.Status == engine.RunRunning {
		action.Kind = ActionContinue
	} else {
		action.Kind = ActionTerminal
	}
	return action
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
	if err := persistEvents(run, next, advanceEvidence(run.checkpoint, next, command)); err != nil {
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
	return persist(run, next, event("program_outcome_recorded", &command, actor, payload))
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
	return persist(run, next, eventForNode("decision_recorded", input.NodeID, actor, payload))
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
		events = append(events, eventForNode("decision_awaited", obligation.NodeID, executorActor, struct {
			NodeID string `json:"nodeId"`
		}{NodeID: obligation.NodeID}))
	}
	if command != nil {
		events = append(events, event("program_prepared", command, executorActor,
			preparedEvidence{Command: cloneCommand(*command)}))
	}
	if len(events) == 0 {
		events = append(events, event("engine_advanced", nil, executorActor, struct {
			Status engine.RunStatus `json:"status"`
		}{Status: advanced.Status}))
	}
	return events
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
