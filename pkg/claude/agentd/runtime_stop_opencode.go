package agentd

import (
	"context"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	platformruntime "github.com/tofutools/tclaude/pkg/claude/platform/runtime"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// openCodeStopRuntime controls the authoritative server. Its tmux pane is an
// optional attachment and is never used as workload-exit evidence.
type openCodeStopRuntime struct {
	key        platformruntime.AttemptKey
	row        db.SessionRow
	runtime    *db.OpenCodeRuntime
	attachment *lifecycleTarget
}

var stopExactOpenCodeRuntimeForStop = stopExactOpenCodeRuntime
var verifyOpenCodeRuntimeForStop = openCodeRuntimeVerified

func (r *openCodeStopRuntime) Key() platformruntime.AttemptKey { return r.key }

func (r *openCodeStopRuntime) RequestStop(ctx context.Context, _ platformruntime.StopRequest) platformruntime.ControlResult {
	return r.stop(ctx)
}

func (r *openCodeStopRuntime) ForceStop(ctx context.Context, _ platformruntime.ForceStopRequest) platformruntime.ControlResult {
	return r.stop(ctx)
}

func (r *openCodeStopRuntime) stop(ctx context.Context) platformruntime.ControlResult {
	if err := ctx.Err(); err != nil {
		return platformruntime.ControlResult{State: platformruntime.ControlRefused, Detail: err.Error()}
	}
	serverMatched := false
	if r.runtime != nil {
		matched, err := stopExactOpenCodeRuntimeForStop(*r.runtime, true)
		if err != nil {
			return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: err.Error()}
		}
		serverMatched = matched
	}
	attachmentControl := r.stopAttachment(ctx)
	if attachmentControl.State == platformruntime.ControlUnknown || attachmentControl.State == platformruntime.ControlRefused {
		return attachmentControl
	}
	if !serverMatched && attachmentControl.State != platformruntime.ControlDispatched {
		return platformruntime.ControlResult{State: platformruntime.ControlNoEffect, Detail: "selected OpenCode server already exited or was replaced"}
	}
	return platformruntime.ControlResult{State: platformruntime.ControlDispatched}
}

func (r *openCodeStopRuntime) stopAttachment(ctx context.Context) platformruntime.ControlResult {
	if r.attachment == nil {
		return platformruntime.ControlResult{State: platformruntime.ControlNoEffect}
	}
	if err := ctx.Err(); err != nil {
		return platformruntime.ControlResult{State: platformruntime.ControlRefused, Detail: err.Error()}
	}
	probe, err := r.attachment.revalidate()
	if err != nil {
		if observation := r.observeAttachment(); observation != platformruntime.AttachmentPresent {
			return platformruntime.ControlResult{State: platformruntime.ControlNoEffect}
		}
		return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: "OpenCode attachment identity is not provable: " + err.Error()}
	}
	if err := killLifecycleTarget(r.attachment); err == nil {
		return platformruntime.ControlResult{State: platformruntime.ControlDispatched}
	}
	pid := probe.panePID
	if pid <= 0 {
		pid = r.attachment.panePID
	}
	for _, step := range softExitSignalLadder {
		if waitForPaneProcessGone(r.attachment, pid, softExitEscalationSignalGrace) {
			return platformruntime.ControlResult{State: platformruntime.ControlDispatched}
		}
		if err := signalLifecycleProcessGroup(pid, step.signal); err == nil &&
			waitForPaneProcessGone(r.attachment, pid, softExitEscalationSignalGrace) {
			return platformruntime.ControlResult{State: platformruntime.ControlDispatched}
		}
	}
	return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: "OpenCode server stopped but its exact attachment remains live"}
}

func (r *openCodeStopRuntime) Observe(ctx context.Context) platformruntime.Observation {
	if err := ctx.Err(); err != nil {
		return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: platformruntime.AttachmentUnknown, Evidence: err.Error()}
	}
	attachment := r.observeAttachment()
	if r.runtime == nil {
		return platformruntime.Observation{Workload: platformruntime.WorkloadExited, Attachment: attachment, Evidence: "selected server authority absent"}
	}
	current, err := db.GetOpenCodeRuntime(r.runtime.SessionID)
	if err != nil {
		return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: attachment, Evidence: err.Error()}
	}
	if current == nil || !sameOpenCodeRuntimeAttempt(*current, *r.runtime) {
		return platformruntime.Observation{Workload: platformruntime.WorkloadExited, Attachment: attachment, Evidence: "selected server authority absent or replaced"}
	}
	if !session.IsProcessAlive(current.PID) {
		return platformruntime.Observation{Workload: platformruntime.WorkloadExited, Attachment: attachment, Evidence: "selected server process exited"}
	}
	if openCodeRuntimeVerified(*current) {
		return platformruntime.Observation{Workload: platformruntime.WorkloadRunning, Attachment: attachment, Evidence: "recorded server process and endpoint verified"}
	}
	return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: attachment, Evidence: fmt.Sprintf("OpenCode server pid %d ownership unverified", current.PID)}
}

func (r *openCodeStopRuntime) observeAttachment() platformruntime.AttachmentState {
	if r.attachment != nil {
		probe, err := probeLifecyclePane(r.attachment.tmuxSession)
		switch {
		case err == nil && probe.state == paneProbeLive && lifecycleProbeMatchesTarget(probe, r.attachment):
			return platformruntime.AttachmentPresent
		case err == nil && (probe.state == paneProbeDead || !lifecycleProbeMatchesTarget(probe, r.attachment)):
			return platformruntime.AttachmentAbsent
		default:
			if alive, known := lifecycleSessionAlive(r.attachment.tmuxSession); known && !alive {
				return platformruntime.AttachmentAbsent
			}
			return platformruntime.AttachmentUnknown
		}
	}
	if r.row.TmuxSession == "" || !session.IsTmuxSessionAlive(r.row.TmuxSession) {
		return platformruntime.AttachmentAbsent
	}
	identity, err := db.GetSessionExitLaunchIdentity(r.row.ID)
	if err == nil && identity.Generation == r.key.Execution.String() && identity.TmuxSession == r.row.TmuxSession {
		return platformruntime.AttachmentPresent
	}
	return platformruntime.AttachmentUnknown
}
