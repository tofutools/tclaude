package processexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// ExclusiveV7Executor is the schema-7 execution integration. It is
// concrete-FS-only. Every adapter side effect occurs after the exact claim is
// durably appended and after the coherent view callback has released all store
// locks.
type ExclusiveV7Executor struct {
	Store    *store.FS
	Adapters map[model.PerformerKind]Adapter
	Now      func() time.Time
}

func NewExclusiveV7(st *store.FS, adapters map[model.PerformerKind]Adapter) *ExclusiveV7Executor {
	copied := make(map[model.PerformerKind]Adapter, len(adapters))
	for kind, adapter := range adapters {
		copied[kind] = adapter
	}
	return &ExclusiveV7Executor{Store: st, Adapters: copied, Now: time.Now}
}

type exclusiveV7Action struct {
	transition *pathv1.ExecutionTransition
	plan       *pathv1.ExclusiveAttemptPlan
	wait       *pathv1.ExclusiveWaitPlan
	checkpoint *pathv1.CheckpointV7
	recover    bool
	terminal   *pathv1.CheckpointBinding
}

// Drive advances one schema-7 path-v1 run until it is terminal, blocked on
// a deferred adapter, or fails closed on an ambiguous claimed side effect.
func (e *ExclusiveV7Executor) Drive(ctx context.Context, runID string) (*pathv1.CheckpointV7, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("path-v1 exclusive executor store is required")
	}
	for round := 0; round < maxDriveRounds; round++ {
		action, err := e.nextAction(ctx, runID)
		if err != nil {
			return nil, err
		}
		if action.terminal != nil {
			checkpoint, err := e.Store.ReconfirmPathV1Durability(ctx, runID, *action.terminal)
			if store.IsConflict(err) {
				continue
			}
			return checkpoint, err
		}
		if action.plan != nil {
			checkpoint, blocked, err := e.executeAttempt(ctx, runID, action)
			if store.IsConflict(err) {
				continue
			}
			if err != nil || blocked {
				return checkpoint, err
			}
			continue
		}
		if action.wait != nil && action.transition == nil {
			return action.checkpoint, nil
		}
		if action.transition == nil {
			return nil, fmt.Errorf("path-v1 exclusive executor produced no exact action")
		}
		if _, err := e.Store.AppendPathV1(ctx, runID, action.transition); err != nil {
			if store.IsConflict(err) {
				continue
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("path-v1 run %q exceeded %d exclusive executor rounds", runID, maxDriveRounds)
}

func (e *ExclusiveV7Executor) nextAction(ctx context.Context, runID string) (exclusiveV7Action, error) {
	var action exclusiveV7Action
	err := e.Store.WithPathV1ExecutionView(ctx, runID, func(view store.PathV1ExecutionView) error {
		if status := pathv1.CurrentRunStatus(view.Checkpoint); status == "completed" || status == "failed" || status == "canceled" {
			binding := view.Binding
			action.terminal = &binding
			return nil
		}
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return err
		}
		for _, command := range aggregate.Commands {
			if command.Identity.Kind == pathv1.CommandCompleteRun && command.State.Active() {
				action.transition, err = pathv1.ObserveExclusiveCompletion(ctx, view.Input)
				return err
			}
		}
		if wait, found, waitErr := pathv1.RecoverExclusiveWait(ctx, view.Input); waitErr != nil {
			return waitErr
		} else if found {
			action.wait = wait
			action.checkpoint = view.Checkpoint
			if wait.WaitKind() != "signal" && !e.now().Before(wait.DueAt()) {
				action.transition, err = pathv1.ObserveExclusiveWait(ctx, view.Input, wait, "engine:path-v1-timer", "")
			}
			return err
		}
		if recovered, found, recoverErr := pathv1.RecoverExclusiveAttempt(ctx, view.Input); recoverErr != nil {
			return recoverErr
		} else if found {
			action.plan, action.recover = recovered, true
			return nil
		}
		_, pending, err := pathv1.PendingExclusiveObservation(ctx, view.Input)
		if err != nil {
			return err
		}
		if pending {
			action.transition, err = pathv1.AdvanceExclusiveRoute(ctx, view.Input)
			return err
		}
		if _, completeErr := pathv1.AssessAggregateCompletion(aggregate.View()); completeErr == nil {
			action.transition, err = pathv1.ClaimExclusiveCompletion(ctx, view.Input)
			return err
		} else if !errors.Is(completeErr, pathv1.ErrAggregateUnsettled) {
			return completeErr
		}

		live := make([]pathv1.PathRecord, 0, 1)
		for _, candidate := range aggregate.Routing.Paths {
			if candidate.Kind == pathv1.PathActivationOutput && candidate.State == pathv1.PathLive {
				live = append(live, candidate)
			}
		}
		if len(live) != 1 {
			return fmt.Errorf("path-v1 exclusive executor found %d live activation outputs", len(live))
		}
		if start, startErr := pathv1.AdvanceExclusiveStart(ctx, view.Input, live[0].ID); startErr == nil {
			action.transition = start
			return nil
		} else if !errors.Is(startErr, pathv1.ErrExclusiveUnsupported) {
			return startErr
		}
		if wait, waitErr := pathv1.PlanExclusiveWait(ctx, view.Input, live[0].ID, e.now()); waitErr == nil {
			action.wait = wait
			action.transition, err = pathv1.ClaimExclusiveWait(ctx, view.Input, wait)
			return err
		} else if !errors.Is(waitErr, pathv1.ErrExclusiveUnsupported) {
			return waitErr
		}
		attempt := uint64(1)
		for _, command := range aggregate.Commands {
			if command.Identity.Kind != pathv1.CommandPerformAttempt || command.Identity.SourceActivationID != live[0].SourceActivation.ID {
				continue
			}
			if command.Identity.Attempt == math.MaxUint64 {
				return fmt.Errorf("path-v1 attempt counter exhausted")
			}
			if command.Identity.Attempt >= attempt {
				attempt = command.Identity.Attempt + 1
			}
		}
		planned, err := pathv1.PlanExclusiveAttempt(ctx, view.Input, live[0].ID, attempt, view.Run.Params)
		if err != nil {
			return err
		}
		if performer := planned.Performer(); performer != nil && performer.Kind == model.PerformerProgram {
			return fmt.Errorf("path-v1 program performers require immutable audited authority and are not supported by the schema-v7 release")
		}
		action.plan = planned
		action.transition, err = pathv1.ClaimExclusiveAttempt(ctx, view.Input, planned)
		return err
	})
	return action, err
}

func (e *ExclusiveV7Executor) executeAttempt(ctx context.Context, runID string, action exclusiveV7Action) (*pathv1.CheckpointV7, bool, error) {
	request, adapter, err := e.exclusiveRequest(action.plan)
	if err != nil {
		return nil, false, err
	}
	if err := adapter.Validate(request); err != nil {
		return nil, false, err
	}
	if !action.recover {
		appended, err := e.Store.AppendPathV1(ctx, runID, action.transition)
		if err != nil {
			return nil, false, err
		}
		if appended.Disposition != store.PathV1AppendApplied {
			action.recover = true
		}
	}

	if deferred, ok := adapter.(DeferredAdapter); ok {
		if !action.recover {
			if _, err := deferred.Dispatch(ctx, request); err != nil {
				return nil, false, fmt.Errorf("dispatch path-v1 command %q: %w", request.Command.ID, err)
			}
			return nil, true, nil
		}
		observation, status, err := deferred.ReconcileDeferred(ctx, request)
		if err != nil {
			return nil, false, fmt.Errorf("reconcile path-v1 command %q: %w", request.Command.ID, err)
		}
		switch status {
		case DeferredMissing:
			if _, err := deferred.Dispatch(ctx, request); err != nil {
				return nil, false, fmt.Errorf("redispatch path-v1 command %q: %w", request.Command.ID, err)
			}
			return nil, true, nil
		case DeferredInFlight:
			return nil, true, nil
		case DeferredObserved:
			return e.appendExclusiveObservation(ctx, runID, action.plan, observation, true)
		default:
			return nil, false, fmt.Errorf("claimed path-v1 command %q is not externally discoverable; refusing to perform it again", request.Command.ID)
		}
	}

	if action.recover {
		reconciler, ok := adapter.(ReconcileAdapter)
		if !ok {
			return nil, false, fmt.Errorf("claimed path-v1 command %q cannot be reconciled; refusing to perform it again", request.Command.ID)
		}
		observation, found, err := reconciler.Reconcile(ctx, request)
		if err != nil {
			return nil, false, fmt.Errorf("reconcile path-v1 command %q: %w", request.Command.ID, err)
		}
		if !found {
			return nil, false, fmt.Errorf("claimed path-v1 command %q is not externally discoverable; refusing to perform it again", request.Command.ID)
		}
		return e.appendExclusiveObservation(ctx, runID, action.plan, observation, true)
	}

	observation, err := adapter.Perform(ctx, request)
	if err != nil {
		return nil, false, fmt.Errorf("perform path-v1 command %q: %w", request.Command.ID, err)
	}
	return e.appendExclusiveObservation(ctx, runID, action.plan, observation, false)
}

func (e *ExclusiveV7Executor) exclusiveRequest(attempt *pathv1.ExclusiveAttemptPlan) (Request, Adapter, error) {
	command := attempt.Command()
	performer := attempt.Performer()
	if performer == nil || command.Identity.Attempt > math.MaxInt {
		return Request{}, nil, fmt.Errorf("path-v1 command %q has invalid performer request", command.ID)
	}
	if performer.Kind == model.PerformerProgram {
		return Request{}, nil, fmt.Errorf("path-v1 program performers require immutable audited authority and are not supported by the schema-v7 release")
	}
	adapter := e.Adapters[performer.Kind]
	if adapter == nil {
		return Request{}, nil, fmt.Errorf("no process performer adapter registered for kind %q", performer.Kind)
	}
	params := attempt.Params()
	request := Request{
		Command: plan.Command{
			ID: exclusiveExternalCommandID(command.ID), IdempotencyKey: command.IdempotencyKey, Kind: plan.CommandKindStartAttempt,
			RunID: command.Identity.RunID, NodeID: attempt.NodeID(), Attempt: int(command.Identity.Attempt),
			Performer: performer, Params: params, ParamsBound: true,
		},
		Performer: *performer,
		Input:     Input{RunID: command.Identity.RunID, NodeID: attempt.NodeID(), Attempt: int(command.Identity.Attempt), Params: params},
	}
	return request, adapter, nil
}

func exclusiveExternalCommandID(pathCommandID string) string {
	const digestChars = 24
	if len(pathCommandID) < digestChars {
		return ""
	}
	return "cmd_" + pathCommandID[:digestChars]
}

func (e *ExclusiveV7Executor) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// RecordObservation is the live report boundary for a claimed schema-7 task
// or decision. The API-visible command id is a deterministic narrow alias of
// the full path-v1 command identity and is rederived before every append.
func (e *ExclusiveV7Executor) RecordObservation(ctx context.Context, runID, nodeID, commandID string, observation Observation) (*pathv1.CheckpointV7, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("path-v1 exclusive executor store is required")
	}
	checkpoint, _, err := e.recordObservation(ctx, runID, nodeID, commandID, observation)
	return checkpoint, err
}

