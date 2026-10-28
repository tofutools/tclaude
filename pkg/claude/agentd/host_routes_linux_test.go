//go:build linux

package agentd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/testutil"
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

func TestHostRouteLinuxHelperDiscovery(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(closeAllHostRoutes)
	agent, _, err := db.EnsureAgentForConv("host-discovery", "publisher")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("host-discovery-group", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: "host-discovery"}))
	credential, generation, err := mintRouteHelperCredential(agent, "host-discovery")
	require.NoError(t, err)
	defer revokeRouteHelperCredentials("host-discovery", "")
	socket := filepath.Join(testutil.CanonicalTempDir(t), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{
		Handler: withIdentity(buildMux()), ReadHeaderTimeout: time.Second,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if unixConn, ok := conn.(*net.UnixConn); ok {
				return context.WithValue(ctx, unixConnKey{}, unixConn)
			}
			return ctx
		},
	}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, routeadapter.ValidateHelper(ctx, routeadapter.HelperConfig{
		SocketPath: socket, AgentID: agent, ConvID: "host-discovery", LaunchGeneration: generation,
		Credential: credential, GroupIDs: []int64{groupID},
	}))
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello through the Linux publisher helper")
	}))
	defer target.Close()
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	route, err := db.CreateAgentRoute(groupID, agent, "host-discovery", generation, group.RouteGeneration, "web", "tcp", "tcp://"+strings.TrimPrefix(target.URL, "http://"))
	require.NoError(t, err)
	// The helper also resolves individual routes without harness ancestry.
	var discovered routeView
	require.NoError(t, routeadapter.GetUnixJSON(ctx, socket, "/v1/routes/"+route.ID, credential, &discovered))
	require.Equal(t, route.ID, discovered.ID)
	var listed struct {
		Routes []routeView `json:"routes"`
	}
	require.NoError(t, routeadapter.GetUnixJSON(ctx, socket, "/v1/routes?group=host-discovery-group", credential, &listed))
	require.Len(t, listed.Routes, 1)
	helperCtx, stopHelper := context.WithCancel(ctx)
	helperDone := make(chan error, 1)
	go func() {
		helperDone <- routeadapter.RunHelper(helperCtx, routeadapter.HelperConfig{
			SocketPath: socket, AgentID: agent, ConvID: "host-discovery", LaunchGeneration: generation,
			Credential: credential, GroupIDs: []int64{groupID}, PollInterval: 10 * time.Millisecond,
		})
	}()
	defer func() { stopHelper(); <-helperDone }()
	lease, err := openHostRoute(route, 0)
	require.NoError(t, err)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + lease.Endpoint)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return err == nil && response.StatusCode == http.StatusOK && string(body) == "hello through the Linux publisher helper"
	}, 5*time.Second, 20*time.Millisecond)
	// Invalid capabilities and non-members must still be refused.
	req := httptest.NewRequest(http.MethodGet, "/v1/routes?group=host-discovery-group", nil)
	req.Header.Set(routeHelperCredentialHeader, "invalid")
	denied := httptest.NewRecorder()
	buildMux().ServeHTTP(denied, req)
	require.Equal(t, http.StatusUnauthorized, denied.Code)
	otherGroup, err := db.CreateAgentGroup("host-other-group", "")
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, "/v1/routes?group_id="+strconv.FormatInt(otherGroup, 10), nil)
	req.Header.Set(routeHelperCredentialHeader, credential)
	denied = httptest.NewRecorder()
	buildMux().ServeHTTP(denied, req)
	require.Equal(t, http.StatusForbidden, denied.Code)
	var refusal map[string]any
	require.NoError(t, json.Unmarshal(denied.Body.Bytes(), &refusal))
	require.Equal(t, "route_not_member", refusal["code"])
	var rejected any
	err = routeadapter.GetUnixJSON(ctx, socket, "/v1/routes?group=host-discovery-group", "invalid", &rejected)
	require.ErrorContains(t, err, "route_helper_auth")
	err = routeadapter.GetUnixJSON(ctx, socket, "/v1/routes?group_id="+strconv.FormatInt(otherGroup, 10), credential, &rejected)
	require.ErrorContains(t, err, "route_not_member")

}
