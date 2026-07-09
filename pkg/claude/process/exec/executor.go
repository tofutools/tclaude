package processexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

const maxDriveRounds = 1000

const maxObservationCASAttempts = 8

type Executor struct {
	Store    store.Store
	Adapters map[model.PerformerKind]Adapter
	Now      func() time.Time
}

type Result struct {
	Command     plan.Command
	Claimed     bool
	Observation *Observation
	State       *state.State
}

func New(st store.Store, adapters map[model.PerformerKind]Adapter) *Executor {
	copied := make(map[model.PerformerKind]Adapter, len(adapters))
	for kind, adapter := range adapters {
		copied[kind] = adapter
	}
	return &Executor{Store: st, Adapters: copied, Now: time.Now}
}

// Drive repeatedly plans and executes commands for one run. It is a library
// operation, not an engine host: scheduling and daemon tick wiring belong to
// the engine-host phase.
func (e *Executor) Drive(ctx context.Context, runID string) (store.Snapshot, error) {
	for round := 0; round < maxDriveRounds; round++ {
		snapshot, err := e.Store.LoadRun(ctx, runID)
		if err != nil {
			return store.Snapshot{}, err
		}
		tmpl, err := e.Store.GetTemplate(ctx, snapshot.Run.TemplateRef)
		if err != nil {
			return store.Snapshot{}, err
		}
		commands, err := plan.Plan(snapshot.State, tmpl)
		if err != nil {
			return store.Snapshot{}, err
		}
		if len(commands) == 0 {
			if !plan.AllowsExecution(snapshot.State.Status) {
				return snapshot, nil
			}
			internal := issuedInternalCommandIDs(snapshot.State)
			if len(internal) > 0 {
				for _, commandID := range internal {
					if _, err := e.ResumeOutstanding(ctx, runID, commandID); err != nil {
						return store.Snapshot{}, err
					}
				}
				continue
			}
			return snapshot, nil
		}
		progressed := false
		for _, command := range commands {
			result, err := e.Execute(ctx, command)
			if err != nil {
				return store.Snapshot{}, err
			}
			progressed = progressed || result.Claimed
		}
		if !progressed {
			return e.Store.LoadRun(ctx, runID)
		}
	}
	return store.Snapshot{}, fmt.Errorf("process run %q exceeded %d executor rounds", runID, maxDriveRounds)
}

func issuedInternalCommandIDs(st *state.State) []string {
	var ids []string
	for commandID, command := range st.OutstandingCommands {
		if command.Status != state.CommandStatusIssued {
			continue
		}
		if command.Kind == plan.CommandKindStartAttempt || command.Kind == plan.CommandKindRecordDecision {
			continue
		}
		ids = append(ids, commandID)
	}
	slices.Sort(ids)
	return ids
}

