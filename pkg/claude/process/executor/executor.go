package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
type Run struct {
	id           string
	stateVersion int64
	checkpoint   engine.Checkpoint
	definition   *engine.Definition
	authorized   map[string]struct{}
	dispatch     *Dispatch
}

type Action struct {
	Kind     ActionKind
	Status   engine.RunStatus
	Command  *engine.Command
	Decision *engine.DecisionObligation
}

// Dispatch is an in-memory permission to execute a command whose complete
// request has already committed. It cannot be constructed outside this package.
type Dispatch struct {
	owner        *Run
	stateVersion int64
	command      engine.Command
	mu           sync.Mutex
	used         bool
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

func (r *Run) Action() Action {
	if r == nil {
		return Action{}
	}
	action := Action{Status: r.checkpoint.Status}
	if r.checkpoint.OutstandingCommand != nil {
		command := cloneCommand(*r.checkpoint.OutstandingCommand)
		action.Command = &command
		if r.dispatch != nil && !r.dispatch.wasUsed() {
			action.Kind = ActionDispatch
		} else {
			action.Kind = ActionNeedsReconcile
		}
		return action
	}
	if r.checkpoint.AwaitingDecision != nil {
		obligation := *r.checkpoint.AwaitingDecision
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

// DecisionVerdicts exposes the prepared verdict vocabulary of the run's
// awaited decision, or nil when no decision is awaited.
func (r *Run) DecisionVerdicts() []string {
	if r == nil || r.definition == nil || r.checkpoint.AwaitingDecision == nil {
		return nil
	}
	verdicts, _ := r.definition.DecisionVerdicts(r.checkpoint.AwaitingDecision.NodeID)
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
// bound command before returning a dispatch permission.
func Prepare(run *Run) (*Dispatch, error) {
	if run == nil || run.definition == nil {
		return nil, ErrInvalidRun
	}
	if run.checkpoint.OutstandingCommand != nil {
		return nil, ErrNeedsReconcile
	}
	if run.checkpoint.Status != engine.RunRunning || run.checkpoint.AwaitingDecision != nil {
		// An awaited decision is already quiescent: advancing again would only
		// persist a no-op transition, so resumes return without a new version.
		return nil, nil
	}
	next, err := engine.AdvanceUntilQuiescent(run.checkpoint, run.definition)
	if err != nil {
		return nil, err
	}
	if err := persist(run, next, advanceEvidence(next)); err != nil {
		return nil, err
	}
	if next.OutstandingCommand == nil {
		return nil, nil
	}
	dispatch := &Dispatch{
		owner: run, stateVersion: run.stateVersion,
		command: cloneCommand(*next.OutstandingCommand),
	}
	run.dispatch = dispatch
	return dispatch, nil
}

// Reissue durably records the operator's explicit retry decision before it
// returns a fresh dispatch permission. It does not execute the program.
func Reissue(run *Run, actor string) (*Dispatch, error) {
	if err := validateReconciliationActor(actor); err != nil {
		return nil, err
	}
	command, err := reconcileCommand(run)
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
	dispatch := &Dispatch{owner: run, stateVersion: run.stateVersion, command: command}
	run.dispatch = dispatch
	return dispatch, nil
}

// RecordOutcome durably applies an operator-supplied outcome to an ambiguous
// command. It is the only reconciliation path other than explicit reissue.
func RecordOutcome(run *Run, actor string, outcome RecordedOutcome) error {
	if err := validateReconciliationActor(actor); err != nil {
		return err
	}
	command, err := reconcileCommand(run)
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

func reconcileCommand(run *Run) (engine.Command, error) {
	if run == nil || run.definition == nil {
		return engine.Command{}, ErrInvalidRun
	}
	if run.checkpoint.OutstandingCommand == nil || run.dispatch != nil {
		return engine.Command{}, ErrNoReconciliation
	}
	return cloneCommand(*run.checkpoint.OutstandingCommand), nil
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
	run.dispatch = nil
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

// advanceEvidence is the one human-facing row for an engine-owned advance:
// the prepared command, the newly awaited decision, or the plain status move.
func advanceEvidence(advanced engine.Checkpoint) db.ProcessRunEvent {
	switch {
	case advanced.OutstandingCommand != nil:
		return event("program_prepared", advanced.OutstandingCommand, executorActor,
			preparedEvidence{Command: cloneCommand(*advanced.OutstandingCommand)})
	case advanced.AwaitingDecision != nil:
		return eventForNode("decision_awaited", advanced.AwaitingDecision.NodeID, executorActor, struct {
			NodeID string `json:"nodeId"`
		}{NodeID: advanced.AwaitingDecision.NodeID})
	default:
		return event("engine_advanced", nil, executorActor, struct {
			Status engine.RunStatus `json:"status"`
		}{Status: advanced.Status})
	}
}

type preparedEvidence struct {
	Command engine.Command `json:"command"`
}

func cloneCommand(command engine.Command) engine.Command {
	command.Program.Args = append([]string(nil), command.Program.Args...)
	return command
}

func (d *Dispatch) wasUsed() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.used
}
