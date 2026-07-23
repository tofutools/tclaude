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
	}))

	runtime, err := GetOpenCodeRuntimeByConvID("ses_test")
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, "spwn-test", runtime.SessionID)
	assert.Equal(t, "private", runtime.Password)

	missing, err := GetOpenCodeRuntimeByConvID("ses_missing")
	require.NoError(t, err)
	assert.Nil(t, missing)
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
		SET updated_at = '2020-01-01T00:00:00Z' WHERE session_id = 'spwn-old'`)
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
