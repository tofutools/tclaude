package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

type testStopRuntime struct {
	key         AttemptKey
	observation Observation
}

func (r testStopRuntime) Key() AttemptKey { return r.key }
func (r testStopRuntime) RequestStop(context.Context, StopRequest) ControlResult {
	return ControlResult{State: ControlDispatched}
}
func (r testStopRuntime) ForceStop(context.Context, ForceStopRequest) ControlResult {
	return ControlResult{State: ControlDispatched}
}
func (r testStopRuntime) Observe(context.Context) Observation { return r.observation }

func TestStopRuntimeSeparatesDispatchFromWorkloadExit(t *testing.T) {
	runtime := testStopRuntime{
		key:         AttemptKey{Execution: execution.ID("11111111111111111111111111111111")},
		observation: Observation{Workload: WorkloadRunning, Attachment: AttachmentAbsent},
	}
	control := runtime.RequestStop(context.Background(), StopRequest{})
	assert.Equal(t, ControlDispatched, control.State)
	assert.Equal(t, WorkloadRunning, runtime.Observe(context.Background()).Workload,
		"native acknowledgement must not imply workload completion")
	assert.Empty(t, runtime.Key().Agent)
	assert.Empty(t, runtime.Key().Conversation,
		"valid execution control does not require admitted Agent or Conversation associations")
}
