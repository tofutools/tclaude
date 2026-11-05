package agentd

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An enrolled launch whose wrapper already reported a failure hands that
// failure back for executeSpawn's confirmed-failure unwind, without settling.
func TestSpawnSettleConcludeReturnsAReportedWrapperFailure(t *testing.T) {
	wrapperFailure := make(chan error, 1)
	boom := errors.New("wrapper died")
	wrapperFailure <- boom
	outcome, failure, keep, wrapperErr := spawnSettle{label: "spwn-settle", launchEnroll: true}.
		conclude("", "spwn-settle", wrapperFailure)
	assert.Same(t, boom, wrapperErr)
	assert.Nil(t, outcome)
	assert.Nil(t, failure)
	assert.False(t, keep)
}

// A launch with no conversation, no enrollment and no async response times
// out and releases the route-helper credential.
func TestSpawnSettleConcludeTimesOutAnUnconfirmedSyncLaunch(t *testing.T) {
	outcome, failure, keep, wrapperErr := spawnSettle{label: "spwn-slow", pollBudget: time.Second}.
		conclude("", "spwn-slow", make(chan error))
	require.NoError(t, wrapperErr)
	assert.Nil(t, outcome)
	require.NotNil(t, failure)
	assert.Equal(t, http.StatusGatewayTimeout, failure.Status)
	assert.Equal(t, "timeout", failure.Kind)
	assert.False(t, keep)
}
