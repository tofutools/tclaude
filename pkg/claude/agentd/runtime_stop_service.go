package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
	platformruntime "github.com/tofutools/tclaude/pkg/claude/platform/runtime"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func stopBoundRuntime(convID string, force bool, lifecycleAction, relatedEventID string, waitPolicy stopWaitPolicy, operation *stopOperationContext) (memberOpResult, softExitOutcome) {
	res := memberOpResult{ConvID: convID}
	bound, row, err := bindStopRuntime(convID)
	if err != nil {
		res.Action = "error"
		res.Detail = "bind selected execution: " + err.Error()
		operation.outcome.State = platformexec.StopUnresolved
		return res, softExitUnattempted
	}
	if bound == nil || row == nil {
		res.Action = "skipped:already_offline"
		operation.outcome.State = platformexec.StopNoExecution
		return res, softExitClosed
	}
	res.TmuxSes = row.TmuxSession
	key := bound.Key()
	operation.outcome.Attempt = platformexec.AttemptRef{ExecutionID: key.Execution, LegacySessionID: row.ID}
	operation.outcome.State = platformexec.StopFailed

	intent := setStopRuntimeIntent(row, key.Execution, lifecycleAction, relatedEventID)
	if lifecycleAction != "" && intent == nil {
		res.Action = "error"
		res.Detail = "selected launch intent became stale"
		return res, softExitUnattempted
	}

	operationCtx, cancel := context.WithCancel(context.Background())
	requestReason := lifecycleAction
	if requestReason == "" {
		requestReason = db.AgentExitActionStop
	}
	var control platformruntime.ControlResult
	if force {
		control = bound.ForceStop(operationCtx, platformruntime.ForceStopRequest{Reason: requestReason})
		operation.outcome.Effect = platformexec.StopEffectKill
	} else {
		control = bound.RequestStop(operationCtx, platformruntime.StopRequest{
			Reason: requestReason,
			Retry:  platformruntime.RetryBudget{Attempts: softExitMaxAttempts, Delay: softExitRetryDelay},
		})
		operation.outcome.Effect = platformexec.StopEffectSoftExit
	}

	switch control.State {
	case platformruntime.ControlDispatched:
		operation.outcome.State = platformexec.StopEffectDelivered
		if force {
			res.Action = "killed"
		} else {
			res.Action = "soft_stopped"
			if projection, ok := bound.(interface{ needsDaemonExitReason() bool }); ok && projection.needsDaemonExitReason() {
				if err := db.SetSessionExitReason(row.ID, daemonSoftExitReason); err != nil {
					slog.Warn("runtime stop: record native clean-exit reason failed", "session", row.ID, "error", err)
				}
			}
		}
	case platformruntime.ControlNoEffect:
		if boundRuntimeStopped(bound.Observe(operationCtx)) {
			operation.outcome.State = platformexec.StopCompleted
			res.Action = "skipped:already_offline"
			cancel()
			settleBoundRuntime(bound)
			return res, softExitClosed
		}
		fallthrough
	case platformruntime.ControlUnknown, platformruntime.ControlRefused, platformruntime.ControlUnsupported:
		res.Action = "error"
		res.Detail = control.Detail
		if res.Detail == "" {
			res.Detail = "runtime stop control was not dispatched"
		}
	}
	if force && control.State != platformruntime.ControlDispatched && control.State != platformruntime.ControlNoEffect {
		clearFailedExitIntent(intent)
		cancel()
		settleBoundRuntime(bound)
		return res, softExitUnattempted
	}
	if !force && control.State != platformruntime.ControlDispatched {
		// The graceful adapter established no effect. Release its attribution;
		// the application will arm the exact attempt again immediately before
		// any later force escalation.
		clearFailedExitIntent(intent)
		intent = nil
	}

	if !stopIntendsPaneClosure(lifecycleAction) {
		if control.State != platformruntime.ControlDispatched {
			clearFailedExitIntent(intent)
			cancel()
			return res, softExitUnattempted
		}
		if waitPolicy.wait {
			outcome := waitForBoundRuntime(bound, waitPolicy.deadline)
			cancel()
			settleBoundRuntime(bound)
			if outcome == softExitClosed {
				reconcileBoundRuntimeStop(bound, row, lifecycleAction, relatedEventID, daemonSoftExitReason, &res)
			}
			return res, outcome
		}
		// Keep the operation context alive for the adapter's bounded retries,
		// even though this unattributed legacy shape does not authorize force
		// escalation. Observation ends the operation early when the workload
		// exits; otherwise the ordinary graceful budget owns cancellation.
		operation.convergenceScheduled = true
		goBackground(func() {
			defer cancel()
			defer settleBoundRuntime(bound)
			_ = waitForBoundRuntime(bound, softExitEscalationDeadline)
		})
		return res, softExitClosed
	}

	if waitPolicy.wait {
		outcome := convergeBoundRuntime(operationCtx, bound, row, force, waitPolicy.deadline,
			lifecycleAction, relatedEventID, &res)
		clearFailedExitIntent(intent)
		cancel()
		settleBoundRuntime(bound)
		return res, outcome
	}
	operation.convergenceScheduled = true
	goBackground(func() {
		defer cancel()
		defer settleBoundRuntime(bound)
		defer clearFailedExitIntent(intent)
		_ = convergeBoundRuntime(operationCtx, bound, row, force, softExitEscalationDeadline,
			lifecycleAction, relatedEventID, &memberOpResult{ConvID: convID, TmuxSes: row.TmuxSession})
	})
	return res, softExitClosed
}

