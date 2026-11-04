package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewAgentApprovalRequestProjectsTheRequestLine(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/abc/resume?recreate=1", nil)
	req := newAgentApprovalRequest(r, PermAgentResume, "caller", "Caller", 5*time.Second)
	assert.NotEmpty(t, req.id)
	assert.Equal(t, PermAgentResume, req.perm)
	assert.Equal(t, "caller", req.convID)
	assert.Equal(t, "Caller", req.convTitle)
	assert.Equal(t, http.MethodPost, req.method)
	assert.Equal(t, "/v1/agent/abc/resume", req.path)
	assert.Equal(t, "recreate=1", req.rawQuery)
	assert.Equal(t, 5*time.Second, req.timeout)
	assert.False(t, req.createdAt.IsZero())
	assert.Equal(t, 1, cap(req.decision))
	assert.Equal(t, 1, cap(req.extend))
	assert.False(t, req.autoGrantable, "a site opts into always-allow itself")
	assert.Empty(t, req.scopeJSON)

	// A sensitive route is recorded the way the HTTP log records it.
	proxy := httptest.NewRequest(http.MethodGet, "/v1/http/proxy/svc/secret/path?token=x", nil)
	req = newAgentApprovalRequest(proxy, "x", "caller", "", time.Second)
	assert.Equal(t, "/v1/http/proxy/", req.path)
	assert.Empty(t, req.rawQuery)
}