func (e *Executor) Execute(ctx context.Context, command plan.Command) (Result, error) {
	if e == nil || e.Store == nil {
		return Result{}, fmt.Errorf("process executor store is required")
	}
	snapshot, err := e.Store.LoadRun(ctx, command.RunID)
	if err != nil {
		return Result{}, err
	}
	if report := processverify.Snapshot(snapshot); report.HasErrors() {
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Severity == model.SeverityError {
				return Result{}, fmt.Errorf("process run %q failed verification (%s at %s): %s", command.RunID, diagnostic.Code, diagnostic.Path, diagnostic.Message)
			}
		}
	}
	if err := validateCommand(snapshot, command, e.now()); err != nil {
		return Result{}, err
	}
	if commandIsClaimed(snapshot.State, command) {
		return Result{Command: command, State: snapshot.State}, nil
	}
	if err := validateCurrentPlan(ctx, e.Store, snapshot, command); err != nil {
		return Result{}, err
	}

	var adapter Adapter
	var request Request
	if command.Performer != nil {
		if command.Performer.Kind == model.PerformerProgram && (!snapshot.Run.AllowPrograms || !programExecutionAudited(snapshot.State)) {
			return Result{}, fmt.Errorf("process run %q does not allow program performers; instantiate it with --allow-programs", command.RunID)
		}
		adapter = e.Adapters[command.Performer.Kind]
		if adapter == nil {
			return Result{}, fmt.Errorf("no process performer adapter registered for kind %q", command.Performer.Kind)
		}
		request = performerRequest(snapshot.Run, command)
		if err := adapter.Validate(request); err != nil {
			return Result{}, err
		}
	}

	claimed, claimState, err := e.claim(ctx, snapshot, command)
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{Command: command, State: claimState}, nil
	}
	result := Result{Command: command, Claimed: true, State: claimState}

	observation := Observation{Verdict: "pass"}
	if adapter != nil {
		observation, err = adapter.Perform(ctx, request)
		if err != nil {
			return result, fmt.Errorf("perform process command %q: %w", command.ID, err)
		}
		observation, err = e.persistPerformerObservation(ctx, command, observation)
		if err != nil {
			return result, err
		}
	}
	if observation.ExternalRef == "" {
		observation.ExternalRef = observation.EvidenceRef
	}
	finished, err := e.appendObservation(ctx, command, observation)
	if err != nil {
		return result, err
	}
	result.Observation = &observation
	result.State = finished
	return result, nil
}

// RecordObservation reconciles an already-issued performer command without
// performing it again. Engine hosts can use the command identity embedded in an
// external side effect to recover the actor, verdict, and evidence after a
// crash between command_issued and command_observed.
func (e *Executor) RecordObservation(ctx context.Context, command plan.Command, observation Observation) (*state.State, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("process executor store is required")
	}
	snapshot, err := e.Store.LoadRun(ctx, command.RunID)
	if err != nil {
		return nil, err
	}
	outstanding, ok := snapshot.State.OutstandingCommands[command.ID]
	if !ok {
		return nil, fmt.Errorf("process command %q is not issued and cannot be reconciled", command.ID)
	}
	if err := validateIssuedPayload(outstanding, command); err != nil {
		return nil, err
	}
	if command.Kind != plan.CommandKindStartAttempt && command.Kind != plan.CommandKindRecordDecision {
		return nil, fmt.Errorf("process command %q is not a performer command", command.ID)
	}
	if outstanding.Status == state.CommandStatusObserved {
		if observation.Actor != "" && observation.Actor != outstanding.Actor ||
			strings.TrimSpace(observation.Verdict) != "" && observation.Verdict != outstanding.Verdict ||
			strings.TrimSpace(observation.EvidenceRef) != "" && observation.EvidenceRef != outstanding.EvidenceRef {
			return nil, fmt.Errorf("process command %q was already observed with a different result", command.ID)
		}
		return snapshot.State, nil
	}
	if outstanding.Status != state.CommandStatusIssued {
		return nil, fmt.Errorf("process command %q is %s and cannot be reconciled", command.ID, outstanding.Status)
	}
	observation, err = e.persistPerformerObservation(ctx, command, observation)
	if err != nil {
		return nil, err
	}
	if observation.ExternalRef == "" {
		observation.ExternalRef = observation.EvidenceRef
	}
	return e.appendObservation(ctx, command, observation)
}

// RecordOutstandingObservation recovers a performer command directly from its
// durable issued payload, so a restarted host does not need an in-memory copy
// of the planner output.
func (e *Executor) RecordOutstandingObservation(ctx context.Context, runID, commandID string, observation Observation) (*state.State, error) {
	command, err := e.outstandingCommand(ctx, runID, commandID)
	if err != nil {
		return nil, err
	}
	return e.RecordObservation(ctx, command, observation)
}

