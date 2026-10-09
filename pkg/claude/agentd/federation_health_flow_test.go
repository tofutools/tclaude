package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederation_FleetHealthWatchConfigurationAndMetadata(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveConvWithTitle("health-reader", "reader")
	for _, path := range []string{"health", "watch"} {
		rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/federation/nodes/"+path, nil), "health-reader"))
		require.Equal(t, 403, rec.Code)
	}
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/nodes/health?peer=bob", nil)
	require.Equal(t, 200, rec.Code)
	require.NotContains(t, rec.Body.String(), `"resources":true`)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/health?peer=bob", map[string]any{"resources": true, "debounce_seconds": 1})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/health?peer=bob", map[string]any{"disk_free_percent": 101})
	require.Equal(t, 400, rec.Code)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fh.f.Mux.ServeHTTP(w, agentd.AsHumanPeer(r)) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/federation/nodes/watch", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	now := time.Now().UTC()
	n := &proto.NodeMetadata{Schema: 1, Resources: proto.NodeResources{Status: "current", ObservedAt: &now, DataDisk: &proto.NodeDisk{TotalBytes: 100, AvailableBytes: 5}}}
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Node: n, NodeAt: now}))
	var event struct{ Kind, Instance, Peer, Message string }
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&event))
	require.Equal(t, "disk", event.Kind)
	require.Equal(t, fh.peer.id.ID(), event.Instance)
	require.Equal(t, "bob", event.Peer)
	fedEventually(t, "fleet notice in operator inbox", func() bool {
		messages, err := db.ListHumanMessages()
		require.NoError(t, err)
		for _, m := range messages {
			if m.FromTitle == "Fleet health" && m.Subject == "Fleet health: bob disk" {
				return true
			}
		}
		return false
	})
}

func openHealthWatch(t *testing.T, fh *fedHarness) *json.Decoder {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fh.f.Mux.ServeHTTP(w, agentd.AsHumanPeer(r)) }))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/federation/nodes/watch", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return json.NewDecoder(resp.Body)
}
func TestFederation_FleetHealthWatchRotationAndPolicyContinuation(t *testing.T) {
	fh := newFedHarness(t)
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.IdentityRotationSeconds = 1
		return nil
	})
	require.NoError(t, err)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/health?peer=bob", map[string]any{"presence": false, "resources": true})
	require.Equal(t, 200, rec.Code)
	dec := openHealthWatch(t, fh)
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	rotation, err := proto.NewRotation(fh.peer.id, next, "", 1, time.Now(), time.Second)
	require.NoError(t, err)
	fh.peer.send(fh.peer.envelope(proto.KindIdentityRotation, proto.Endpoint{}, rotation))
	var event struct{ Kind, Instance string }
	require.NoError(t, dec.Decode(&event))
	require.Equal(t, "identity_pending", event.Kind)
	require.NoError(t, dec.Decode(&event))
	require.Equal(t, "identity_accepted", event.Kind)
	require.Equal(t, next.ID(), event.Instance)
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/nodes/health?peer=bob", nil)
	require.Equal(t, 200, rec.Code)
	var policy config.FederationHealthPolicy
	testharness.DecodeJSON(t, rec, &policy)
	require.True(t, policy.Resources)
	require.NotNil(t, policy.Presence)
	require.False(t, *policy.Presence)
}
func TestFederation_FleetHealthWatchTeleportLeaseRecovery(t *testing.T) {
	fh := newFedHarness(t)
	fedBackupPolicy(t, "manual")
	dec := openHealthWatch(t, fh)
	aid, _ := fedPausedBackup(t, fh)
	var event struct{ Kind, Message string }
	require.NoError(t, dec.Decode(&event))
	require.Equal(t, "teleport_lease_lost", event.Kind)
	require.Contains(t, event.Message, aid)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/teleport/recover", map[string]any{"agent": aid})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	require.NoError(t, dec.Decode(&event))
	require.Equal(t, "teleport_recovered", event.Kind)
	fedEventually(t, "lease loss and recovery notices", func() bool {
		rows, err := db.ListHumanMessages()
		require.NoError(t, err)
		found := 0
		for _, m := range rows {
			if m.FromTitle == "Fleet health" {
				found++
			}
		}
		return found == 2
	})
}
