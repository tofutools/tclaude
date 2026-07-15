package agentd

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScribeApprovalGranterDistinguishesDashboardHuman(t *testing.T) {
	r := asDashboardHumanPeer(httptest.NewRequest("POST", "/api/scribe", nil))
	assert.Equal(t, "<human-dashboard>:scribe-summon:approval-id=abc123",
		scribeApprovalGranter(r, "", "abc123"))

	agentRequest := httptest.NewRequest("POST", "/v1/scribe", nil)
	assert.Equal(t, "caller-conv:scribe-summon:approval-id=abc123",
		scribeApprovalGranter(agentRequest, "caller-conv", "abc123"))
}