func setStopRuntimeIntent(row *db.SessionRow, executionID platformexec.ID, action, relatedEventID string) *db.SessionExitIntentRef {
	if row == nil || action == "" {
		return nil
	}
	ref, err := db.SetSessionExitIntentIfTarget(row.ID, row.TmuxSession, executionID.String(), action, relatedEventID, time.Now())
	if err != nil {
		slog.Warn("exit audit: bound runtime intent CAS failed", "session", row.ID,
			"execution", executionID, "error", err)
		return nil
	}
	return &ref
}

func waitForBoundRuntime(bound platformruntime.StopRuntime, window time.Duration) softExitOutcome {
	deadline := time.Now().Add(window)
	for {
		if softExitEscalationPollForTest != nil {
			softExitEscalationPollForTest()
		}
		observation := bound.Observe(context.Background())
		switch observation.Workload {
		case platformruntime.WorkloadExited:
			if !boundRuntimeStopped(observation) {
				break
			}
			return softExitClosed
		case platformruntime.WorkloadUnknown:
			// Unknown remains unresolved until the application-owned budget ends.
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return softExitStuck
		}
		delay := softExitEscalationPollInterval
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
	}
}

func boundRuntimeStopped(observation platformruntime.Observation) bool {
	return observation.Workload == platformruntime.WorkloadExited &&
		observation.Attachment == platformruntime.AttachmentAbsent
}

func convergeBoundRuntime(ctx context.Context, bound platformruntime.StopRuntime, row *db.SessionRow, force bool, grace time.Duration, lifecycleAction, relatedEventID string, res *memberOpResult) softExitOutcome {
	if !force && waitForBoundRuntime(bound, grace) == softExitClosed {
		reconcileBoundRuntimeStop(bound, row, lifecycleAction, relatedEventID, daemonSoftExitReason, res)
		return softExitClosed
	}
	if !force {
		if row != nil {
			_ = db.SetSessionExitReason(row.ID, daemonEscalatedKillReason)
		}
		forceIntent := setStopRuntimeIntent(row, bound.Key().Execution, lifecycleAction, relatedEventID)
		defer clearFailedExitIntent(forceIntent)
		control := bound.ForceStop(ctx, platformruntime.ForceStopRequest{Reason: lifecycleAction})
		if control.State != platformruntime.ControlDispatched && control.State != platformruntime.ControlNoEffect {
			res.Detail = joinDetail(res.Detail, "force-stop: "+control.Detail)
		}
	}
	forceWait := softExitEscalationSignalGrace
	if _, ok := bound.(*openCodeStopRuntime); ok {
		forceWait = openCodeEndpointCloseWait
	}
	if waitForBoundRuntime(bound, forceWait) != softExitClosed {
		res.Detail = joinDetail(res.Detail, "authoritative workload still running or unknown after force-stop")
		return softExitStuck
	}
	if !force {
		res.Detail = joinDetail(res.Detail, "workload did not exit; escalated to force-stop")
	}
	reconcileBoundRuntimeStop(bound, row, lifecycleAction, relatedEventID, daemonEscalatedKillReason, res)
	return softExitEscalated
}

func reconcileBoundRuntimeStop(bound platformruntime.StopRuntime, row *db.SessionRow, lifecycleAction, relatedEventID, reason string, res *memberOpResult) {
	var target *lifecycleTarget
	switch runtime := bound.(type) {
	case *terminalStopRuntime:
		target = runtime.target
	case *openCodeStopRuntime:
		target = runtime.attachment
		if target == nil && row != nil && (row.TmuxSession == "" || !session.IsTmuxSessionAlive(row.TmuxSession)) {
			if err := reconcileStoppedOpenCodeRow(runtime, lifecycleAction, relatedEventID, reason); err != nil {
				res.Action = "error"
				res.Detail = joinDetail(res.Detail, fmt.Sprintf("OpenCode server stopped but recording exited state failed: %v", err))
			}
			return
		}
	}
	if target == nil {
		return
	}
	if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, reason); err != nil {
		res.Action = "error"
		res.Detail = joinDetail(res.Detail, fmt.Sprintf("session stopped but recording exited state failed: %v", err))
	}
}

func reconcileStoppedOpenCodeRow(runtime *openCodeStopRuntime, lifecycleAction, relatedEventID, reason string) error {
	if runtime == nil {
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		row, err := db.LoadSession(runtime.row.ID)
		if err != nil {
			return err
		}
		if row == nil || row.Status == session.StatusExited {
			return nil
		}
		identity, err := db.GetSessionExitLaunchIdentity(row.ID)
		if err != nil {
			return err
		}
		if identity.Generation != runtime.key.Execution.String() {
			return nil
		}
		ok, _, err := db.MarkSessionExitedAndRecordObservationIfUnchanged(
			row.ID, row.Status, row.UpdatedAt, reason,
			db.AgentExitObservation{
				At: time.Now(), SessionID: row.ID, TmuxSession: row.TmuxSession,
				Observer: db.AgentExitObserverReconcile, CauseKind: db.AgentExitCauseDisappeared,
				LifecycleAction: lifecycleAction, RelatedEventID: relatedEventID,
				ExpectedGeneration: runtime.key.Execution.String(),
			},
		)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("session row kept changing after the OpenCode server exited")
}

func settleBoundRuntime(bound platformruntime.StopRuntime) {
	if terminal, ok := bound.(*terminalStopRuntime); ok {
		terminal.target.markSoftExitSettled()
	}
}
