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
	key     platformruntime.AttemptKey
	row     db.SessionRow
	runtime db.OpenCodeRuntime
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
	matched, err := stopExactOpenCodeRuntimeForStop(r.runtime, true)
	if err != nil {
		return platformruntime.ControlResult{State: platformruntime.ControlUnknown, Detail: err.Error()}
	}
	if !matched {
		return platformruntime.ControlResult{State: platformruntime.ControlNoEffect, Detail: "selected OpenCode server already exited or was replaced"}
	}
	return platformruntime.ControlResult{State: platformruntime.ControlDispatched}
}

func (r *openCodeStopRuntime) Observe(ctx context.Context) platformruntime.Observation {
	if err := ctx.Err(); err != nil {
		return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: platformruntime.AttachmentUnknown, Evidence: err.Error()}
	}
	attachment := platformruntime.AttachmentUnknown
	if r.row.TmuxSession == "" {
		attachment = platformruntime.AttachmentAbsent
	} else if session.IsTmuxSessionAlive(r.row.TmuxSession) {
		identity, err := db.GetSessionExitLaunchIdentity(r.row.ID)
		if err == nil && identity.Generation == r.key.Execution.String() && identity.TmuxSession == r.row.TmuxSession {
			attachment = platformruntime.AttachmentPresent
		}
	} else {
		attachment = platformruntime.AttachmentAbsent
	}
	current, err := db.GetOpenCodeRuntime(r.runtime.SessionID)
	if err != nil {
		return platformruntime.Observation{Workload: platformruntime.WorkloadUnknown, Attachment: attachment, Evidence: err.Error()}
	}
	if current == nil || !sameOpenCodeRuntimeAttempt(*current, r.runtime) {
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
