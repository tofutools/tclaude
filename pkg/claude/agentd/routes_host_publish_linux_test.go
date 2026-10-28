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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHostOnlyPublicationWithoutGroup(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(closeAllHostRoutes)
	agentID, _, err := db.EnsureAgentForConv("host-only-publisher", "publisher")
	require.NoError(t, err)
	_, _, err = db.EnsureAgentForConv("host-only-other", "other")
	require.NoError(t, err)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "host-only web UI") }))
	defer target.Close()
	mux := buildMux()
	request := func(method, path, conv, body string) *httptest.ResponseRecorder {
		req := AsAgentPeer(httptest.NewRequest(method, path, strings.NewReader(body)), conv)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	body := `{"host":true,"name":"web","target":"tcp://` + strings.TrimPrefix(target.URL, "http://") + `"}`
	// A group-publication grant is not host-publication authority.
	require.NoError(t, db.GrantAgentPermission("host-only-publisher", PermRoutesPublish, "test"))
	require.Equal(t, http.StatusForbidden, request("POST", "/v1/routes/publish", "host-only-publisher", body).Code)
	require.NoError(t, db.GrantAgentPermission("host-only-publisher", PermRoutesPublishHost, "test"))
	require.Equal(t, http.StatusConflict, request("POST", "/v1/routes/publish", "host-only-publisher", body).Code)
	args := clcommon.SpawnArgs{ConvID: "host-only-publisher", Harness: "claude", SandboxImplementation: "tclaude-layer", EffectiveSandbox: &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{NetworkAccess: sandboxpolicy.NetworkAccessNone}}}
	require.NoError(t, prepareRouteHelperResumeArgs(&args))
	require.Equal(t, []int64{0}, args.RouteHelperGroupIDs)
	require.NotEmpty(t, args.RouteHelperCredential)
	body = strings.Replace(body, `"name":"web"`, `"name":"web","launch_generation":"`+args.RouteHelperLaunchGeneration+`"`, 1)
	defer revokeRouteHelperCredentials(args.ConvID, "")
	require.Equal(t, http.StatusBadRequest, request("POST", "/v1/routes/publish", args.ConvID, `{"host":true,"group":"team"}`).Code)
	rec := request("POST", "/v1/routes/publish", args.ConvID, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var view routeView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.True(t, view.HostOnly)
	require.Zero(t, view.GroupID)
	groups, err := db.ListGroupsForAgent(agentID)
	require.NoError(t, err)
	require.Empty(t, groups)
	route, err := db.GetAgentRoute(view.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, request("POST", "/v1/routes/"+route.ID+"/open", args.ConvID, `{}`).Code)
	require.Equal(t, http.StatusNotFound, request("GET", "/v1/routes/"+route.ID, "host-only-other", "").Code)
	listed := request("GET", "/v1/routes?host=true", "host-only-other", "")
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	require.NotContains(t, listed.Body.String(), route.ID)
	socket := filepath.Join(testutil.CanonicalTempDir(t), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: withIdentity(mux), ReadHeaderTimeout: time.Second,
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
	cfg := routeadapter.HelperConfig{SocketPath: socket, AgentID: agentID, ConvID: args.ConvID, LaunchGeneration: args.RouteHelperLaunchGeneration, Credential: args.RouteHelperCredential, GroupIDs: args.RouteHelperGroupIDs, PollInterval: 10 * time.Millisecond}
	require.NoError(t, routeadapter.ValidateHelper(ctx, cfg))
	helperCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- routeadapter.RunHelper(helperCtx, cfg) }()
	defer func() { stop(); <-done }()
	require.Eventually(t, func() bool { return GroupRouteBroker().PublisherAttached(route.ID) }, 3*time.Second, 10*time.Millisecond)
	lease, err := openHostRouteMode(route, 0, "http")
	require.NoError(t, err)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + lease.Endpoint + "/")
	require.NoError(t, err)
	contents, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "host-only web UI", string(contents))
	snapshot := dashboardRouteMap{Routes: []dashboardRoute{}}
	appendDashboardHostPublications(&snapshot)
	require.Len(t, snapshot.Routes, 1)
	require.True(t, snapshot.Routes[0].HostOnly)
	// The host-specific grant does not permit publishing to a group.
	require.NoError(t, db.SetAgentPermissionOverride(args.ConvID, PermRoutesPublish, db.PermEffectDeny, "test"))
	groupID, err := db.CreateAgentGroup("host-permission-test", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: args.ConvID}))
	require.Equal(t, http.StatusForbidden, request("POST", "/v1/routes/publish", args.ConvID, strings.Replace(body, `"host":true`, `"group":"host-permission-test"`, 1)).Code)
	groupRoutes, err := db.ListAgentRoutes(groupID)
	require.NoError(t, err)
	require.Empty(t, groupRoutes)
	require.NoError(t, db.SetAgentPermissionOverride(args.ConvID, PermRoutesPublishHost, db.PermEffectDeny, "test"))
	require.Eventually(t, func() bool { return len(hostRoutesForRoute(route.ID)) == 0 }, time.Second, 10*time.Millisecond)
	_, err = client.Get("http://" + lease.Endpoint + "/")
	require.Error(t, err)
}

func TestHostPermissionDoesNotBreakUnsupportedResume(t *testing.T) {
	setupTestDB(t)
	_, _, err := db.EnsureAgentForConv("host-open-permission", "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission("host-open-permission", PermRoutesPublishHost, "test"))
	args := clcommon.SpawnArgs{ConvID: "host-open-permission", Harness: "claude"}
	require.NoError(t, prepareRouteHelperResumeArgs(&args))
	require.Empty(t, args.RouteHelperCredential)
	require.Empty(t, args.RouteHelperGroupIDs)
}
