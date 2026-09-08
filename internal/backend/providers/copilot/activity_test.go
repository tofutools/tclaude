//go:build linux || darwin

package copilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Uses the actual released terminal and provider-owned observation command.
func assertNativeTurnActivity(t *testing.T, runtime *Runtime, request ports.PreparationRequest) {
	t.Helper()
	hooks, err := os.ReadFile(filepath.Join(runtime.stateRoot, "hooks", "tclaude-observation.json"))
	require.NoError(t, err)
	require.Contains(t, string(hooks), "UserPromptSubmit")
	require.Contains(t, string(hooks), "Stop")
	if runtime.nativeID == "" {
		runtime.observations = &observationSink{}
		writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: "00000000-0000-4000-8000-000000000001", HookEventName: "SessionStart", Source: "startup"})
		_, err := runtime.Observe(context.Background())
		require.NoError(t, err)
		require.Equal(t, "00000000-0000-4000-8000-000000000001", runtime.nativeID)
	}
	observe := func(want ports.AgentActivityObservedState) time.Time {
		t.Helper()
		result, err := runtime.Observe(context.Background())
		require.NoError(t, err)
		require.Equal(t, want, result.AgentActivity)
		return result.AgentActivityObservedAt
	}
	assertRecovered := func(want ports.AgentActivityObservedState, at time.Time) {
		t.Helper()
		observed, err := runtime.Observe(context.Background())
		require.NoError(t, err)
		recovered, err := runtime.provider.Recover(context.Background(), ports.RecoveryRequest{ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: observed.Evidence, Attempt: request.Spec.Attempt})
		require.NoError(t, err)
		require.Equal(t, ports.RecoveryControlled, recovered.State)
		require.Equal(t, want, recovered.Observation.AgentActivity)
		require.Equal(t, at, recovered.Observation.AgentActivityObservedAt)
	}
	observe(ports.AgentActivityUnknown)
	writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: runtime.nativeID, HookEventName: "UserPromptSubmit"})
	activeAt := observe(ports.AgentActivityActive)
	require.False(t, activeAt.IsZero())
	assertRecovered(ports.AgentActivityActive, activeAt)
	require.Equal(t, activeAt, observe(ports.AgentActivityActive), "polling is not new native evidence")
	writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: "00000000-0000-4000-8000-000000000099", HookEventName: "Stop"})
	writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: runtime.nativeID, AgentID: "child", HookEventName: "Stop"})
	observe(ports.AgentActivityActive)
	writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: runtime.nativeID, HookEventName: "Stop"})
	idleAt := observe(ports.AgentActivityIdle)
	require.True(t, idleAt.After(activeAt))
	assertRecovered(ports.AgentActivityIdle, idleAt)
	_, err = runtime.Interact(context.Background(), ports.Interaction{Text: "another turn"})
	require.NoError(t, err)
	unknownAt := observe(ports.AgentActivityUnknown)
	assertRecovered(ports.AgentActivityUnknown, unknownAt)
	writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: runtime.nativeID, HookEventName: "UserPromptSubmit"})
	observe(ports.AgentActivityActive)
}
