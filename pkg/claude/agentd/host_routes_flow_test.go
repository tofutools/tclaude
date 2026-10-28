package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

func TestHostRoutesFlow(t *testing.T) {
	for _, revoke := range []string{"withdraw", "generation", "publisher-exit", "operator-close", "shutdown"} {
		t.Run(revoke, func(t *testing.T) {
			setupTestDB(t)
			t.Cleanup(closeAllHostRoutes)
			publisher, _, err := db.EnsureAgentForConv("host-publisher", "publisher")
			require.NoError(t, err)
			groupID, err := db.CreateAgentGroup("host-routes", "")
			require.NoError(t, err)
			require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: "host-publisher"}))
			require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish}, "test"))
			group, err := db.GetAgentGroupByID(groupID)
			require.NoError(t, err)
			target, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer target.Close()
			go func() {
				conn, err := target.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
			route, err := db.CreateAgentRoute(groupID, publisher, "host-publisher", "host-generation", group.RouteGeneration, "api", "tcp", "tcp://"+target.Addr().String())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ours, theirs := net.Pipe()
			defer ours.Close()
			defer theirs.Close()
			auth := routebroker.PublisherAuth{RouteID: route.ID, AgentID: publisher, ConvID: "host-publisher", LaunchGeneration: "host-generation", GroupGeneration: group.RouteGeneration}
			ready := make(chan error, 1)
			go func() {
				_ = GroupRouteBroker().AttachPublisherReady(ctx, auth, theirs, func(err error) { ready <- err })
			}()
			select {
			case err := <-ready:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("publisher attach timed out")
			}
			go func() { _ = routeadapter.RunPublisher(ctx, ours, route.Target) }()

			cookie, base := withDashboardAuthForTest(t)
			dashboard := http.NewServeMux()
			registerDashboardHostRoutes(dashboard)
			path := "/api/routes/" + route.ID + "/host"
			request := func(method, path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, base+path, bytes.NewBufferString("{}"))
				req.Header.Set("Origin", base)
				req.AddCookie(cookie)
				rec := httptest.NewRecorder()
				dashboard.ServeHTTP(rec, req)
				return rec
			}
			// An agent cannot obtain host authority, even when it knows the route ID.
			denied := httptest.NewRecorder()
			handleHostRouteOpen(denied, AsAgentPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}")), "host-publisher"))
			require.Equal(t, http.StatusForbidden, denied.Code)
			socket := httptest.NewRecorder()
			buildMux().ServeHTTP(socket, AsAgentPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}")), "host-publisher"))
			require.Equal(t, http.StatusNotFound, socket.Code)
			unauth := httptest.NewRecorder()
			dashboard.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, base+path, bytes.NewBufferString("{}")))
			require.NotEqual(t, http.StatusCreated, unauth.Code)

			rec := request(http.MethodPost, path)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var lease hostRouteLease
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &lease))
			require.Equal(t, "host", lease.ConsumerKind)
			require.Contains(t, lease.Endpoint, "127.0.0.1:")
			require.Len(t, routeViewFor(route).HostLeases, 1)
			// A failed bind leaves neither a lease nor unmatched close audit.
			_, err = openHostRoute(route, connPort(t, lease.Endpoint))
			require.Error(t, err)
			require.Len(t, hostRoutesForRoute(route.ID), 1)
			hostRouteLeases.Lock()
			forged := hostRouteLeases.items[lease.ID].consumer
			hostRouteLeases.Unlock()
			forged.AgentID = publisher
			require.Error(t, (databaseRouteAuthority{}).AuthorizeConsumer(context.Background(), forged))

			snapshot := buildDashboardRouteMap([]*db.AgentGroup{group}, nil, map[int64][]*db.AgentRoute{groupID: {route}}, nil, nil)
			require.Equal(t, "host", snapshot.Routes[0].Consumers[0].ConsumerKind)
			conn, err := net.DialTimeout("tcp4", lease.Endpoint, time.Second)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
			_, err = conn.Write([]byte("opaque"))
			require.NoError(t, err)
			payload := make([]byte, 6)
			_, err = io.ReadFull(conn, payload)
			require.NoError(t, err)
			require.Equal(t, "opaque", string(payload))
			switch revoke {
			case "withdraw":
				withdraw := httptest.NewRecorder()
				buildMux().ServeHTTP(withdraw, AsAgentPeer(httptest.NewRequest(http.MethodPost, "/v1/routes/"+route.ID+"/withdraw", bytes.NewBufferString("{}")), "host-publisher"))
				require.Equal(t, http.StatusOK, withdraw.Code, withdraw.Body.String())
			case "generation":
				require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish, PermRoutesConsume}, "changed"))
			case "publisher-exit":
				require.NoError(t, db.MarkAgentRoutePublisherLost(route.ID, "publisher exited"))
			case "shutdown":
				t.Cleanup(func() {
					hostRouteLeases.Lock()
					hostRouteLeases.closing = false
					hostRouteLeases.Unlock()
				})
				shutdownHostRoutes()
				_, err := openHostRoute(route, 0)
				require.ErrorContains(t, err, "shutting down")
			case "operator-close":
				require.Equal(t, http.StatusNoContent, request(http.MethodDelete, "/api/routes/host/"+lease.ID).Code)
			}
			require.Eventually(t, func() bool {
				c, err := net.DialTimeout("tcp4", lease.Endpoint, 50*time.Millisecond)
				if err == nil {
					_ = c.Close()
					return false
				}
				return true
			}, 3*time.Second, 20*time.Millisecond)
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
			_, err = conn.Read(make([]byte, 1))
			require.Error(t, err)
			var timeout net.Error
			require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "revocation must close the active stream, not leave it blocked")

			require.Empty(t, hostRoutesForRoute(route.ID))
			database, err := db.Open()
			require.NoError(t, err)
			var count int
			require.NoError(t, database.QueryRow(`SELECT COUNT(*) FROM agent_route_audit WHERE lease_id = ? AND action IN ('host-open', 'host-close')`, lease.ID).Scan(&count))
			require.Eventually(t, func() bool {
				err := database.QueryRow(`SELECT COUNT(*) FROM agent_route_audit WHERE route_id = ? AND action IN ('host-open', 'host-close')`, route.ID).Scan(&count)
				return err == nil && count == 2
			}, time.Second, time.Millisecond)
		})
	}
}

func TestHostRoutePortAllocationAvoidsDarwinSandboxClaims(t *testing.T) {
	setupTestDB(t)
	publisher, _, err := db.EnsureAgentForConv("slot-publisher", "publisher")
	require.NoError(t, err)
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := free.Addr().(*net.TCPAddr).Port
	require.NoError(t, free.Close())
	require.NoError(t, db.RegisterDarwinRouteLaunch(publisher, "slot-publisher", "generation", []int{port}))
	listener, err := listenHostRoute(port, "darwin")
	require.ErrorContains(t, err, "reserved by a sandbox")
	require.Nil(t, listener)
	listener, err = listenHostRoute(0, "darwin")
	require.NoError(t, err)
	defer listener.Close()
	require.NotEqual(t, port, listener.Addr().(*net.TCPAddr).Port)
	busy, err := listenHostRoute(listener.Addr().(*net.TCPAddr).Port, "darwin")
	require.Error(t, err)
	require.Nil(t, busy)
}

func connPort(t *testing.T, endpoint string) int {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp4", endpoint)
	require.NoError(t, err)
	return addr.Port
}
