//go:build linux

package agentd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
	"golang.org/x/sys/unix"
)

func TestRouteHelperCredentialHandoffSendsOneShotFD(t *testing.T) {
	setupTestDB(t)
	path, cleanup, err := prepareRouteHelperCredentialHandoff("handoff-secret")
	require.NoError(t, err)
	defer cleanup()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	defer conn.Close()
	raw, err := conn.SyscallConn()
	require.NoError(t, err)
	var credentialFD int
	var recvErr error
	require.NoError(t, raw.Read(func(socketFD uintptr) bool {
		payload := make([]byte, 1)
		oob := make([]byte, unix.CmsgSpace(4))
		_, _, _, _, err := unix.Recvmsg(int(socketFD), payload, oob, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				return false
			}
			recvErr = err
			return true
		}
		messages, err := unix.ParseSocketControlMessage(oob)
		if err != nil || len(messages) != 1 {
			recvErr = fmt.Errorf("parse route helper handoff: %w", err)
			return true
		}
		rights, err := unix.ParseUnixRights(&messages[0])
		if err != nil || len(rights) != 1 {
			recvErr = fmt.Errorf("parse route helper handoff rights: %w", err)
			return true
		}
		credentialFD = rights[0]
		return true
	}))
	require.NoError(t, recvErr)
	require.Positive(t, credentialFD)
	credential := os.NewFile(uintptr(credentialFD), "route-helper-test")
	require.NotNil(t, credential)
	data, err := io.ReadAll(credential)
	require.NoError(t, err)
	require.NoError(t, credential.Close())
	require.Equal(t, "handoff-secret", string(data))
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(path)
		return os.IsNotExist(statErr)
	}, time.Second, 10*time.Millisecond, "handoff socket must be removed after one-shot consumption")
}

