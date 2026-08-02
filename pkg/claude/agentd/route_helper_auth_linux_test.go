//go:build linux

package agentd

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRouteHelperSiblingProcess is the other end of the topology exercised by
// TestRouteHelperSiblingAttachesWithCapability. It is intentionally a child
// process with no harness ancestor: the only proof accepted by the production
// channel handler is the daemon-minted capability.
func TestRouteHelperSiblingProcess(t *testing.T) {
	if os.Getenv("TCLAUDE_ROUTE_HELPER_CHILD") != "1" {
		return
	}
	socketPath := os.Getenv("TCLAUDE_ROUTE_HELPER_SOCKET")
	expected, err := strconv.Atoi(os.Getenv("TCLAUDE_ROUTE_HELPER_EXPECT"))
	require.NoError(t, err)
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer conn.Close()

	requestURL, err := url.Parse("http://agentd" + routeChannelPath)
	require.NoError(t, err)
	req := &http.Request{Method: http.MethodPost, URL: requestURL, Host: "agentd", Header: make(http.Header)}
	req.Header.Set("X-Tclaude-Route-Helper-Credential", os.Getenv("TCLAUDE_ROUTE_HELPER_CREDENTIAL"))
	req.Header.Set("X-Tclaude-Route-Role", "publisher")
	req.Header.Set("X-Tclaude-Route-ID", os.Getenv("TCLAUDE_ROUTE_HELPER_ROUTE"))
	req.Header.Set("X-Tclaude-Route-Agent-ID", os.Getenv("TCLAUDE_ROUTE_HELPER_AGENT"))
	req.Header.Set("X-Tclaude-Route-Conv-ID", os.Getenv("TCLAUDE_ROUTE_HELPER_CONV"))
	req.Header.Set("X-Tclaude-Route-Launch-Generation", os.Getenv("TCLAUDE_ROUTE_HELPER_GENERATION"))
	req.Header.Set("X-Tclaude-Route-Group-Generation", os.Getenv("TCLAUDE_ROUTE_HELPER_GROUP_GENERATION"))
	require.NoError(t, req.Write(conn))
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.NoError(t, err)
	require.Equal(t, expected, response.StatusCode)
	if expected == http.StatusSwitchingProtocols {
		// Keep the hijacked channel alive until the parent has observed that the
		// broker accepted it.
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
	}
}

func TestRouteHelperSiblingAttachesWithCapability(t *testing.T) {
	setupTestDB(t)

	const (
		convID           = "route-helper-sibling-conv"
		launchGeneration = "route-helper-sibling-launch"
	)
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("route-helper-sibling-group", "")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish, PermRoutesConsume}, "test"))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: convID, Role: "worker"}))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	route, err := db.CreateAgentRoute(groupID, agentID, convID, launchGeneration, group.RouteGeneration,
		"sibling", "tcp", "tcp://127.0.0.1:39001")
	require.NoError(t, err)
	credential, _, err := mintRouteHelperCredential(agentID, convID)
	require.NoError(t, err)
	t.Cleanup(func() { revokeRouteHelperCredentials(convID, "") })

	socketPath := filepath.Join("/tmp", "tclaude-rh-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".sock")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	server := &http.Server{
		Handler:           withIdentity(buildMux()),
		ReadHeaderTimeout: time.Second,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if unixConn, ok := conn.(*net.UnixConn); ok {
				return context.WithValue(ctx, unixConnKey{}, unixConn)
			}
			return ctx
		},
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-serverDone
	})

	startSibling := func(token string, expected int) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestRouteHelperSiblingProcess", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"TCLAUDE_ROUTE_HELPER_CHILD=1",
			"TCLAUDE_ROUTE_HELPER_SOCKET="+socketPath,
			"TCLAUDE_ROUTE_HELPER_EXPECT="+strconv.Itoa(expected),
			"TCLAUDE_ROUTE_HELPER_CREDENTIAL="+token,
			"TCLAUDE_ROUTE_HELPER_ROUTE="+route.ID,
			"TCLAUDE_ROUTE_HELPER_AGENT="+agentID,
			"TCLAUDE_ROUTE_HELPER_CONV="+convID,
			"TCLAUDE_ROUTE_HELPER_GENERATION="+launchGeneration,
			"TCLAUDE_ROUTE_HELPER_GROUP_GENERATION="+strconv.FormatInt(group.RouteGeneration, 10),
		)
		return cmd
	}

	sibling := startSibling(credential, http.StatusSwitchingProtocols)
	release, err := sibling.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, sibling.Start())
	require.Eventually(t, func() bool {
		return GroupRouteBroker().Metrics().PublisherChannels > 0
	}, time.Second, 10*time.Millisecond, "production route handler did not attach the sibling channel")
	_, err = release.Write([]byte{'x'})
	require.NoError(t, err)
	require.NoError(t, release.Close())
	require.NoError(t, sibling.Wait())

	wrong := startSibling("not-the-minted-capability", http.StatusUnauthorized)
	require.NoError(t, wrong.Run(), "a sibling with only guessed identity must be refused")

	revokeRouteHelperCredentials(convID, "")
	stale := startSibling(credential, http.StatusUnauthorized)
	require.NoError(t, stale.Run(), "a revoked capability must be refused")
	replayed := startSibling(credential, http.StatusUnauthorized)
	require.NoError(t, replayed.Run(), "a replay of a revoked capability must be refused")
}