// ResumeIssued completes an already-issued internal command. Internal commands
// have no external side effect, so recovery only appends their deterministic
// observation and reducer transition; it never invokes a performer adapter.
func (e *Executor) ResumeIssued(ctx context.Context, command plan.Command) (*state.State, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("process executor store is required")
	}
	snapshot, err := e.Store.LoadRun(ctx, command.RunID)
	if err != nil {
		return nil, err
	}
	outstanding, ok := snapshot.State.OutstandingCommands[command.ID]
	if !ok {
		return nil, fmt.Errorf("process command %q is not issued and cannot be resumed", command.ID)
	}
	if err := validateIssuedPayload(outstanding, command); err != nil {
		return nil, err
	}
	if command.Kind == plan.CommandKindStartAttempt || command.Kind == plan.CommandKindRecordDecision {
		return nil, fmt.Errorf("process command %q is a performer command and requires RecordObservation", command.ID)
	}
	if outstanding.Status == state.CommandStatusObserved {
		return snapshot.State, nil
	}
	if outstanding.Status != state.CommandStatusIssued {
		return nil, fmt.Errorf("process command %q is %s and cannot be resumed", command.ID, outstanding.Status)
	}
	if !plan.AllowsExecution(snapshot.State.Status) {
		return nil, fmt.Errorf("process run %q is %s and command %q cannot be resumed", command.RunID, snapshot.State.Status, command.ID)
	}
	return e.appendObservation(ctx, command, Observation{Verdict: "pass"})
}

// ResumeOutstanding recovers an internal command directly from its durable
// issued payload. The command's full payload hash is checked before its
// deterministic reducer transition is appended.
func (e *Executor) ResumeOutstanding(ctx context.Context, runID, commandID string) (*state.State, error) {
	command, err := e.outstandingCommand(ctx, runID, commandID)
	if err != nil {
		return nil, err
	}
	return e.ResumeIssued(ctx, command)
}

func (e *Executor) outstandingCommand(ctx context.Context, runID, commandID string) (plan.Command, error) {
	if e == nil || e.Store == nil {
		return plan.Command{}, fmt.Errorf("process executor store is required")
	}
	snapshot, err := e.Store.LoadRun(ctx, runID)
	if err != nil {
		return plan.Command{}, err
	}
	outstanding, ok := snapshot.State.OutstandingCommands[commandID]
	if !ok {
		return plan.Command{}, fmt.Errorf("process command %q is not outstanding", commandID)
	}
	if len(outstanding.Payload) == 0 {
		return plan.Command{}, fmt.Errorf("process command %q has no durable command payload", commandID)
	}
	var command plan.Command
	if err := json.Unmarshal(outstanding.Payload, &command); err != nil {
		return plan.Command{}, fmt.Errorf("decode process command %q payload: %w", commandID, err)
	}
	if command.ID != commandID || command.RunID != runID {
		return plan.Command{}, fmt.Errorf("process command %q durable payload identity does not match its record", commandID)
	}
	if err := validateIssuedPayload(outstanding, command); err != nil {
		return plan.Command{}, err
	}
	return command, nil
}

func validateIssuedPayload(outstanding state.OutstandingCommand, command plan.Command) error {
	payloadHash := command.PayloadHash()
	if payloadHash == "" || outstanding.PayloadHash == "" || outstanding.PayloadHash != payloadHash ||
		outstanding.Kind != command.Kind || outstanding.NodeID != command.NodeID || outstanding.Attempt != command.Attempt ||
		(outstanding.IdempotencyKey != "" && outstanding.IdempotencyKey != command.IdempotencyKey) {
		return fmt.Errorf("process command %q does not match its issued command record", command.ID)
	}
	return nil
}

func (e *Executor) persistPerformerObservation(ctx context.Context, command plan.Command, observation Observation) (Observation, error) {
	if err := validateObservation(observation); err != nil {
		return Observation{}, fmt.Errorf("record process command %q observation: %w", command.ID, err)
	}
	if observation.Evidence != nil {
		artifact, err := e.Store.PutArtifact(ctx, command.RunID, observation.Evidence.Name, bytes.NewReader(observation.Evidence.Data))
		if err != nil {
			return Observation{}, fmt.Errorf("store evidence for process command %q: %w", command.ID, err)
		}
		observation.EvidenceRef = artifact.Ref
	}
	return observation, nil
}

