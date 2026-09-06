// Package runtime defines small, portable capabilities for controlling one
// already-selected harness execution. It deliberately contains no daemon,
// database, terminal, HTTP, or vendor configuration types.
package runtime

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/platform/conversation"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

// AttemptKey identifies the execution a runtime handle is permanently bound
// to. Agent and Conversation are optional attribution captured from existing
// platform records; Execution is the required control identity.
type AttemptKey struct {
	Execution    execution.ID
	Agent        string
	Conversation conversation.ID
}

// RetryBudget is chosen by the lifecycle application and executed by the
// native adapter. Attempts includes the initial dispatch.
type RetryBudget struct {
	Attempts int
	Delay    time.Duration
}

type StopRequest struct {
	Reason string
	Retry  RetryBudget
}

type ForceStopRequest struct {
	Reason string
}

type ControlState string

const (
	ControlUnsupported ControlState = "unsupported"
	ControlRefused     ControlState = "refused"
	ControlDispatched  ControlState = "dispatched"
	ControlNoEffect    ControlState = "no_effect"
	ControlUnknown     ControlState = "unknown"
)

// ControlResult describes only the control effect. Dispatch is not workload
// completion; callers must establish that independently through Observe.
type ControlResult struct {
	State  ControlState
	Detail string
}

type WorkloadState string

const (
	WorkloadRunning WorkloadState = "running"
	WorkloadExited  WorkloadState = "exited"
	WorkloadUnknown WorkloadState = "unknown"
)

type AttachmentState string

const (
	AttachmentPresent AttachmentState = "present"
	AttachmentAbsent  AttachmentState = "absent"
	AttachmentUnknown AttachmentState = "unknown"
)

// Observation contains normalized facts about the selected execution.
// Evidence is an opaque diagnostic label produced by the trusted binder; it
// is not authority and is never interpreted as a platform identity.
type Observation struct {
	Workload   WorkloadState
	Attachment AttachmentState
	Evidence   string
}

type Stopper interface {
	RequestStop(context.Context, StopRequest) ControlResult
	ForceStop(context.Context, ForceStopRequest) ControlResult
}

type Observer interface {
	Observe(context.Context) Observation
}

// StopRuntime is one bound control-and-observation capability. Returning one
// object prevents control and observation from accidentally selecting
// different executions.
type StopRuntime interface {
	Stopper
	Observer
	Key() AttemptKey
}
