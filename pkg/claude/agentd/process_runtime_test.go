package agentd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessRunManagerCapsRetainedActiveClaims(t *testing.T) {
	manager := newProcessRunManager()
	claims := make(map[string]*processRunClaim, processRunMaxClaims)
	for i := range processRunMaxClaims {
		id := fmt.Sprintf("run_%02d", i)
		claim, acquired, err := manager.claim(id)
		require.NoError(t, err)
		require.True(t, acquired)
		claims[id] = claim
	}

	_, acquired, err := manager.claim("run_over_capacity")
	assert.False(t, acquired)
	assert.ErrorIs(t, err, errProcessRunCapacity)
	assert.Len(t, manager.claims, processRunMaxClaims)

	for id, claim := range claims {
		manager.release(id, claim)
	}
	manager.wg.Wait()
}
