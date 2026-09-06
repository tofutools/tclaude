package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	platformruntime "github.com/tofutools/tclaude/pkg/claude/platform/runtime"
)

type terminalStopRecipe struct {
	name             string
	signalKeys       []string
	recordExitReason bool
}

type terminalStopRuntime struct {
	key    platformruntime.AttemptKey
	target *lifecycleTarget
	recipe terminalStopRecipe
}

func (r *terminalStopRuntime) Key() platformruntime.AttemptKey { return r.key }

func (r *terminalStopRuntime) RequestStop(ctx context.Context, request platformruntime.StopRequest) platformruntime.ControlResult {
	if r == nil || r.target == nil || r.key.Execution == "" {
		return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: "unbound terminal execution"}
	}
	if err := ctx.Err(); err != nil {
		return platformruntime.ControlResult{State: platformruntime.ControlRefused, Detail: err.Error()}
	}
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := r.target.revalidate(); err != nil {
		observation := r.Observe(ctx)
		if observation.Workload == platformruntime.WorkloadExited {
			return platformruntime.ControlResult{State: platformruntime.ControlNoEffect, Detail: "selected execution already exited"}
		}
		return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: "selected terminal identity is not provable: " + err.Error()}
	}
	logStopAdapterDispatch(r.target, r.recipe.name, 1, request.Retry.Attempts)
	if err := sendTerminalStopControl(r.target, r.recipe); err != nil {
		slog.Warn("runtime stop adapter initial dispatch failed", "harness", r.recipe.name,
			"execution", r.key.Execution, "error", err)
		return platformruntime.ControlResult{State: platformruntime.ControlRefused,
			Detail: "managed " + r.recipe.name + " signal exit dispatch failed"}
	}
	if afterSoftExitTargetSendForTest != nil {
		afterSoftExitTargetSendForTest()
	}
	if r.Observe(ctx).Workload == platformruntime.WorkloadRunning {
		r.scheduleRetries(ctx, request)
	}
	return platformruntime.ControlResult{State: platformruntime.ControlDispatched}
}

func (r *terminalStopRuntime) needsDaemonExitReason() bool { return r.recipe.recordExitReason }

func (r *terminalStopRuntime) scheduleRetries(ctx context.Context, request platformruntime.StopRequest) {
	attempts := request.Retry.Attempts
	if attempts <= 1 {
		return
	}
	delay := request.Retry.Delay
	if delay <= 0 {
		delay = softExitRetryDelay
	}
	goBackground(func() {
		for attempt := 2; attempt <= attempts; attempt++ {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-r.target.softExitSettled:
				timer.Stop()
				return
			case <-timer.C:
			}
			if beforeSoftExitTargetRetryProbeForTest != nil {
				beforeSoftExitTargetRetryProbeForTest(attempt)
			}
			observation := r.Observe(ctx)
			if observation.Workload != platformruntime.WorkloadRunning {
				return
			}
			logStopAdapterDispatch(r.target, r.recipe.name, attempt, attempts)
			if err := sendTerminalStopControl(r.target, r.recipe); err != nil {
				slog.Warn("runtime stop adapter retry failed", "harness", r.recipe.name,
					"execution", r.key.Execution, "attempt", attempt, "error", err)
				return
			}
		}
	})
}

func (r *terminalStopRuntime) ForceStop(ctx context.Context, request platformruntime.ForceStopRequest) platformruntime.ControlResult {
	if err := ctx.Err(); err != nil {
		return platformruntime.ControlResult{State: platformruntime.ControlRefused, Detail: err.Error()}
	}
	if beforeSoftExitEscalationRevalidateForTest != nil {
		beforeSoftExitEscalationRevalidateForTest()
	}
	probe, err := r.target.revalidate()
	if err != nil {
		if r.Observe(ctx).Workload == platformruntime.WorkloadExited {
			return platformruntime.ControlResult{State: platformruntime.ControlNoEffect, Detail: "selected execution already exited"}
		}
		return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: err.Error()}
	}
	dispatched := false
	if err := killLifecycleTarget(r.target); err == nil {
		dispatched = true
	} else {
		slog.Warn("runtime stop adapter tmux kill failed; continuing to process signals",
			"harness", r.recipe.name, "execution", r.key.Execution, "error", err)
	}
	pid := probe.panePID
	if pid <= 0 {
		pid = r.target.panePID
	}
	if pid > 0 {
		for _, step := range softExitSignalLadder {
			if waitForPaneProcessGone(r.target, pid, softExitEscalationSignalGrace) {
				break
			}
			if err := signalLifecycleProcessGroup(pid, step.signal); err != nil {
				slog.Warn("runtime stop adapter process signal failed", "harness", r.recipe.name,
					"execution", r.key.Execution, "signal", step.name, "error", err)
				continue
			}
			dispatched = true
		}
	}
	if !dispatched {
		return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: "no force-stop effect could be established"}
	}
	return platformruntime.ControlResult{State: platformruntime.ControlDispatched, Detail: request.Reason}
}

func (r *terminalStopRuntime) Observe(ctx context.Context) platformruntime.Observation {
	if err := ctx.Err(); err != nil {
		return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: platformruntime.AttachmentUnknown, Evidence: err.Error()}
	}
	probe, err := probeLifecyclePane(r.target.tmuxSession)
	switch {
	case err == nil && probe.state == paneProbeLive && lifecycleProbeMatchesTarget(probe, r.target):
		return platformruntime.Observation{Workload: platformruntime.WorkloadRunning, Attachment: platformruntime.AttachmentPresent, Evidence: "generation/pane/process match"}
	case err == nil && (probe.state == paneProbeDead || !lifecycleProbeMatchesTarget(probe, r.target)):
		return platformruntime.Observation{Workload: platformruntime.WorkloadExited, Attachment: platformruntime.AttachmentAbsent, Evidence: "selected pane ended or was replaced"}
	case err != nil || probe.state == paneProbeUnknown:
		if alive, known := lifecycleSessionAlive(r.target.tmuxSession); known && !alive {
			return platformruntime.Observation{Workload: platformruntime.WorkloadExited, Attachment: platformruntime.AttachmentAbsent, Evidence: "tmux session absent"}
		}
	}
	return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: platformruntime.AttachmentUnknown, Evidence: "terminal observation unavailable"}
}

func sendTerminalStopControl(target *lifecycleTarget, recipe terminalStopRecipe) error {
	if len(recipe.signalKeys) == 0 {
		return fmt.Errorf("%s runtime has no graceful stop control", recipe.name)
	}
	return injectSignalExitSerializedBy(target.tmuxSession+":0.0", target.paneID, recipe.signalKeys)
}

func logStopAdapterDispatch(target *lifecycleTarget, harnessName string, attempt, maxAttempts int) {
	slog.Info("runtime stop adapter dispatch", "harness", harnessName,
		"execution", target.attempt.ExecutionID, "session", target.sessionID,
		"pane_id", target.paneID, "attempt", attempt, "max_attempts", maxAttempts)
}
