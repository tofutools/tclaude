package agentd_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func serveRouteAgent(t *testing.T, f *testharness.Flow, method, path, convID string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := testharness.JSONRequest(t, method, path, body)
	req = agentd.AsAgentPeer(req, convID)
	rec := testharness.Serve(f.Mux, req)
	var out map[string]any
	if rec.Body.Len() != 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode route response: %s", rec.Body.String())
	}
	return rec, out
}

func TestRoutesBroker_DatabaseAuthorityClosesWithdrawnChannels(t *testing.T) {
	f := newFlow(t)
	const publisher = "route-broker-publisher"
	const consumer = "route-broker-consumer"
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveConvWithTitle(consumer, "consumer")
	f.HaveGroup("broker-group")
	f.HaveMember("broker-group", publisher)
	f.HaveMember("broker-group", consumer)
	g, err := db.GetAgentGroupByName("broker-group")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "test"))

	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "broker-group", "name": "api", "target": "tcp://127.0.0.1:43127",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)
	rec, lease := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "broker-group",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	publisherAuth := routebroker.PublisherAuth{
		RouteID: routeID, AgentID: route["publisher_agent_id"].(string), ConvID: route["publisher_conv_id"].(string),
		LaunchGeneration: route["publisher_launch_generation"].(string), GroupGeneration: int64(route["group_generation"].(float64)),
	}
	consumerAuth := routebroker.ConsumerAuth{
		LeaseID: lease["id"].(string), RouteID: routeID, AgentID: lease["consumer_agent_id"].(string), ConvID: lease["consumer_conv_id"].(string),
		LaunchGeneration: lease["consumer_launch_generation"].(string), GroupGeneration: int64(lease["group_generation"].(float64)),
	}
	b := agentd.NewGroupRouteBrokerForTest()
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	publisherBroker, publisherPeer := net.Pipe()
	consumerBroker, consumerPeer := net.Pipe()
	publisherDone := make(chan error, 1)
	consumerDone := make(chan error, 1)
	go func() { publisherDone <- b.AttachPublisher(context.Background(), publisherAuth, publisherBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().PublisherChannels == 1 }, time.Second, time.Millisecond)
	go func() { consumerDone <- b.AttachConsumer(context.Background(), consumerAuth, consumerBroker) }()
	t.Cleanup(func() { _ = publisherPeer.Close(); _ = consumerPeer.Close() })

	// Withdraw through the M1 API. The broker's periodic authority check must
	// fail already-attached channels closed; no payload is needed to prove this
	// lifecycle seam.
	writeRouteBrokerFrame(t, consumerPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	_ = readRouteBrokerFrame(t, publisherPeer)
	rec, _ = serveRouteAgent(t, f, http.MethodDelete, "/v1/routes/"+routeID, publisher, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_ = consumerPeer.SetReadDeadline(time.Now().Add(time.Second))
	_, readErr := consumerPeer.Read(make([]byte, 1))
	require.Error(t, readErr)
	select {
	case <-publisherDone:
	case <-time.After(time.Second):
		t.Fatal("withdrawn publisher channel did not close")
	}
	select {
	case <-consumerDone:
	case <-time.After(time.Second):
		t.Fatal("withdrawn consumer channel did not close")
	}
}

func writeRouteBrokerFrame(t *testing.T, conn net.Conn, frame routebroker.Frame) {
	t.Helper()
	require.NoError(t, routebroker.WriteFrame(conn, frame, routebroker.MaxFramePayload))
}

func readRouteBrokerFrame(t *testing.T, conn net.Conn) routebroker.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := routebroker.ReadFrame(conn, routebroker.MaxFramePayload)
	require.NoError(t, err)
	return frame
}

