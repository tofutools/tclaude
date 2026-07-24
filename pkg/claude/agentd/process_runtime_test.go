package agentd

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
)

func TestProcessRunCreateAuditDetailExcludesParams(t *testing.T) {
	request := processRunCreateRequest{
		TemplateID: "release", Params: map[string]string{"token": "secret"},
		AuthorizeProgramProfiles: []string{"deploy", "report"},
	}
	detail := processRunCreateAuditDetail(request)
	assert.Contains(t, detail, "release")
	assert.Contains(t, detail, `["deploy","report"]`)
	assert.False(t, strings.Contains(detail, "token") || strings.Contains(detail, "secret"), detail)
}

func TestProcessRunCreateAuditDoesNotPrebufferRequestBody(t *testing.T) {
	route, _, _, ok := matchAuditRoute(http.MethodPost, "/v1/process/runs")
	require.True(t, ok)
	assert.Nil(t, route.describe, "the bounded handler supplies audit detail after strict decoding")
}

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

// TestProcessRunClaimAccountsUnderTheSameLockTheViewReads pins the ordering
// property that makes the accounting windows safe at all: the owner holds the
// accounting lock ACROSS its planning commit, so a reader — which takes the
// same lock — cannot slip between "the command became durable" and "its owner
// accounts for it". The flow tests exercise the real handoff windows; this
// states the invariant they rely on, without depending on timing.
func TestProcessRunClaimAccountsUnderTheSameLockTheViewReads(t *testing.T) {
	claim := &processRunClaim{accounted: map[string]struct{}{}}
	committed := false

	_, err := claim.plan(func() (*executor.Dispatch, error) {
		committed = true
		// The accounting lock is the same one accountedNodes takes, and it is
		// already held while this commit runs. Nothing else has taken it, so a
		// failed acquisition here can only mean plan is holding it.
		assert.False(t, claim.mu.TryLock(),
			"the planning commit must run with the accounting lock held")
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, committed)

	// And it is released afterwards, so readers are not starved.
	require.True(t, claim.mu.TryLock())
	claim.mu.Unlock()
	assert.Empty(t, claim.accountedNodes(), "a commit that planned nothing accounts nothing")
}
