package agentd

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScribeSummonGranterDistinguishesDashboardHuman(t *testing.T) {
	r := asDashboardHumanPeer(httptest.NewRequest("POST", "/api/scribe", nil))
	assert.Equal(t, "<human-dashboard>:scribe-summon:correlation-id=abc123",
		scribeSummonGranter(r, "", 0, "abc123"))

	agentRequest := httptest.NewRequest("POST", "/v1/scribe", nil)
	assert.Equal(t, "caller-conv:scribe-summon:correlation-id=abc123",
		scribeSummonGranter(agentRequest, "caller-conv", 0, "abc123"))
	assert.Equal(t, "caller-conv:via-sudo:grant-id=42:scribe-summon:correlation-id=abc123",
		scribeSummonGranter(agentRequest, "caller-conv", 42, "abc123"))
}
