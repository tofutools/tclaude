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
