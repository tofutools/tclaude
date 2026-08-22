package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeRuntimeLookupByConversation(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, UpsertOpenCodeRuntime(OpenCodeRuntime{
		SessionID: "spwn-test", ConvID: "ses_test",
		ServerURL: "http://127.0.0.1:43210", Password: "private",
		Cwd: "/tmp/project", PID: 42,
		SandboxImplementation: "tclaude-layer",
		SandboxLaunchSpecJSON: `{"version":1}`,
		ExecutionBoundaryJSON: `{"version":1,"path":{"host":"/original"}}`,
		PermissionJSON:        `[{"permission":"*","pattern":"*","action":"deny"}]`,
	}))

	runtime, err := GetOpenCodeRuntimeByConvID("ses_test")
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, "spwn-test", runtime.SessionID)
	assert.Equal(t, "private", runtime.Password)
	assert.Equal(t, "tclaude-layer", runtime.SandboxImplementation)
	assert.Equal(t, `{"version":1}`, runtime.SandboxLaunchSpecJSON)
	assert.JSONEq(t, `{"version":1,"path":{"host":"/original"}}`, runtime.ExecutionBoundaryJSON)
	assert.Equal(t, `[{"permission":"*","pattern":"*","action":"deny"}]`, runtime.PermissionJSON)

	missing, err := GetOpenCodeRuntimeByConvID("ses_missing")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestOpenCodeRuntimePersistsUnixReplayAuthority(t *testing.T) {
	setupTestDB(t)
	input := OpenCodeRuntime{
		SessionID: "spwn-unix", ServerURL: "http://127.0.0.1:43210",
		Password: "secret", Cwd: "/tmp/project", PID: 42,
		SandboxImplementation: "tclaude-layer",
		SandboxLaunchSpecJSON: `{"version":4}`,
		Transport:             OpenCodeTransportUnixRelay,
		ControlSocketPath:     "/tmp/agents/agt_abc/control.sock",
		ControlSocketDevice:   41, ControlSocketInode: 42,
		ResourceCgroupDir: "/sys/fs/cgroup/tclaude-spawn",
	}
	require.NoError(t, UpsertOpenCodeRuntime(input))
	got, err := GetOpenCodeRuntime(input.SessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, input.Transport, got.Transport)
	assert.Equal(t, input.ControlSocketPath, got.ControlSocketPath)
	assert.Equal(t, input.ControlSocketDevice, got.ControlSocketDevice)
	assert.Equal(t, input.ControlSocketInode, got.ControlSocketInode)
	assert.Equal(t, input.ResourceCgroupDir, got.ResourceCgroupDir)

	broken := input
	broken.SessionID = "spwn-broken"
	broken.ControlSocketInode = 0
	require.ErrorContains(t, UpsertOpenCodeRuntime(broken), "incomplete socket authority")
}

func TestOpenCodeRuntimeLookupByPID(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, UpsertOpenCodeRuntime(OpenCodeRuntime{
		SessionID: "spwn-old", ConvID: "ses_old",
		ServerURL: "http://127.0.0.1:43210", Password: "old",
		Cwd: "/tmp/old", PID: 4242,
	}))
	require.NoError(t, UpsertOpenCodeRuntime(OpenCodeRuntime{
		SessionID: "spwn-new", ConvID: "ses_new",
		ServerURL: "http://127.0.0.1:43211", Password: "new",
		Cwd: "/tmp/new", PID: 4242,
	}))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE opencode_runtimes
		SET updated_at = 1577836800000000000 WHERE session_id = 'spwn-old'`)
	require.NoError(t, err)

	runtime, err := FindOpenCodeRuntimeByPID(4242)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, "spwn-new", runtime.SessionID)
	assert.Equal(t, "ses_new", runtime.ConvID)

	missing, err := FindOpenCodeRuntimeByPID(9999)
	require.NoError(t, err)
	assert.Nil(t, missing)

	require.NoError(t, UpsertOpenCodeRuntime(OpenCodeRuntime{
		SessionID: "spwn-premint", ConvID: "",
		ServerURL: "http://127.0.0.1:43212", Password: "premint",
		Cwd: "/tmp/premint", PID: 0,
	}))
	zero, err := FindOpenCodeRuntimeByPID(0)
	require.NoError(t, err)
	assert.Nil(t, zero, "pid 0 is a column default, never a process identity")
}
