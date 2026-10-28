//go:build linux

package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestHostRouteLeaseCannotAttachThroughHelper(t *testing.T) {
	setupTestDB(t)
	agent, _, err := db.EnsureAgentForConv("host-helper", "helper")
	require.NoError(t, err)
	credential, generation, err := mintRouteHelperCredential(agent, "host-helper")
	require.NoError(t, err)
	defer revokeRouteHelperCredentials("host-helper", "")
	req := httptest.NewRequest(http.MethodPost, routeChannelPath, nil)
	for name, value := range map[string]string{
		routeHelperCredentialHeader: credential,
		"X-Tclaude-Route-Role":      "consumer", "X-Tclaude-Route-ID": "rte_test",
		"X-Tclaude-Route-Agent-ID": agent, "X-Tclaude-Route-Conv-ID": "host-helper",
		"X-Tclaude-Route-Launch-Generation": generation, "X-Tclaude-Route-Group-Generation": "1",
		"X-Tclaude-Route-Lease-ID": hostRouteLeasePrefix + "test",
	} {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	buildMux().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "host leases are not available to helpers")
}