func TestRoutesAuthority_ExactGroupAndIndependentCapabilities(t *testing.T) {
	f := newFlow(t)
	const publisher = "route-publisher-0001"
	const consumer = "route-consumer-0002"
	const stranger = "route-stranger-0003"
	const inspector = "route-inspector-0004"
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveConvWithTitle(consumer, "consumer")
	f.HaveConvWithTitle(stranger, "stranger")
	f.HaveConvWithTitle(inspector, "inspector")
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveGroup("caps")
	f.HaveMember("alpha", publisher)
	f.HaveMember("beta", publisher)
	f.HaveMember("beta", stranger)
	f.HaveMember("caps", publisher)
	f.HaveMember("caps", consumer)
	f.HaveMember("caps", inspector)

	alpha, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, alpha)
	beta, err := db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	require.NotNil(t, beta)
	caps, err := db.GetAgentGroupByName("caps")
	require.NoError(t, err)
	require.NotNil(t, caps)
	require.NoError(t, db.ReplaceAgentGroupPermissions(alpha.ID, []string{agentd.PermRoutesPublish}, "test"))
	require.NoError(t, db.ReplaceAgentGroupPermissions(beta.ID, nil, "test"))
	require.NoError(t, db.ReplaceAgentGroupPermissions(caps.ID, nil, "test"))

	// A group grant is scoped to its exact target group. The publisher is an
	// alpha member, but cannot use alpha's grant to publish into beta.
	rec, _ := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "alpha", "name": "api", "target": "tcp://127.0.0.1:43127",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	// A caller that belongs to multiple groups must select one explicitly, and
	// the two selection forms may not disagree.
	rec, body := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"name": "ambiguous", "target": "tcp://127.0.0.1:43126",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "route_group", body["code"])
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "caps", "group_id": alpha.ID, "name": "conflicting", "target": "tcp://127.0.0.1:43126",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "route_group", body["code"])
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "beta", "name": "leak", "target": "tcp://127.0.0.1:43128",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_permission", body["code"])

	// Publish and consume are independent. A consumer with neither a group
	// consume grant nor a default must be refused, then can consume through an
	// explicit per-agent grant without gaining publish authority. The member-only
	// inspector has no route slugs; inspection is a membership-only operation.
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "caps", "name": "before-grant", "target": "tcp://127.0.0.1:43129",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_permission", body["code"])
	require.NoError(t, db.GrantAgentPermission(publisher, agentd.PermRoutesPublish, "test"))
	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "caps", "name": "api", "target": "tcp://127.0.0.1:43129",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID, ok := route["id"].(string)
	require.True(t, ok)
	require.NoError(t, db.SetAgentPermissionOverride(consumer, agentd.PermRoutesConsume, db.PermEffectDeny, "test"))
	rec, _ = serveRouteAgent(t, f, http.MethodGet, "/v1/routes/"+routeID, inspector, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "caps",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_permission", body["code"])
	sudoID, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: consumer, Slug: agentd.PermRoutesConsume,
		ExpiresAt: time.Now().Add(time.Hour), GrantedBy: "test", Reason: "route review",
	})
	require.NoError(t, err)
	rec, sudoLease := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "caps",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	sudoLeaseID := sudoLease["id"].(string)
	_, err = db.RevokeSudoGrant(sudoID)
	require.NoError(t, err)
	consumerAgentID, err := db.AgentIDForConv(consumer)
	require.NoError(t, err)
	require.NotEmpty(t, consumerAgentID)
	require.NoError(t, db.CloseAgentRouteLease(sudoLeaseID, consumerAgentID, consumer))
	_, err = db.RevokeAgentPermission(consumer, agentd.PermRoutesConsume)
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(consumer, agentd.PermRoutesConsume, "test"))
	rec, lease := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "caps",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	leaseID, ok := lease["id"].(string)
	require.True(t, ok)
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", consumer, map[string]any{
		"group": "caps", "name": "consumer-publish", "target": "tcp://127.0.0.1:43130",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_permission", body["code"])

	// Only the lease owner may close it, even when the publisher is granted
	// the independent consume capability.
	require.NoError(t, db.GrantAgentPermission(publisher, agentd.PermRoutesConsume, "test"))
	rec, body = serveRouteAgent(t, f, http.MethodDelete, "/v1/routes/leases/"+leaseID, publisher, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_not_owner", body["code"])
	rec, _ = serveRouteAgent(t, f, http.MethodDelete, "/v1/routes/leases/"+leaseID, consumer, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Membership is required independently of permission provenance: a beta
	// member cannot inspect alpha's registry.
	rec, body = serveRouteAgent(t, f, http.MethodGet, "/v1/routes?group=alpha", stranger, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_not_member", body["code"])
}

// An ordinary publisher process exit must withdraw its routes at the same
// authoritative lifecycle seam that marks the session exited. The reaper is
// the production path for a pane that disappears without a SessionEnd hook.
func TestRoutesAuthority_OrdinaryPublisherExitWithdrawsLeases(t *testing.T) {
	f := newFlow(t)
	const publisher = "route-exit-publisher"
	const consumer = "route-exit-consumer"
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveConvWithTitle(consumer, "consumer")
	f.HaveAliveSession(publisher, "route-exit-session", "route-exit-tmux", f.TestCwd("route-exit"))
	f.HaveGroup("exit-group")
	f.HaveMember("exit-group", publisher)
	f.HaveMember("exit-group", consumer)
	g, err := db.GetAgentGroupByName("exit-group")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "test"))

	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "exit-group", "name": "api", "target": "tcp://127.0.0.1:43131",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID, ok := route["id"].(string)
	require.True(t, ok)
	rec, lease := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "exit-group",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	leaseID, ok := lease["id"].(string)
	require.True(t, ok)

	f.MarkOffline("route-exit-tmux")
	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick())

	updated, err := db.GetAgentRoute(routeID)
	require.NoError(t, err)
	require.Equal(t, db.RouteStatePublisherLost, updated.State)
	closed, err := db.GetAgentRouteLease(leaseID)
	require.NoError(t, err)
	require.Equal(t, db.RouteLeaseClosed, closed.State)

	rec, body := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "exit-group",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "route_open_refused", body["code"])
}