func (e *Executor) claim(ctx context.Context, snapshot store.Snapshot, command plan.Command) (bool, *state.State, error) {
	if commandIsClaimed(snapshot.State, command) {
		return false, snapshot.State, nil
	}
	at := e.now()
	outstanding, err := command.OutstandingCommand(at)
	if err != nil {
		return false, nil, err
	}
	entries := []evidence.LogEntry{commandEntry(command, state.Event{
		Type:    state.EventCommandIssued,
		Command: &outstanding,
	}, "", at)}
	if command.Kind == plan.CommandKindStartAttempt {
		entries = append(entries, commandEntry(command, state.Event{
			Type:      state.EventNodeAttemptStarted,
			Attempt:   command.Attempt,
			CommandID: command.ID,
		}, "", at))
	}
	appended, err := e.Store.Append(ctx, command.RunID, snapshot.State.LastLogSeq, entries)
	if err != nil {
		if store.IsConflict(err) {
			latest, loadErr := e.Store.LoadRun(ctx, command.RunID)
			if loadErr == nil && commandIsClaimed(latest.State, command) {
				return false, latest.State, nil
			}
			return false, nil, fmt.Errorf("claim process command %q: %w", command.ID, err)
		}
		return false, nil, err
	}
	return true, appended.State, nil
}

func (e *Executor) appendObservation(ctx context.Context, command plan.Command, observation Observation) (*state.State, error) {
	for attempt := 0; attempt < maxObservationCASAttempts; attempt++ {
		snapshot, err := e.Store.LoadRun(ctx, command.RunID)
		if err != nil {
			return nil, err
		}
		outstanding, ok := snapshot.State.OutstandingCommands[command.ID]
		if !ok {
			return nil, fmt.Errorf("process command %q disappeared before observation", command.ID)
		}
		if outstanding.Status == state.CommandStatusObserved {
			return snapshot.State, nil
		}
		if outstanding.Status != state.CommandStatusIssued {
			return nil, fmt.Errorf("process command %q is %s and cannot be observed", command.ID, outstanding.Status)
		}
		entries, err := observationEntries(command, observation, snapshot, e.now())
		if err != nil {
			return nil, err
		}
		appended, err := e.Store.Append(ctx, command.RunID, snapshot.State.LastLogSeq, entries)
		if err == nil {
			return appended.State, nil
		}
		if !store.IsConflict(err) {
			return nil, fmt.Errorf("observe process command %q: %w", command.ID, err)
		}
	}
	return nil, fmt.Errorf("observe process command %q: exceeded %d CAS attempts", command.ID, maxObservationCASAttempts)
}

func validateCurrentPlan(ctx context.Context, st store.Store, snapshot store.Snapshot, command plan.Command) error {
	tmpl, err := st.GetTemplate(ctx, snapshot.Run.TemplateRef)
	if err != nil {
		return err
	}
	commands, err := plan.Plan(snapshot.State, tmpl)
	if err != nil {
		return err
	}
	for _, current := range commands {
		if reflect.DeepEqual(current, command) {
			return nil
		}
	}
	return fmt.Errorf("process command %q is not a current planner output for run %q", command.ID, command.RunID)
}

func commandIsClaimed(st *state.State, command plan.Command) bool {
	for _, outstanding := range st.OutstandingCommands {
		matchesKey := command.IdempotencyKey != "" && outstanding.IdempotencyKey == command.IdempotencyKey
		if outstanding.ID != command.ID && !matchesKey {
			continue
		}
		switch outstanding.Status {
		case state.CommandStatusIssued, state.CommandStatusObserved:
			return true
		}
	}
	return false
}

