package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionExecutionBoundaryRoundTrip(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{ID: "spwn-boundary", ConvID: "conv-boundary"}))
	require.NoError(t, SetSessionExecutionBoundary("spwn-boundary", `{"version":1,"path":{"before_pre_launch":"/.tclaude/bin:/usr/bin"}}`))
	raw, err := SessionExecutionBoundary("spwn-boundary")
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"path":{"before_pre_launch":"/.tclaude/bin:/usr/bin"}}`, raw)

	missing, err := SessionExecutionBoundary("missing")
	require.NoError(t, err)
	assert.Empty(t, missing)
}
