package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetAgentGroupDefaultContextIfMatch(t *testing.T) {
	setupTestDB(t)
	_, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	_, err = SetAgentGroupDefaultContext("alpha", "first")
	require.NoError(t, err)

	n, hash, err := SetAgentGroupDefaultContextIfMatch("alpha", "second", GroupContextHash("first"))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.Equal(t, GroupContextHash("second"), hash)

	n, hash, err = SetAgentGroupDefaultContextIfMatch("alpha", "third", GroupContextHash("first"))
	assert.ErrorIs(t, err, ErrGroupContextConflict)
	assert.Zero(t, n)
	assert.Equal(t, GroupContextHash("second"), hash, "a conflict reports the current hash")

	n, _, err = SetAgentGroupDefaultContextIfMatch("missing", "x", GroupContextHash(""))
	require.NoError(t, err)
	assert.Zero(t, n, "no such group")

	// An empty context has a hash too: expecting "still empty" works.
	_, err = SetAgentGroupDefaultContext("alpha", "")
	require.NoError(t, err)
	n, _, err = SetAgentGroupDefaultContextIfMatch("alpha", "set", GroupContextHash(""))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}