func programExecutionAudited(st *state.State) bool {
	if st == nil {
		return false
	}
	for _, record := range st.AdminRecords {
		if record.Type == state.EventAdminProgramsAllowed {
			return true
		}
	}
	return false
}

func validateCommand(snapshot store.Snapshot, command plan.Command, at time.Time) error {
	if strings.TrimSpace(command.ID) == "" {
		return fmt.Errorf("process command id is required")
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" {
		return fmt.Errorf("process command %q idempotency key is required", command.ID)
	}
	if !command.Kind.IsValid() {
		return fmt.Errorf("process command %q has invalid kind %q", command.ID, command.Kind)
	}
	if command.RunID != snapshot.Run.ID || command.RunID != snapshot.State.RunID {
		return fmt.Errorf("process command %q run id %q does not match loaded run %q", command.ID, command.RunID, snapshot.Run.ID)
	}
	if command.Performer != nil && command.Kind != plan.CommandKindStartAttempt && command.Kind != plan.CommandKindRecordDecision {
		return fmt.Errorf("process command %q kind %q cannot carry a performer", command.ID, command.Kind)
	}
	if command.Performer == nil && (command.Kind == plan.CommandKindStartAttempt || command.Kind == plan.CommandKindRecordDecision) {
		return fmt.Errorf("process command %q kind %q requires a performer", command.ID, command.Kind)
	}
	if command.Kind == plan.CommandKindSetTimer {
		if _, err := commandDueAt(command, at); err != nil {
			return err
		}
	}
	return nil
}

func observationEntries(command plan.Command, observation Observation, snapshot store.Snapshot, at time.Time) ([]evidence.LogEntry, error) {
	entries := []evidence.LogEntry{commandEntry(command, state.Event{
		Type:        state.EventCommandObserved,
		CommandID:   command.ID,
		Actor:       observation.Actor,
		Outcome:     observation.Verdict,
		EvidenceRef: observation.EvidenceRef,
		ExternalRef: observation.ExternalRef,
	}, observation.EvidenceRef, at)}

	switch command.Kind {
	case plan.CommandKindStartAttempt:
		return entries, nil
	case plan.CommandKindSettleAttempt:
		source, ok := snapshot.State.OutstandingCommands[command.SourceCommandID]
		if !ok || source.Status != state.CommandStatusObserved {
			return nil, fmt.Errorf("settle command %q source %q is not observed", command.ID, command.SourceCommandID)
		}
		status := state.SettleNodeStatus(source.Verdict, command.Attempt, &model.RetryPolicy{MaxAttempts: command.MaxAttempts})
		entries = append(entries, commandEntry(command, state.Event{
			Type:        state.EventNodeAttemptSettled,
			Actor:       source.Actor,
			Outcome:     source.Verdict,
			NodeStatus:  status,
			EvidenceRef: source.EvidenceRef,
		}, source.EvidenceRef, at))
	case plan.CommandKindRecordDecision:
		entries = append(entries, commandEntry(command, state.Event{
			Type:        state.EventDecisionRecorded,
			Actor:       observation.Actor,
			Outcome:     observation.Verdict,
			ChosenEdge:  observation.Verdict,
			EvidenceRef: observation.EvidenceRef,
			Decision: &state.DecisionRecord{
				Actor:       observation.Actor,
				Verdict:     observation.Verdict,
				EvidenceRef: observation.EvidenceRef,
				Timestamp:   at,
			},
		}, observation.EvidenceRef, at))
	case plan.CommandKindActivateNode:
		entries = append(entries, nodeEntry(command.TargetNodeID, state.Event{
			Type:       state.EventNodeStatusSet,
			NodeStatus: command.NodeStatus,
		}, "", at))
	case plan.CommandKindCompleteRun:
		entries = append(entries, runEntry(state.Event{
			Type:      state.EventRunStatusSet,
			RunStatus: command.RunStatus,
		}, "", at))
	case plan.CommandKindSetTimer:
		dueAt, err := commandDueAt(command, at)
		if err != nil {
			return nil, err
		}
		entries = append(entries, commandEntry(command, state.Event{
			Type: state.EventTimerCreated,
			Timer: &state.TimerRecord{
				ID:        command.WaitID,
				NodeID:    command.NodeID,
				Status:    state.WaitStatusPending,
				CreatedAt: at,
				DueAt:     dueAt,
			},
		}, "", at))
	case plan.CommandKindWaitSignal:
		entries = append(entries, commandEntry(command, state.Event{
			Type: state.EventWaitCreated,
			Wait: &state.WaitRecord{
				ID:        command.WaitID,
				NodeID:    command.NodeID,
				Kind:      state.WaitKindSignal,
				Status:    state.WaitStatusPending,
				CommandID: command.ID,
				CreatedAt: at,
			},
		}, "", at))
	default:
		return nil, fmt.Errorf("unsupported process command kind %q", command.Kind)
	}
	return entries, nil
}

