package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestUnrestrictedPeerCopiesACatalogItemAsListed: the fused dock copies an
// item to a node trusting this one unrestricted by posting the item as a
// node lists it (less its id and timestamps) to that node's create route
// (dock.js copyDirect). Every kind's listed shape must create the same item.
func TestUnrestrictedPeerCopiesACatalogItemAsListed(t *testing.T) {
	fh := newFedHarness(t)
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) []byte {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		require.Less(t, rec.Code, 300, "%s %s: %s", method, path, rec.Body.String())
		return rec.Body.Bytes()
	}
	kinds := []struct {
		list, field string
		create      map[string]any
	}{
		{"/api/spawn-profiles", "profiles", map[string]any{"name": "cheap", "harness": "claude", "model": "haiku", "effort": "low", "descr": "cheap runs"}},
		{"/api/roles", "roles", map[string]any{"name": "checker", "descr": "checks", "brief": "Check it."}},
		{"/api/saved-prompts", "saved_prompts", map[string]any{"name": "rules", "body": "Follow the rules."}},
		{"/api/templates", "templates", map[string]any{"name": "crew", "descr": "a crew"}},
		{"/api/sandbox-profiles", "", map[string]any{"name": "tight", "harness_config": "read"}},
	}
	listed := func(list, field, name string) map[string]any {
		t.Helper()
		var items []map[string]any
		if field == "" {
			require.NoError(t, json.Unmarshal(call("GET", list, nil), &items))
		} else {
			var snap map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(call("GET", "/api/snapshot", nil), &snap))
			require.NoError(t, json.Unmarshal(snap[field], &items))
		}
		for _, item := range items {
			if item["name"] == name {
				for _, key := range []string{"id", "created_at", "updated_at"} {
					delete(item, key)
				}
				return item
			}
		}
		t.Fatalf("%s lists no %s", list, name)
		return nil
	}
	for _, k := range kinds {
		name := k.create["name"].(string)
		call("POST", k.list, k.create)
		item := listed(k.list, k.field, name)
		call("DELETE", k.list+"/"+name, nil)
		body := map[string]any{}
		for key, v := range item {
			body[key] = v
		}
		if k.field == "" {
			body["network_access"] = "" // sandboxProfileForWire
		}
		call("POST", k.list, body)
		require.Equal(t, item, listed(k.list, k.field, name), "%s copies as listed", k.list)
	}
}