func TestRouteHelperCredentialReadRejectsPredecessorGeneration(t *testing.T) {
	setupTestDB(t)
	const convID = "route-helper-generation-current"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	const currentGeneration = "0123456789abcdef0123456789abcdef"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "route-helper-generation-session", ConvID: convID,
		Cwd: "/tmp", Status: "working",
	}))
	require.NoError(t, db.SetSessionExitLaunchGeneration("route-helper-generation-session", currentGeneration))
	old := routeHelperCredential{agentID: "agt_test", convID: convID, launchGeneration: "fedcba9876543210fedcba9876543210"}
	current := old
	current.launchGeneration = currentGeneration
	require.False(t, routeHelperCredentialCurrent(old), "a predecessor capability must fail read-only discovery")
	require.True(t, routeHelperCredentialCurrent(current), "the live launch generation must remain valid")
}

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

	method := os.Getenv("TCLAUDE_ROUTE_HELPER_METHOD")
	if method == "" {
		method = http.MethodPost
	}
	path := routeChannelPath
	if method == http.MethodGet {
		path = "/v1/routes/" + os.Getenv("TCLAUDE_ROUTE_HELPER_ROUTE")
	}
	requestURL, err := url.Parse("http://agentd" + path)
	require.NoError(t, err)
	req := &http.Request{Method: method, URL: requestURL, Host: "agentd", Header: make(http.Header)}
	req.Header.Set("X-Tclaude-Route-Helper-Credential", os.Getenv("TCLAUDE_ROUTE_HELPER_CREDENTIAL"))
	req.Header.Set("X-Tclaude-Route-Role", "publisher")
	req.Header.Set("X-Tclaude-Route-ID", os.Getenv("TCLAUDE_ROUTE_HELPER_ROUTE"))
	req.Header.Set("X-Tclaude-Route-Agent-ID", os.Getenv("TCLAUDE_ROUTE_HELPER_AGENT"))
	req.Header.Set("X-Tclaude-Route-Conv-ID", os.Getenv("TCLAUDE_ROUTE_HELPER_CONV"))
	launchGeneration := os.Getenv("TCLAUDE_ROUTE_HELPER_GENERATION")
	if override := os.Getenv("TCLAUDE_ROUTE_HELPER_GENERATION_OVERRIDE"); override != "" {
		launchGeneration = override
	}
	req.Header.Set("X-Tclaude-Route-Launch-Generation", launchGeneration)
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

	const convID = "route-helper-sibling-conv"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("route-helper-sibling-group", "")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish, PermRoutesConsume}, "test"))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: convID, Role: "worker"}))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	credential, launchGeneration, err := mintRouteHelperCredential(agentID, convID)
	require.NoError(t, err)
	t.Cleanup(func() { revokeRouteHelperCredentials(convID, "") })
	route, err := db.CreateAgentRoute(groupID, agentID, convID, launchGeneration, group.RouteGeneration,
		"sibling", "tcp", "tcp://127.0.0.1:39001")
	require.NoError(t, err)
	require.Equal(t, launchGeneration, route.PublisherLaunchGeneration,
		"route authority must use the capability's minted launch generation")

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

	startSibling := func(token string, generation string, method string, expected int) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestRouteHelperSiblingProcess", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"TCLAUDE_ROUTE_HELPER_CHILD=1",
			"TCLAUDE_ROUTE_HELPER_SOCKET="+socketPath,
			"TCLAUDE_ROUTE_HELPER_EXPECT="+strconv.Itoa(expected),
			"TCLAUDE_ROUTE_HELPER_METHOD="+method,
			"TCLAUDE_ROUTE_HELPER_CREDENTIAL="+token,
			"TCLAUDE_ROUTE_HELPER_ROUTE="+route.ID,
			"TCLAUDE_ROUTE_HELPER_AGENT="+agentID,
			"TCLAUDE_ROUTE_HELPER_CONV="+convID,
			"TCLAUDE_ROUTE_HELPER_GENERATION="+generation,
			"TCLAUDE_ROUTE_HELPER_GROUP_GENERATION="+strconv.FormatInt(group.RouteGeneration, 10),
		)
		return cmd
	}

	sibling := startSibling(credential, launchGeneration, http.MethodPost, http.StatusSwitchingProtocols)
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

	wrong := startSibling("not-the-minted-capability", launchGeneration, http.MethodPost, http.StatusUnauthorized)
	require.NoError(t, wrong.Run(), "a sibling with only guessed identity must be refused")

	mismatchedGeneration := func() *exec.Cmd {
		cmd := startSibling(credential, launchGeneration, http.MethodPost, http.StatusForbidden)
		cmd.Env = append(cmd.Env, "TCLAUDE_ROUTE_HELPER_GENERATION_OVERRIDE=other-launch-generation")
		return cmd
	}
	mismatch := mismatchedGeneration()
	require.NoError(t, mismatch.Run(), "a capability must not authorize a mismatched launch generation")

	newCredential, newGeneration, err := mintRouteHelperCredential(agentID, convID)
	require.NoError(t, err)
	oldDiscovery := startSibling(credential, launchGeneration, http.MethodGet, http.StatusUnauthorized)
	require.NoError(t, oldDiscovery.Run(), "a predecessor capability must be refused after generation rotation")
	newDiscovery := startSibling(newCredential, newGeneration, http.MethodGet, http.StatusOK)
	require.NoError(t, newDiscovery.Run(), "the current generation must retain read-only route discovery")
	currentRoute, err := db.GetAgentRoute(route.ID)
	require.NoError(t, err)
	require.Equal(t, db.RouteStateReady, currentRoute.State,
		"read-only discovery must preserve the existing M1 route authority")
	require.Equal(t, launchGeneration, currentRoute.PublisherLaunchGeneration)

	revokeRouteHelperCredentials(convID, "")
	stale := startSibling(credential, launchGeneration, http.MethodPost, http.StatusUnauthorized)
	require.NoError(t, stale.Run(), "a revoked capability must be refused")
	replayed := startSibling(credential, launchGeneration, http.MethodPost, http.StatusUnauthorized)
	require.NoError(t, replayed.Run(), "a replay of a revoked capability must be refused")
}
