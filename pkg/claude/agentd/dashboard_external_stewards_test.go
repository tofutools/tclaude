package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// The peer-view allowlist refuses these routes first. This is the line behind it,
// so a route moved onto that list still can't hand a peer a credential.
func TestDashboardExternalStewardRoute_RefusesPeerAuthority(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Collab: true}
	require.NoError(t, config.Save(cfg))

	reached := false
	route := dashboardExternalStewardRoute(func(http.ResponseWriter, *http.Request) { reached = true })
	for name, ctx := range map[string]context.Context{
		"peer action": context.WithValue(context.Background(), peerActionKey{}, &peerActionAuthority{}),
		"peer grant":  context.WithValue(context.Background(), peerGrantKey{}, &peerGrantAuthority{}),
	} {
		w := httptest.NewRecorder()
		route(w, dashboardRequest(http.MethodGet, "/api/external-stewards", "").WithContext(ctx))
		assert.Equal(t, http.StatusForbidden, w.Code, name)
		assert.False(t, reached, name)
	}
	route(httptest.NewRecorder(), dashboardRequest(http.MethodGet, "/api/external-stewards", ""))
	assert.True(t, reached, "the operator's own browser gets through")
}
