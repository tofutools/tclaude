package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederation_NodeExportLabelsAndWithdrawal(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	latest := func() proto.CatalogPayload {
		var c proto.CatalogPayload
		es := p.envelopes(proto.KindCatalog)
		if len(es) > 0 {
			require.NoError(t, es[len(es)-1].DecodePayload(&c))
		}
		return c
	}
	fedEventually(t, "initial catalog", func() bool { return len(p.envelopes(proto.KindCatalog)) > 0 })
	require.Nil(t, latest().Node)
	r := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeRead, "scope": "group=private"})
	require.Equal(t, 400, r.Code)
	r = fedHuman(t, f, http.MethodPost, "/v1/federation/node-labels", map[string]any{"add": []string{"gpu", "test-rig"}})
	require.Equal(t, 200, r.Code, r.Body.String())
	r = fedHuman(t, f, http.MethodPost, "/v1/federation/node-labels", map[string]any{"add": []string{"mac", "gpu"}, "remove": []string{"test-rig"}})
	require.Equal(t, 200, r.Code)
	r = fedHuman(t, f, http.MethodGet, "/v1/federation/node-labels", nil)
	require.Contains(t, r.Body.String(), `["gpu","mac"]`)
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.MaxLiveAgents = 4
		return nil
	})
	require.NoError(t, err)
	agentd.RefreshHostMetricsForTest()
	r = fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeRead})
	require.Equal(t, 200, r.Code)
	fedEventually(t, "node export", func() bool { return latest().Node != nil })
	n := latest().Node
	require.NotEmpty(t, n.OS)
	require.NotEmpty(t, n.TclaudeVersion)
	require.Equal(t, 4, n.MaxLiveAgents)
	require.Equal(t, []string{"gpu", "mac"}, n.Labels)
	require.NotNil(t, n.Resources.ObservedAt)
	es := p.envelopes(proto.KindCatalog)
	raw := string(es[len(es)-1].Payload)
	require.NotContains(t, raw, config.DataDir())
	require.NotContains(t, raw, `"path"`)
	require.NotContains(t, raw, `"errors"`)
	require.NotContains(t, raw, `"hostname"`)
	fedEventually(t, "dedicated node publication", func() bool { return len(p.envelopes(proto.KindNodeUpdate)) > 0 })
	r = fedHuman(t, f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeRead})
	require.Equal(t, 200, r.Code)
	fedEventually(t, "node withdrawal", func() bool { return latest().Node == nil })
	setFedTrustLevel(t, fh, "unrestricted")
	fedEventually(t, "implicit node authority", func() bool { return latest().Node != nil })
	f.HaveConvWithTitle("node-writer", "writer")
	r = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/node-labels", map[string]any{"add": []string{"bad"}}), "node-writer"))
	require.Equal(t, 403, r.Code)
	r = fedHuman(t, f, http.MethodPost, "/v1/federation/node-labels", map[string]any{"add": []string{"bad\nlabel"}})
	require.Equal(t, 400, r.Code)
}
func TestFederation_NodeReadScopeFreshnessAndOrdering(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const caller = "node-reader"
	f.HaveConvWithTitle(caller, "reader")
	type view struct {
		*proto.NodeMetadata
		Stale bool `json:"stale"`
	}
	list := func(human bool, match string) []view {
		req := testharness.JSONRequest(t, http.MethodGet, "/v1/federation/nodes?match="+match, nil)
		if human {
			req = agentd.AsHumanPeer(req)
		} else {
			req = agentd.AsAgentPeer(req, caller)
		}
		r := testharness.Serve(f.Mux, req)
		require.Equal(t, 200, r.Code, r.Body.String())
		var rows []view
		testharness.DecodeJSON(t, r, &rows)
		return rows
	}
	at := time.Now().UTC()
	n := proto.NodeMetadata{Schema: 1, OS: "darwin", Arch: "arm64", Labels: []string{"gpu"}, Harnesses: []proto.NodeHarness{{Name: "codex", Version: "1"}}, Resources: proto.NodeResources{Status: "current", ObservedAt: &at, CPU: proto.NodeCPU{LogicalCores: 8}}}
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Node: &n, NodeAt: at, NodeReceivedAt: at.Add(time.Hour), Groups: []proto.CatalogGroup{{Name: "builders", Caps: []string{proto.CapRoster}}}}))
	fedEventually(t, "node stored", func() bool { return len(list(true, "")) == 1 })
	require.Empty(t, list(false, ""))
	require.False(t, list(true, "")[0].Stale)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermNodeRead, "test"))
	require.Empty(t, list(false, ""), "restricted peer requires peer= even with unscoped local grant")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermNodeRead, `{"peer":["`+p.id.ID()+`/builders"]}`, "test"))
	require.Empty(t, list(false, ""), "group-scoped grant cannot expose whole-node metadata")
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermNodeRead, `{"peer":["`+p.id.ID()+`"]}`, "test"))
	require.Len(t, list(false, "os=darwin,label=gpu,harness=codex"), 1)
	require.Empty(t, list(false, "os=linux"))
	_, catalogAt, err := db.GetFederationCatalog(p.id.ID())
	require.NoError(t, err)
	updated := at.Add(100 * time.Millisecond)
	n.Labels = []string{"new"}
	p.send(p.envelope(proto.KindNodeUpdate, proto.Endpoint{}, proto.NodeUpdatePayload{Node: &n, At: updated}))
	fedEventually(t, "node update", func() bool { return len(list(true, "label=new")) == 1 })
	_, after, err := db.GetFederationCatalog(p.id.ID())
	require.NoError(t, err)
	require.Equal(t, catalogAt, after, "node updates do not freshen group presence")
	n.Labels = []string{"old"}
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Node: &n, NodeAt: at, Groups: []proto.CatalogGroup{}}))
	fedEventually(t, "older full catalog processed", func() bool {
		raw, _, _ := db.GetFederationCatalog(p.id.ID())
		var c proto.CatalogPayload
		require.NoError(t, json.Unmarshal([]byte(raw), &c))
		return len(c.Groups) == 0
	})
	require.Len(t, list(true, "label=new"), 1, "delayed catalogs cannot roll node back")
	old := time.Now().Add(-time.Hour)
	n.Resources.ObservedAt = &old
	p.send(p.envelope(proto.KindNodeUpdate, proto.Endpoint{}, proto.NodeUpdatePayload{Node: &n, At: updated.Add(100 * time.Millisecond)}))
	fedEventually(t, "old source observation stale", func() bool { return list(true, "")[0].Stale })
	fh.hub.Close()
	fedEventually(t, "offline marked stale", func() bool { return list(true, "")[0].Stale })
}
