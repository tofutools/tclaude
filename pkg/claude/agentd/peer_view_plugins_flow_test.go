package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A fused view's Plugins section reads and manages a peer's plugins through
// its peer view, which serves them only under unrestricted trust.
func TestPeerViewPluginsNeedUnrestrictedTrust(t *testing.T) {
	fh := newFedHarness(t)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/plugins", nil))
	require.Equal(t, 403, rec.Code, "a restricted peer reads no plugins: %s", rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/plugins", map[string]any{"name": "probe", "steps": []any{map[string]any{"name": "s", "check": "true"}}}))
	require.Equal(t, 403, rec.Code, "a restricted peer installs no plugin: %s", rec.Body.String())

	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/plugins", map[string]any{"name": "probe", "steps": []any{map[string]any{"name": "s", "check": "true"}}}))
	require.Less(t, rec.Code, 300, rec.Body.String())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/plugins", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var body struct {
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Plugins, 1)
	require.Equal(t, "probe", body.Plugins[0].Name)
}
