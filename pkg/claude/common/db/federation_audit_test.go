package db

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFederationActivityUnifiesMetadataAndFiltersBeforeLimit(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, TrustFederationPeer(FederationPeer{InstanceID: "peer-a", Label: "alice", PubKey: bytes.Repeat([]byte{1}, 32)}))
	require.NoError(t, TrustFederationPeer(FederationPeer{InstanceID: "peer-b", Label: "bob", PubKey: bytes.Repeat([]byte{2}, 32)}))
	old := time.Now().Add(-2 * time.Hour)
	_, err := InsertAuditLog(AuditLogEntry{At: old, Verb: "sessions.attach.open", ActorLabel: "remote:alice", Detail: "secret-audit-detail", Source: "federation", Status: 200})
	require.NoError(t, err)
	require.NoError(t, InsertFederationOutbox(FederationOutboxRow{EnvelopeID: "mail-a", Kind: "mail", ToInstance: "peer-a", Sealed: []byte("secret-envelope"), Subject: "secret-subject", BodyPreview: "secret-preview", ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, InsertFederationOutbox(FederationOutboxRow{EnvelopeID: "mail-b", Kind: "mail", ToInstance: "peer-b", Sealed: []byte("secret-envelope"), ExpiresAt: time.Now().Add(time.Hour)}))
	_, err = InsertFederationSpawnRequest(&FederationSpawnRequest{FromInstance: "peer-a", EnvelopeID: "spawn-a", GroupName: "team", Brief: "secret-brief", ExpiresAt: time.Now().Add(time.Hour)}, 0)
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO federation_jobs(id,direction,peer,fingerprint,state,request,created_at,expires_at) VALUES('job-a','out','peer-a','','complete','{"argv":["secret-command"]}',?,?)`, dbTime(time.Now()), dbTime(time.Now().Add(time.Hour)))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO federation_bundle_offers(id,peer,direction,kind,descriptor,bytes,state,created_at,expires_at) VALUES('offer-a','peer-a','in','config','{"note":"secret-offer"}',0,'ready',?,?)`, dbTime(time.Now()), dbTime(time.Now().Add(time.Hour)))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO federation_teleports(direction,peer,offer,chain,source_agent,created_at,state,snapshot) VALUES('out','peer-a','teleport-a','chain','agent-source',?,'complete','{"credentials":"secret-mode"}')`, dbTime(time.Now()))
	require.NoError(t, err)
	require.NoError(t, IssueModelProxyLease(ModelProxyLease{ID: "lease-a", Peer: "peer-a", Request: "spawn-a", Kind: "spawn", Proxy: "main", IdleSeconds: 86400}))
	group, err := CreateAgentGroup("routes", "")
	require.NoError(t, err)
	actor, err := AllocateAgent("route-publisher", "test")
	require.NoError(t, err)
	_, err = InsertFederationInboundMessage(&AgentMessage{FromConv: "remote", ToConv: "route-publisher", Body: "secret-mail"}, FederationInbound{FromInstance: "peer-a", EnvelopeID: "inbound-a"}, time.Now().Add(time.Hour), 0, nil)
	require.NoError(t, err)
	_, err = InsertFederationInboundHumanMessage(&HumanMessage{GroupName: FederationHumanGroup("peer-a"), FromTitle: "alice", Body: "secret-human-message"}, "peer-a", "human-a", time.Now().Add(time.Hour), 0)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO agent_routes(id,group_id,publisher_agent_id,publisher_conv_id,publisher_launch_generation,group_generation,name,transport,target,state,created_at) VALUES('route-a',?,?,'route-publisher','generation',1,'test','tcp','secret-endpoint','ready',?)`, group, actor, dbTime(time.Now()))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO federation_route_mirrors(route_id,peer,remote_route,created_at) VALUES('route-a','peer-a','remote-route',?)`, dbTime(time.Now()))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO agent_route_leases(id,route_id,consumer_agent_id,consumer_launch_generation,group_generation,state,opened_at) VALUES('route-lease','route-a',?,'generation',1,'open',?)`, actor, dbTime(time.Now()))
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO federation_route_proxies(lease_id,peer,route_id,created_at) VALUES('route-lease','peer-a','route-a',?)`, dbTime(time.Now()))
	require.NoError(t, err)
	budget := ModelProxyBudget{Requests: 100, Tokens: 100, PeerRequests: 100, PeerTokens: 100, SessionRequests: 100, SessionTokens: 100}
	require.NoError(t, ReserveModelProxyRequest(ModelProxyUsage{ID: "model-request", Peer: "peer-a", Proxy: "main", Session: "session", Model: "sonnet", Day: time.Now().Format("2006-01-02"), ChargedTokens: 10}, budget))
	rows, err := ListFederationActivity("", time.Time{}, 200)
	require.NoError(t, err)
	require.Len(t, rows, 13)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret-")
	sources := map[string]bool{}
	for i, row := range rows {
		sources[row.Source] = true
		if i > 0 {
			require.False(t, row.At.After(rows[i-1].At))
		}
	}
	for _, source := range []string{"audit", "outbox", "spawns", "jobs", "offers", "teleports", "model_leases", "inbound", "operator_inbox", "routes", "model_requests"} {
		require.True(t, sources[source], source)
	}
	filtered, err := ListFederationActivity("peer-a", old.Add(time.Minute), 2)
	require.NoError(t, err)
	require.Len(t, filtered, 2)
	for _, row := range filtered {
		require.Equal(t, "peer-a", row.Peer)
		require.NotEqual(t, "audit", row.Source)
	}
	filtered, err = ListFederationActivity("peer-b", time.Time{}, 1)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Equal(t, "outbox:mail-b", filtered[0].ID)
	filtered, err = ListFederationActivity("", time.Now().Add(time.Hour), 200)
	require.NoError(t, err)
	require.Empty(t, filtered)
	filtered, err = ListFederationActivity("", time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), 200)
	require.NoError(t, err)
	require.Empty(t, filtered, "far-future timestamps cannot wrap into the stored epoch")
	filtered, err = ListFederationActivity("", time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC), 200)
	require.NoError(t, err)
	require.Len(t, filtered, 13)

}