func commandDueAt(command plan.Command, at time.Time) (time.Time, error) {
	if strings.TrimSpace(command.Until) != "" {
		dueAt, err := time.Parse(time.RFC3339, strings.TrimSpace(command.Until))
		if err != nil {
			return time.Time{}, fmt.Errorf("timer command %q has invalid until %q: %w", command.ID, command.Until, err)
		}
		return dueAt, nil
	}
	if strings.TrimSpace(command.Duration) == "" {
		return time.Time{}, fmt.Errorf("timer command %q requires duration or until", command.ID)
	}
	duration, err := time.ParseDuration(strings.TrimSpace(command.Duration))
	if err != nil || duration <= 0 {
		return time.Time{}, fmt.Errorf("timer command %q has invalid duration %q", command.ID, command.Duration)
	}
	return at.Add(duration), nil
}

func performerRequest(run store.RunRecord, command plan.Command) Request {
	params := make(map[string]string, len(run.Params))
	for key, value := range run.Params {
		params[key] = value
	}
	return Request{
		Command:   command,
		Performer: *command.Performer,
		Input: Input{
			RunID:   run.ID,
			NodeID:  command.NodeID,
			Attempt: command.Attempt,
			Params:  params,
		},
	}
}

func commandEntry(command plan.Command, event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	if event.NodeID == "" {
		event.NodeID = command.NodeID
	}
	event.At = at
	kind := evidence.EntryKindGate
	switch command.Kind {
	case plan.CommandKindStartAttempt, plan.CommandKindSettleAttempt:
		kind = evidence.EntryKindAttempt
	case plan.CommandKindRecordDecision:
		kind = evidence.EntryKindDecision
	}
	if command.NodeID == "" {
		return runEntryWithKind(kind, event, evidenceRef, at)
	}
	return evidence.LogEntry{
		At:          at,
		Scope:       evidence.Scope{Kind: evidence.ScopeNode, ID: command.NodeID},
		Kind:        kind,
		Event:       &event,
		EvidenceRef: evidenceRef,
	}
}

func nodeEntry(nodeID string, event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	event.NodeID = nodeID
	event.At = at
	return evidence.LogEntry{
		At:          at,
		Scope:       evidence.Scope{Kind: evidence.ScopeNode, ID: nodeID},
		Kind:        evidence.EntryKindGate,
		Event:       &event,
		EvidenceRef: evidenceRef,
	}
}

func runEntry(event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	return runEntryWithKind(evidence.EntryKindGate, event, evidenceRef, at)
}

func runEntryWithKind(kind evidence.EntryKind, event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	event.At = at
	return evidence.LogEntry{
		At:          at,
		Scope:       evidence.Scope{Kind: evidence.ScopeRun},
		Kind:        kind,
		Event:       &event,
		EvidenceRef: evidenceRef,
	}
}

func (e *Executor) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}