func TestRoutesAuthority_GenerationsPublisherLossAndRename(t *testing.T) {
	f := newFlow(t)
	const publisher = "route-generation-pub"
	const consumer = "route-generation-con"
	const added = "route-generation-add"
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveConvWithTitle(consumer, "consumer")
	f.HaveConvWithTitle(added, "added")
	f.HaveGroup("stable")
	f.HaveMember("stable", publisher)
	f.HaveMember("stable", consumer)
	g, err := db.GetAgentGroupByName("stable")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "test"))
	g, err = db.GetAgentGroupByName("stable")
	require.NoError(t, err)

	rec, route := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "stable", "name": "api", "target": "tcp://127.0.0.1:43130",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)
	oldGeneration := route["group_generation"].(float64)

	// Offline membership changes advance the group epoch. A lease carrying the
	// old epoch is rejected, so a stale member cannot reuse route authority.
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g.ID, ConvID: added}))
	rec, body := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "stable", "group_generation": int64(oldGeneration),
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "route_open_refused", body["code"])

	// A known launch generation is equally binding. The explicit predecessor
	// token must not authorize a lease after the session has a newer launch.
	const sessionID = "route-generation-session"
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: sessionID, ConvID: consumer, Cwd: "/tmp", Status: "running"}))
	currentLaunch := strings.Repeat("a", 32)
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, currentLaunch))
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/open", consumer, map[string]any{
		"route_id": routeID, "group": "stable", "launch_generation": strings.Repeat("b", 32),
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "route_generation_stale", body["code"])

	// Stable route identity survives a group rename, while the old publisher
	// conversation does not survive actor rotation.
	_, err = db.RenameAgentGroup("stable", "renamed", "human")
	require.NoError(t, err)
	newPublisher := publisher + "-successor"
	_, err = db.RotateAgentConv(publisher, newPublisher, "test")
	require.NoError(t, err)
	rec, list := serveRouteAgent(t, f, http.MethodGet, "/v1/routes?group=renamed", consumer, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	routes, ok := list["routes"].([]any)
	require.True(t, ok)
	require.Len(t, routes, 1)
	view := routes[0].(map[string]any)
	require.Equal(t, routeID, view["id"])
	require.Equal(t, "renamed", view["group"])
	require.Equal(t, db.RouteStatePublisherLost, view["state"])
}

func TestRoutesAuthority_OnlineMembershipMutationRequiresOfflineRoster(t *testing.T) {
	f := newFlow(t)
	const online = "route-online-member"
	const added = "route-offline-add"
	f.HaveConvWithTitle(online, "online")
	f.HaveConvWithTitle(added, "added")
	f.HaveAliveSession(online, "route-online-session", "route-online-tmux", f.TestCwd("route-online"))
	f.HaveGroup("locked")
	f.HaveMember("locked", online)
	g, err := db.GetAgentGroupByName("locked")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublish}, "test"))

	add := func() *httptest.ResponseRecorder {
		req := testharness.JSONRequest(t, http.MethodPost, "/v1/groups/locked/members", map[string]any{"conv": added})
		return testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
	}
	rec := add()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var refusal map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refusal))
	require.Equal(t, "route_membership_locked", refusal["code"])

	remove := testharness.JSONRequest(t, http.MethodDelete, "/v1/groups/locked/members/"+online, nil)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(remove))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	f.MarkOffline("route-online-tmux")
	rec = add()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	remove = testharness.JSONRequest(t, http.MethodDelete, "/v1/groups/locked/members/"+added, nil)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(remove))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}
