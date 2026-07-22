package agentd

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