// RecordNodeObservation is the human-resolution boundary where the caller has
// a run/node obligation rather than the API command alias. The recovered
// durable claim remains the sole authority for the observation.
func (e *ExclusiveV7Executor) RecordNodeObservation(ctx context.Context, runID, nodeID string, observation Observation) (*pathv1.CheckpointV7, string, error) {
	if e == nil || e.Store == nil {
		return nil, "", fmt.Errorf("path-v1 exclusive executor store is required")
	}
	return e.recordObservation(ctx, runID, nodeID, "", observation)
}

func (e *ExclusiveV7Executor) recordObservation(ctx context.Context, runID, nodeID, commandID string, observation Observation) (*pathv1.CheckpointV7, string, error) {
	if err := validateObservation(observation); err != nil {
		return nil, "", err
	}
	if observation.ExternalRef == "" {
		observation.ExternalRef = observation.EvidenceRef
	}
	for retry := 0; retry < maxObservationCASAttempts; retry++ {
		var attempt *pathv1.ExclusiveAttemptPlan
		var checkpoint *pathv1.CheckpointV7
		var replayCommand string
		err := e.Store.WithPathV1ExecutionView(ctx, runID, func(view store.PathV1ExecutionView) error {
			checkpoint = view.Checkpoint
			current, found, recoverErr := pathv1.RecoverExclusiveAttempt(ctx, view.Input)
			if recoverErr != nil {
				return recoverErr
			}
			if found {
				alias := exclusiveExternalCommandID(current.Command().ID)
				if current.NodeID() != nodeID || (commandID != "" && alias != commandID) {
					return fmt.Errorf("path-v1 report command does not belong to the requested run/node")
				}
				attempt = current
				replayCommand = alias
				return nil
			}
			commandPrefix := strings.TrimPrefix(commandID, "cmd_")
			recorded, exact, exactErr := pathv1.ExactExclusiveAttemptObserved(ctx, view.Input, nodeID, commandPrefix, pathv1.ExclusiveObservation{
				Outcome: strings.TrimSpace(observation.Verdict), Actor: string(observation.Actor), Feedback: observation.Feedback,
				EvidenceRef: observation.EvidenceRef, EvidenceHash: observation.EvidenceHash, ExternalRef: observation.ExternalRef,
			})
			if exactErr != nil {
				return exactErr
			}
			if !exact {
				return fmt.Errorf("node %q has no pending schema-7 obligation", nodeID)
			}
			replayCommand = exclusiveExternalCommandID(recorded.ID)
			if commandID != "" && replayCommand != commandID {
				return fmt.Errorf("path-v1 report command does not belong to the requested run/node")
			}
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		if attempt == nil {
			return checkpoint, replayCommand, nil
		}
		appended, _, err := e.appendExclusiveObservation(ctx, runID, attempt, observation, false)
		if store.IsConflict(err) {
			continue
		}
		return appended, replayCommand, err
	}
	return nil, "", fmt.Errorf("path-v1 observation remained contended")
}

// SatisfySignal observes the sole pending schema-7 signal wait when its exact
// immutable signal and live node match. Replays after satisfaction are
// idempotent once the same wait command is already observed.
func (e *ExclusiveV7Executor) SatisfySignal(ctx context.Context, runID, nodeID, signal string, actor state.ActorRef) (*pathv1.CheckpointV7, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("path-v1 exclusive executor store is required")
	}
	for attempt := 0; attempt < maxObservationCASAttempts; attempt++ {
		var transition *pathv1.ExecutionTransition
		var current *pathv1.CheckpointV7
		err := e.Store.WithPathV1ExecutionView(ctx, runID, func(view store.PathV1ExecutionView) error {
			current = view.Checkpoint
			wait, found, waitErr := pathv1.RecoverExclusiveWait(ctx, view.Input)
			if waitErr != nil {
				return waitErr
			}
			if !found {
				observed, observedErr := pathv1.ExactExclusiveSignalObserved(ctx, view.Input, nodeID, signal, string(actor))
				if observedErr != nil {
					return observedErr
				}
				if observed {
					return nil
				}
				return fmt.Errorf("path-v1 signal does not match the pending wait")
			}
			if wait.WaitKind() != "signal" || wait.NodeID() != nodeID || wait.Signal() != strings.TrimSpace(signal) {
				return fmt.Errorf("path-v1 signal does not match the pending wait")
			}
			transition, waitErr = pathv1.ObserveExclusiveWait(ctx, view.Input, wait, string(actor), "signal:"+strings.TrimSpace(signal))
			return waitErr
		})
		if err != nil {
			return nil, err
		}
		if transition == nil {
			return current, nil
		}
		appended, err := e.Store.AppendPathV1(ctx, runID, transition)
		if store.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return appended.Checkpoint, nil
	}
	return nil, fmt.Errorf("path-v1 signal remained contended")
}

func (e *ExclusiveV7Executor) appendExclusiveObservation(ctx context.Context, runID string, attempt *pathv1.ExclusiveAttemptPlan, observation Observation, recovered bool) (*pathv1.CheckpointV7, bool, error) {
	if err := validateObservation(observation); err != nil {
		return nil, false, fmt.Errorf("record path-v1 command %q observation: %w", attempt.Command().ID, err)
	}
	if observation.Evidence != nil {
		artifact, err := e.Store.PutArtifact(ctx, runID, observation.Evidence.Name, bytes.NewReader(observation.Evidence.Data))
		if err != nil {
			return nil, false, err
		}
		if observation.EvidenceHash != "" && observation.EvidenceHash != artifact.SHA256 {
			return nil, false, fmt.Errorf("path-v1 observation evidence hash %q does not match stored artifact %q", observation.EvidenceHash, artifact.SHA256)
		}
		observation.EvidenceRef, observation.EvidenceHash = artifact.Ref, artifact.SHA256
	}
	if observation.ExternalRef == "" {
		observation.ExternalRef = observation.EvidenceRef
	}
	var transition *pathv1.ExecutionTransition
	err := e.Store.WithPathV1ExecutionView(ctx, runID, func(view store.PathV1ExecutionView) error {
		current, found, err := pathv1.RecoverExclusiveAttempt(ctx, view.Input)
		if err != nil {
			return err
		}
		if !found || current.Command().ID != attempt.Command().ID {
			return fmt.Errorf("path-v1 durable claim %q is no longer current", attempt.Command().ID)
		}
		transition, err = pathv1.ObserveExclusiveAttempt(ctx, view.Input, current, pathv1.ExclusiveObservation{
			Outcome: strings.TrimSpace(observation.Verdict), Actor: string(observation.Actor), Feedback: observation.Feedback,
			EvidenceRef: observation.EvidenceRef, EvidenceHash: observation.EvidenceHash, ExternalRef: observation.ExternalRef,
		}, recovered)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	appended, err := e.Store.AppendPathV1(ctx, runID, transition)
	if err != nil {
		return nil, false, err
	}
	return appended.Checkpoint, false, nil
}
