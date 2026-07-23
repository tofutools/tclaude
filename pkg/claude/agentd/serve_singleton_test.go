package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentdSingletonLockCoversCustomSocketsSharingOneDatabase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	release, err := acquireAgentdSingletonLock()
	require.NoError(t, err)
	defer release()

	_, err = acquireAgentdSingletonLock()
	assert.ErrorContains(t, err, "another agentd already owns")

	release()
	releaseAgain, err := acquireAgentdSingletonLock()
	require.NoError(t, err)
	releaseAgain()
}
