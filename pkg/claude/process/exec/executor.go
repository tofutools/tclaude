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

const DefaultReconcileDelay = 30 * time.Second

const maxObservationCASAttempts = 8

type Executor struct {
	Store    store.Store
	Adapters map[model.PerformerKind]Adapter
	Now      func() time.Time
	// ReconcileDelay is the grace period before an issued performer command is
	// considered abandoned by its original worker. Zero uses the production
	// default; a negative value makes the command immediately reconcilable.
	ReconcileDelay time.Duration
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
	// Bind the run parameters into the durable issued payload. Recovery must
	// replay the exact performer request that was claimed, even if run.json is
	// edited after the claim or the store is restarted.
	command = materializePerformer(command, snapshot.Run.Params)

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
	if command.Kind == plan.CommandKindResolveBlock {
		resolved, resolveErr := e.applyResolveBlockCommand(ctx, command)
		if resolveErr != nil {
			return result, resolveErr
		}
		result.State = resolved
		return result, nil
	}

	if deferred, ok := adapter.(DeferredAdapter); ok {
		dispatched, dispatchErr := deferred.Dispatch(ctx, request)
		if dispatchErr != nil {
			return result, fmt.Errorf("dispatch process command %q: %w", command.ID, dispatchErr)
		}
		finished, dispatchErr := e.appendDispatch(ctx, command, dispatched)
		if dispatchErr != nil {
			return result, dispatchErr
		}
		result.State = finished
		return result, nil
	}

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

func (e *Executor) appendDispatch(ctx context.Context, command plan.Command, dispatched DispatchResult) (*state.State, error) {
	if command.Performer == nil {
		return nil, fmt.Errorf("process command %q has no deferred performer", command.ID)
	}
	externalRef := strings.TrimSpace(dispatched.ExternalRef)
	if externalRef == "" {
		return nil, fmt.Errorf("dispatch process command %q: external ref is required", command.ID)
	}
	cadence, budget, escalation, err := ContactScheduleFor(*command.Performer)
	if err != nil {
		return nil, fmt.Errorf("dispatch process command %q: %w", command.ID, err)
	}
	actions, err := e.obligationActions(ctx, command, dispatched.AvailableActions)
	if err != nil {
		return nil, err
	}
	at := e.now()
	waitKind := state.WaitKindAgent
	if command.Performer.Kind == model.PerformerHuman {
		waitKind = state.WaitKindHuman
	}
	contact := state.ContactState{
		CommandID:        command.ID,
		Kind:             waitKind,
		Assignee:         strings.TrimSpace(dispatched.Assignee),
		Cadence:          cadence.String(),
		Budget:           budget,
		EscalationTarget: escalation,
		NextContactAt:    at.Add(cadence),
	}
	entries := []evidence.LogEntry{commandEntry(command, state.Event{
		Type:        state.EventCommandDispatched,
		CommandID:   command.ID,
		ExternalRef: externalRef,
	}, "", at)}
	if dispatched.CreateObligation {
		dueAt := dispatched.DueAt
		if dueAt.IsZero() {
			dueAt = contact.NextContactAt
		}
		obligation := state.ObligationRecord{
			ID:               "obl_" + strings.TrimPrefix(command.ID, "cmd_"),
			RunID:            command.RunID,
			NodeID:           command.NodeID,
			Attempt:          command.Attempt,
			CommandID:        command.ID,
			Kind:             waitKind,
			Assignee:         strings.TrimSpace(dispatched.Assignee),
			Status:           state.WaitStatusPending,
			DueAt:            dueAt,
			Summary:          strings.TrimSpace(dispatched.Summary),
			AvailableActions: actions,
			NodeLink:         command.RunID + "/" + command.NodeID,
			CreatedAt:        at,
		}
		if obligation.Assignee == "" {
			obligation.Assignee = externalRef
		}
		if obligation.Summary == "" {
			obligation.Summary = "Complete process node " + command.NodeID
		}
		entries = append(entries, commandEntry(command, state.Event{Type: state.EventObligationCreated, Obligation: &obligation}, "", at))
	}
	entries = append(entries, commandEntry(command, state.Event{Type: state.EventContactScheduled, Contact: &contact}, "", at))

	for attempt := 0; attempt < maxObservationCASAttempts; attempt++ {
		snapshot, loadErr := e.Store.LoadRun(ctx, command.RunID)
		if loadErr != nil {
			return nil, loadErr
		}
		outstanding, ok := snapshot.State.OutstandingCommands[command.ID]
		if !ok {
			return nil, fmt.Errorf("process command %q disappeared before dispatch record", command.ID)
		}
		if outstanding.ExternalRef != "" {
			if outstanding.ExternalRef != externalRef {
				return nil, fmt.Errorf("process command %q dispatched as both %q and %q", command.ID, outstanding.ExternalRef, externalRef)
			}
			return snapshot.State, nil
		}
		appended, appendErr := e.Store.Append(ctx, command.RunID, snapshot.State.LastLogSeq, entries)
		if appendErr == nil {
			return appended.State, nil
		}
		if !store.IsConflict(appendErr) {
			return nil, fmt.Errorf("record process command %q dispatch: %w", command.ID, appendErr)
		}
	}
	return nil, fmt.Errorf("record process command %q dispatch: exceeded %d CAS attempts", command.ID, maxObservationCASAttempts)
}

// obligationActions makes decision obligations advertise the pinned
// template's real edge vocabulary. Task obligations keep the adapter's action
// vocabulary, whose approve/reject forms are normalized to pass/fail when the
// observation is recorded.
func (e *Executor) obligationActions(ctx context.Context, command plan.Command, fallback []string) ([]string, error) {
	if command.Kind != plan.CommandKindRecordDecision {
		return append([]string(nil), fallback...), nil
	}
	run, err := e.Store.GetRun(ctx, command.RunID)
	if err != nil {
		return nil, fmt.Errorf("load run %q for decision actions: %w", command.RunID, err)
	}
	tmpl, err := e.Store.GetTemplate(ctx, run.TemplateRef)
	if err != nil {
		return nil, fmt.Errorf("load template %q for decision actions: %w", run.TemplateRef, err)
	}
	node, ok := tmpl.Nodes[command.NodeID]
	if !ok {
		return nil, fmt.Errorf("decision node %q is missing from template %q", command.NodeID, run.TemplateRef)
	}
	actions := make([]string, 0, len(node.Next))
	for action := range node.Next {
		actions = append(actions, action)
	}
	slices.Sort(actions)
	if len(actions) == 0 {
		return nil, fmt.Errorf("decision node %q has no available actions", command.NodeID)
	}
	return actions, nil
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
	observation, err = normalizeObligationObservation(snapshot, command, observation)
	if err != nil {
		return nil, err
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

func normalizeObligationObservation(snapshot store.Snapshot, command plan.Command, observation Observation) (Observation, error) {
	for _, id := range sortedObligationIDs(snapshot.State.Obligations) {
		obligation := snapshot.State.Obligations[id]
		if obligation.CommandID != command.ID || obligation.Status != state.WaitStatusPending {
			continue
		}
		raw := strings.ToLower(strings.TrimSpace(observation.Verdict))
		allowed := make([]string, 0, len(obligation.AvailableActions)+2)
		allowedSet := map[string]struct{}{}
		addAllowed := func(value string) {
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "" {
				return
			}
			if _, exists := allowedSet[value]; exists {
				return
			}
			allowedSet[value] = struct{}{}
			allowed = append(allowed, value)
		}
		for _, action := range obligation.AvailableActions {
			addAllowed(action)
		}
		if command.Kind != plan.CommandKindRecordDecision {
			addAllowed("pass")
			addAllowed("fail")
		}
		if _, ok := allowedSet[raw]; !ok {
			return Observation{}, fmt.Errorf("verdict %q is not allowed for obligation %q; allowed: %s", observation.Verdict, obligation.ID, strings.Join(allowed, ", "))
		}
		if command.Kind == plan.CommandKindRecordDecision {
			observation.Verdict = raw
			return observation, nil
		}
		switch raw {
		case "pass", "approve":
			observation.Verdict = "pass"
		case "fail", "reject", "ask-changes":
			observation.Verdict = "fail"
		default:
			return Observation{}, fmt.Errorf("action %q has no pass/fail semantics for obligation %q; allowed: %s", observation.Verdict, obligation.ID, strings.Join(allowed, ", "))
		}
		return observation, nil
	}
	return observation, nil
}

func sortedObligationIDs(obligations map[string]state.ObligationRecord) []string {
	ids := make([]string, 0, len(obligations))
	for id := range obligations {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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

// ReconcileOutstanding asks a discoverable performer adapter to locate an
// issued command's external side effect by its durable idempotency key. It
// never invokes Perform and therefore never duplicates an unknown side effect.
func (e *Executor) ReconcileOutstanding(ctx context.Context, runID, commandID string) (*state.State, bool, error) {
	st, status, err := e.ReconcileDeferredOutstanding(ctx, runID, commandID)
	return st, status == DeferredObserved, err
}

// ReconcileDeferredOutstanding preserves the three-way state required by
// asynchronous agents and humans. The legacy ReconcileOutstanding wrapper
// intentionally collapses in-flight and missing for older callers.
func (e *Executor) ReconcileDeferredOutstanding(ctx context.Context, runID, commandID string) (*state.State, DeferredStatus, error) {
	command, err := e.outstandingCommand(ctx, runID, commandID)
	if err != nil {
		return nil, DeferredMissing, err
	}
	if command.Performer == nil || (command.Kind != plan.CommandKindStartAttempt && command.Kind != plan.CommandKindRecordDecision) {
		return nil, DeferredMissing, fmt.Errorf("process command %q is not a performer command", commandID)
	}
	snapshot, err := e.Store.LoadRun(ctx, runID)
	if err != nil {
		return nil, DeferredMissing, err
	}
	if outstanding := snapshot.State.OutstandingCommands[commandID]; outstanding.Status == state.CommandStatusObserved {
		return snapshot.State, DeferredObserved, nil
	}
	adapter := e.Adapters[command.Performer.Kind]
	if adapter == nil {
		return snapshot.State, DeferredMissing, nil
	}
	request := performerRequest(snapshot.Run, command)
	if err := adapter.Validate(request); err != nil {
		return nil, DeferredMissing, err
	}
	if deferred, ok := adapter.(DeferredAdapter); ok {
		observation, status, reconcileErr := deferred.ReconcileDeferred(ctx, request)
		if reconcileErr != nil {
			return snapshot.State, status, reconcileErr
		}
		if status == DeferredInFlight && snapshot.State.OutstandingCommands[commandID].ExternalRef == "" {
			dispatched, dispatchErr := deferred.Dispatch(ctx, request)
			if dispatchErr != nil {
				return snapshot.State, status, dispatchErr
			}
			attached, dispatchErr := e.appendDispatch(ctx, command, dispatched)
			return attached, status, dispatchErr
		}
		if status != DeferredObserved {
			return snapshot.State, status, nil
		}
		finished, recordErr := e.RecordObservation(ctx, command, observation)
		return finished, DeferredObserved, recordErr
	}
	reconciler, ok := adapter.(ReconcileAdapter)
	if !ok {
		return snapshot.State, DeferredMissing, nil
	}
	observation, found, err := reconciler.Reconcile(ctx, request)
	if err != nil || !found {
		return snapshot.State, DeferredMissing, err
	}
	finished, err := e.RecordObservation(ctx, command, observation)
	return finished, DeferredObserved, err
}

func (e *Executor) DeferredRequest(ctx context.Context, runID, commandID string) (Request, Adapter, error) {
	command, err := e.outstandingCommand(ctx, runID, commandID)
	if err != nil {
		return Request{}, nil, err
	}
	snapshot, err := e.Store.LoadRun(ctx, runID)
	if err != nil {
		return Request{}, nil, err
	}
	if command.Performer == nil {
		return Request{}, nil, fmt.Errorf("process command %q has no performer", commandID)
	}
	adapter := e.Adapters[command.Performer.Kind]
	if adapter == nil {
		return Request{}, nil, fmt.Errorf("no process performer adapter registered for kind %q", command.Performer.Kind)
	}
	return performerRequest(snapshot.Run, command), adapter, nil
}

// RetryOutstanding re-invokes an issued performer command only when the host
// has positive knowledge that the previous attempt stopped before its side
// effect (currently the RateLimitError contract). Generic crash recovery must
// use ReconcileOutstanding instead.
func (e *Executor) RetryOutstanding(ctx context.Context, runID, commandID string) (*state.State, error) {
	command, err := e.outstandingCommand(ctx, runID, commandID)
	if err != nil {
		return nil, err
	}
	if command.Performer == nil || (command.Kind != plan.CommandKindStartAttempt && command.Kind != plan.CommandKindRecordDecision) {
		return nil, fmt.Errorf("process command %q is not a performer command", commandID)
	}
	snapshot, err := e.Store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !plan.AllowsExecution(snapshot.State.Status) {
		return nil, fmt.Errorf("process run %q is %s and command %q cannot be retried", runID, snapshot.State.Status, commandID)
	}
	outstanding := snapshot.State.OutstandingCommands[commandID]
	if outstanding.Status != state.CommandStatusIssued {
		return nil, fmt.Errorf("process command %q is %s and cannot be retried", commandID, outstanding.Status)
	}
	if command.Performer.Kind == model.PerformerProgram && (!snapshot.Run.AllowPrograms || !programExecutionAudited(snapshot.State)) {
		return nil, fmt.Errorf("process run %q does not allow program performers; instantiate it with --allow-programs", runID)
	}
	adapter := e.Adapters[command.Performer.Kind]
	if adapter == nil {
		return nil, fmt.Errorf("no process performer adapter registered for kind %q", command.Performer.Kind)
	}
	request := performerRequest(snapshot.Run, command)
	if err := adapter.Validate(request); err != nil {
		return nil, err
	}
	observation, err := adapter.Perform(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("perform process command %q: %w", command.ID, err)
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
	if command.Kind == plan.CommandKindResolveBlock {
		return e.applyResolveBlockCommand(ctx, command)
	}
	if !plan.AllowsExecution(snapshot.State.Status) {
		return nil, fmt.Errorf("process run %q is %s and command %q cannot be resumed", command.RunID, snapshot.State.Status, command.ID)
	}
	return e.appendObservation(ctx, command, Observation{Verdict: "pass"})
}

// applyResolveBlockCommand is the crash-safe decision-node bridge into the
// shared poison-resolution funnel. The command is claimed before this method
// runs. If the process stops after the audited resolution append but before
// command_observed, ResumeOutstanding repeats the idempotent resolution and
// marks the original command observed without issuing a second decision.
func (e *Executor) applyResolveBlockCommand(ctx context.Context, command plan.Command) (*state.State, error) {
	snapshot, err := e.Store.LoadRun(ctx, command.RunID)
	if err != nil {
		return nil, err
	}
	request, err := BindBlockResolution(snapshot, BlockResolutionRequest{
		RunID: command.RunID, NodeID: command.TargetNodeID, BlockedAttempt: command.BlockedAttempt,
		Decision: command.BlockDecision, Actor: command.Actor, Reason: command.Reason, EvidenceRef: command.EvidenceRef,
	})
	if err != nil {
		return nil, err
	}
	return e.resolveBlocked(ctx, request, &command)
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
		// The store-computed content hash always wins over an adapter-claimed
		// one: silently keeping a supplied hash would let a constant claim on
		// changing evidence bytes drive the evidence-unchanged short-circuit
		// against work the gate never evaluated. A mismatching claim is an
		// invalid observation, not something to correct quietly.
		if observation.EvidenceHash != "" && observation.EvidenceHash != artifact.SHA256 {
			return Observation{}, fmt.Errorf("record process command %q observation: supplied evidence hash %q does not match stored artifact sha256 %q", command.ID, observation.EvidenceHash, artifact.SHA256)
		}
		observation.EvidenceHash = artifact.SHA256
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
	if command.Performer != nil {
		delay := e.ReconcileDelay
		if delay == 0 {
			delay = DefaultReconcileDelay
		}
		outstanding.ReconcileAfter = at.Add(delay)
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
	if command.Kind == plan.CommandKindResolveBlock {
		if command.TargetNodeID == "" || command.BlockedAttempt <= 0 || !command.BlockDecision.IsValid() {
			return fmt.Errorf("process command %q has an invalid block resolution target", command.ID)
		}
		if !state.ValidateActorRef(command.Actor) || state.IsEngineActor(command.Actor) || strings.TrimSpace(command.Reason) == "" || strings.TrimSpace(command.EvidenceRef) == "" {
			return fmt.Errorf("process command %q has invalid block resolution provenance", command.ID)
		}
	}
	return nil
}

func observationEntries(command plan.Command, observation Observation, snapshot store.Snapshot, at time.Time) ([]evidence.LogEntry, error) {
	entries := []evidence.LogEntry{commandEntry(command, state.Event{
		Type:         state.EventCommandObserved,
		CommandID:    command.ID,
		Actor:        observation.Actor,
		Outcome:      observation.Verdict,
		EvidenceRef:  observation.EvidenceRef,
		EvidenceHash: observation.EvidenceHash,
		Feedback:     observation.Feedback,
		ExternalRef:  observation.ExternalRef,
	}, observation.EvidenceRef, at)}
	for id, obligation := range snapshot.State.Obligations {
		if obligation.CommandID == command.ID && obligation.Status == state.WaitStatusPending {
			entries = append(entries, commandEntry(command, state.Event{
				Type:         state.EventObligationResolved,
				ObligationID: id,
				EvidenceRef:  observation.EvidenceRef,
			}, observation.EvidenceRef, at))
		}
	}
	if contact, ok := snapshot.State.Contacts[command.ID]; ok {
		contact.Paused = true
		contact.PauseReason = "performer observed"
		contact.NextContactAt = time.Time{}
		entries = append(entries, commandEntry(command, state.Event{Type: state.EventContactUpdated, Contact: &contact}, "", at))
	}

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
			Type:             state.EventNodeAttemptSettled,
			Actor:            source.Actor,
			Outcome:          source.Verdict,
			NodeStatus:       status,
			EvidenceRef:      source.EvidenceRef,
			EvidenceHash:     source.EvidenceHash,
			Feedback:         source.Feedback,
			WorkEvidenceHash: command.WorkEvidenceHash,
		}, source.EvidenceRef, at))
	case plan.CommandKindShortCircuit:
		// A stale issued short-circuit resumed after the gate moved on must
		// not re-apply: replaying gate_short_circuited against a settled (or
		// loop-reset pending) gate fails the reducer forever and wedges
		// Drive. Applicability mirrors what the planner required when it
		// emitted the command — a READY re-entering gate holding a prior
		// verdict whose recorded work-evidence hash still matches — AND the
		// command must be bound to the exact loop generation it was issued
		// for: the shape checks alone would also pass in a LATER window that
		// reverted to the same evidence bytes, standing the wrong
		// generation's verdict. Anything else, including a gate missing from
		// state entirely, is idempotent success: mark observed, append
		// nothing.
		node, ok := snapshot.State.Nodes[command.NodeID]
		applicable := ok && node.Status == state.NodeStatusReady &&
			len(node.Decisions) > 0 &&
			len(node.Decisions) == command.DecisionCount &&
			node.LastEvidenceHash != "" && node.LastEvidenceHash == command.EvidenceHash
		if !applicable {
			return entries, nil
		}
		entries = append(entries, commandEntry(command, state.Event{
			Type:         state.EventGateShortCircuited,
			Actor:        state.ActorEvidenceUnchanged,
			EvidenceHash: command.EvidenceHash,
		}, "", at))
	case plan.CommandKindGateFeedback:
		// A stale issued feedback command resumed after the loop already
		// re-entered (gate no longer failed, or the target work stage is no
		// longer settled-completed) must not re-apply: it would re-ready the
		// target mid-attempt and reset gates against work it has not seen.
		// The command is additionally bound to the loop generation it was
		// issued for (gate attempt AND verdict count): without that, a loop
		// that manually cycled back to the same failed/completed shape would
		// accept the replay and route the OLD window's payload against the
		// NEW failure, losing the newer feedback. Idempotent success, same as
		// the expand/block guards; a node missing from state entirely is also
		// inapplicable.
		gate, gok := snapshot.State.Nodes[command.NodeID]
		target, tok := snapshot.State.Nodes[command.TargetNodeID]
		applicable := gok && tok &&
			gate.Status == state.NodeStatusFailed &&
			gate.Attempt == command.Attempt &&
			len(gate.Decisions) == command.DecisionCount &&
			target.Status == state.NodeStatusCompleted
		if !applicable {
			return entries, nil
		}
		// One append batch routes the gate payload to its work stage, resets
		// the re-entering gate span, and re-readies the work stage; splitting
		// these would leave checkpoints mid-loop that replanning rejects.
		entries = append(entries, nodeEntry(command.TargetNodeID, state.Event{
			Type:        state.EventFeedbackRecorded,
			FromNodeID:  command.NodeID,
			Feedback:    command.Feedback,
			EvidenceRef: command.EvidenceRef,
		}, "", at))
		entries = append(entries, nodeEntry(nodeParentID(snapshot, command.NodeID), state.Event{
			Type:          state.EventGateLoopReset,
			Gates:         command.Gates,
			ResetCounters: command.ResetCounters,
			Reason:        command.Reason,
		}, "", at))
		entries = append(entries, nodeEntry(command.TargetNodeID, state.Event{
			Type:       state.EventNodeStatusSet,
			NodeStatus: state.NodeStatusReady,
		}, "", at))
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
		if command.SourceNodeStatus == state.NodeStatusBlocked {
			source, sourceOK := snapshot.State.Nodes[command.NodeID]
			target, targetOK := snapshot.State.Nodes[command.TargetNodeID]
			if !sourceOK || !targetOK || source.Status != state.NodeStatusBlocked || target.Status != state.NodeStatusPending {
				// A claimed poison-escalation activation may resume after a
				// human used process unblock. Observe that stale command as a
				// no-op; never create an obsolete decision obligation.
				return entries, nil
			}
		}
		entries = append(entries, nodeEntry(command.TargetNodeID, state.Event{
			Type:       state.EventNodeStatusSet,
			NodeStatus: command.NodeStatus,
			Attempt:    command.Attempt,
		}, "", at))
	case plan.CommandKindExpandNode:
		// The children come from the command's durable payload, so a crashed
		// host resumes the exact expansion the planner derived; the reducer
		// validates their shape and verify re-derives them from the template.
		//
		// An issued expand may also be resumed AFTER the expansion already
		// landed: a manual advance can expand the node between this command's
		// claim and its observation (crash recovery, or a CAS retry race).
		// Replaying node_expanded would then fail the reducer forever and
		// wedge Drive, so a recorded expansion identical to the durable
		// payload is idempotent success: mark observed, append nothing else.
		// A differing recorded expansion stays a hard error.
		if node, ok := snapshot.State.Nodes[command.NodeID]; ok && len(node.Children) > 0 {
			if err := expansionMatchesCommand(snapshot.State, node, command); err != nil {
				return nil, err
			}
			return entries, nil
		}
		entries = append(entries, commandEntry(command, state.Event{
			Type:  state.EventNodeExpanded,
			Nodes: command.Children,
		}, "", at))
	case plan.CommandKindBlockNode:
		// Block the poisoned stage child and its parent mirror in one append
		// batch: the blocked-mirror invariant does not allow an intermediate
		// child-blocked/parent-running checkpoint. (CAS-level batching, not a
		// crash guarantee — a torn write surfaces as dirty and needs repair.)
		//
		// A stale issued block resumed after the child already blocked is
		// idempotent success and must not re-apply: replaying could overwrite
		// a newer reason, and once the poison-resolution flow lands it would
		// silently re-block a deliberately released node.
		if node, ok := snapshot.State.Nodes[command.NodeID]; ok {
			if node.Status == state.NodeStatusBlocked ||
				node.BlockResolution != nil && command.Attempt <= node.BlockResolution.BlockedAttempt {
				return entries, nil
			}
		}
		entries = append(entries, commandEntry(command, state.Event{
			Type:    state.EventNodeBlocked,
			Attempt: command.Attempt,
			Reason:  command.Reason,
			Owner:   command.Owner,
		}, "", at))
		if command.TargetNodeID != "" {
			entries = append(entries, nodeEntry(command.TargetNodeID, state.Event{
				Type:       state.EventNodeBlocked,
				Attempt:    command.Attempt,
				FromNodeID: command.NodeID,
				Reason:     command.Reason,
				Owner:      command.Owner,
			}, "", at))
		}
	case plan.CommandKindResolveBlock:
		// resolveBlocked handles this command because its observation must be
		// atomic with the audited release (especially when cancel turns the run
		// terminal). Reaching the generic observation path is a programming bug.
		return nil, fmt.Errorf("resolve-block command %q requires the resolution funnel", command.ID)
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

// nodeParentID resolves a stage child's recorded parent node id.
func nodeParentID(snapshot store.Snapshot, nodeID string) string {
	if node, ok := snapshot.State.Nodes[nodeID]; ok {
		return node.Parent
	}
	return ""
}

// expansionMatchesCommand reports whether a node's recorded expansion is
// exactly the one an expand command's durable payload derives: same child ids
// in the same order, each with the same parent linkage, stage, and step id.
func expansionMatchesCommand(st *state.State, node state.NodeState, command plan.Command) error {
	if len(node.Children) != len(command.Children) {
		return fmt.Errorf("expand command %q payload derives %d children but node %q records %d", command.ID, len(command.Children), command.NodeID, len(node.Children))
	}
	for i, init := range command.Children {
		childID := node.Children[i]
		if childID != init.ID {
			return fmt.Errorf("expand command %q child %q does not match recorded child %q", command.ID, init.ID, childID)
		}
		child, ok := st.Nodes[childID]
		if !ok || child.Parent != command.NodeID || child.Stage != init.Stage || child.StepID != init.StepID {
			return fmt.Errorf("expand command %q child %q does not match its recorded stage node", command.ID, childID)
		}
	}
	return nil
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
	performer := model.InterpolatePerformer(*command.Performer, params)
	return Request{
		Command:   command,
		Performer: performer,
		Input: Input{
			RunID:   run.ID,
			NodeID:  command.NodeID,
			Attempt: command.Attempt,
			Params:  params,
		},
	}
}

func materializePerformer(command plan.Command, params map[string]string) plan.Command {
	if command.Performer == nil {
		return command
	}
	performer := model.InterpolatePerformer(*command.Performer, params)
	command.Performer = &performer
	return command
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
	case plan.CommandKindExpandNode:
		kind = evidence.EntryKindExpansion
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
		Kind:        evidence.KindForEvent(event.Type),
		Event:       &event,
		EvidenceRef: evidenceRef,
	}
}

func runEntry(event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	return runEntryWithKind(evidence.KindForEvent(event.Type), event, evidenceRef, at)
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
