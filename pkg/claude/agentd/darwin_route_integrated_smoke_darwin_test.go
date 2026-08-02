//go:build darwin

package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	darwinRouteSmokeRoleEnv     = "TCL951_CHILD_ROLE"
	darwinRouteSmokeReadyEnv    = "TCL951_CHILD_READY"
	darwinRouteSmokeStopEnv     = "TCL951_CHILD_STOP"
	darwinRouteSmokeEndpointEnv = "TCL951_CHILD_ENDPOINT"
	darwinRouteSmokeHelperRun   = "^TestDarwinRouteSmokeChild$"
	darwinRouteSmokeOpaque      = "tcl951-integrated-opaque"
)

// routeSmokeSpawner is the only fake in this cell. It replaces the external
// Claude executable while leaving tmux, tclaude-layer, slot reservation,
// launch-contract activation, and supervision on their production paths.
type routeSmokeSpawner struct {
	role     string
	ready    string
	stop     string
	endpoint string
	helper   string
}

func (s routeSmokeSpawner) Binary() string { return "tcl951-route-helper" }

func (s routeSmokeSpawner) BuildCommand(_ harness.SpawnSpec) string {
	assign := []string{
		darwinRouteSmokeRoleEnv + "=" + clcommon.ShellQuoteArg(s.role),
		darwinRouteSmokeReadyEnv + "=" + clcommon.ShellQuoteArg(s.ready),
		darwinRouteSmokeStopEnv + "=" + clcommon.ShellQuoteArg(s.stop),
		darwinRouteSmokeEndpointEnv + "=" + clcommon.ShellQuoteArg(s.endpoint),
	}
	return strings.Join(assign, " ") + " exec " + clcommon.ShellQuoteArg(s.helper) +
		" -test.run " + clcommon.ShellQuoteArg(darwinRouteSmokeHelperRun)
}

// TestDarwinRouteSmokeChild is selected only by routeSmokeSpawner. The normal
// package run skips it, while the dedicated workflow runs it in the actual
// tclaude-layer Seatbelt child.
func TestDarwinRouteSmokeChild(t *testing.T) {
	role := os.Getenv(darwinRouteSmokeRoleEnv)
	if role == "" {
		t.Skip("route smoke helper subprocess")
	}
	ready := os.Getenv(darwinRouteSmokeReadyEnv)
	stop := os.Getenv(darwinRouteSmokeStopEnv)
	endpointFile := os.Getenv(darwinRouteSmokeEndpointEnv)
	switch role {
	case "publisher":
		darwinRouteSmokePublisher(t, ready, stop)
	case "consumer":
		darwinRouteSmokeConsumer(t, ready, stop, endpointFile)
	default:
		t.Fatalf("unknown route smoke role %q", role)
	}
}

func darwinRouteSmokePublisher(t *testing.T, ready, stop string) {
	slots, err := session.ParseDarwinRouteSlots(os.Getenv(session.DarwinRouteSlotsEnv))
	if err != nil {
		darwinRouteSmokeWrite(ready, "publisher:slot-parse-error:"+err.Error())
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(slots[0])))
	if err != nil {
		darwinRouteSmokeWrite(ready, "publisher:target-bind-error:"+err.Error())
		t.Fatal(err)
	}
	defer listener.Close()
	neighbor := slots[len(slots)-1] + 1
	neighborListener, neighborErr := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(neighbor)))
	if neighborErr == nil {
		neighborListener.Close()
		darwinRouteSmokeWrite(ready, fmt.Sprintf("publisher:neighbor-allowed:%d", neighbor))
		t.Fatalf("unreserved neighboring port %d was bindable inside Seatbelt", neighbor)
	}
	darwinRouteSmokeWrite(ready, fmt.Sprintf("publisher:seatbelt-ready:neighbor-denied:%d:pid=%d", neighbor, os.Getpid()))

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				payload := make([]byte, len(darwinRouteSmokeOpaque))
				if _, readErr := io.ReadFull(conn, payload); readErr == nil && string(payload) == darwinRouteSmokeOpaque {
					_, _ = conn.Write([]byte("reply:" + darwinRouteSmokeOpaque))
				}
			}()
		}
	}()
	for !darwinRouteSmokeExists(stop) {
		time.Sleep(20 * time.Millisecond)
	}
}

func darwinRouteSmokeConsumer(t *testing.T, ready, stop, endpointFile string) {
	var endpoint string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(endpointFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			endpoint = strings.TrimSpace(string(raw))
			break
		}
		if darwinRouteSmokeExists(stop) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if endpoint == "" {
		darwinRouteSmokeWrite(ready, "consumer:endpoint-timeout")
		t.Fatal("consumer endpoint was not published")
	}
	conn, err := net.DialTimeout("tcp4", endpoint, 5*time.Second)
	if err != nil {
		darwinRouteSmokeWrite(ready, "consumer:dial-error:"+err.Error())
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(darwinRouteSmokeOpaque)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("reply:")+len(darwinRouteSmokeOpaque))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "reply:"+darwinRouteSmokeOpaque {
		t.Fatalf("unexpected route response %q", response)
	}
	darwinRouteSmokeWrite(ready, "consumer:opaque-exchange-ready:pid="+strconv.Itoa(os.Getpid()))
	for {
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		var one [1]byte
		_, readErr := conn.Read(one[:])
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, net.ErrClosed) || errors.Is(readErr, io.EOF) || errors.Is(readErr, syscall.ECONNRESET) {
			darwinRouteSmokeWrite(ready, "consumer:endpoint-closed")
			return
		}
		if errors.Is(readErr, os.ErrDeadlineExceeded) || errors.Is(readErr, syscall.EAGAIN) {
			if darwinRouteSmokeExists(stop) {
				return
			}
			continue
		}
		t.Fatal(readErr)
	}
}

func darwinRouteSmokeWrite(path, value string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(value), 0o600)
}

func darwinRouteSmokeExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type darwinRouteSmokeLaunch struct {
	agentID  string
	convID   string
	gen      string
	launch   *db.DarwinRouteLaunch
	rowID    string
	tmux     string
	ready    string
	stop     string
	endpoint string
}

func startDarwinRouteSmokeLaunch(t *testing.T, home, helper, role, convID, agentID string) darwinRouteSmokeLaunch {
	t.Helper()
	control := filepath.Join(home, "work", "route-control", convID)
	require.NoError(t, os.MkdirAll(control, 0o700))
	ready := filepath.Join(control, "ready")
	stop := filepath.Join(control, "stop")
	endpoint := filepath.Join(control, "endpoint")

	original := harness.MustGet(harness.DefaultName)
	replacement := *original
	replacement.Spawn = routeSmokeSpawner{role: role, ready: ready, stop: stop, endpoint: endpoint, helper: helper}
	harness.Register(&replacement)
	t.Cleanup(func() { harness.Register(original) })
	previousAncestorCheck := session.ClaudeAncestorCheck
	session.ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { session.ClaudeAncestorCheck = previousAncestorCheck })

	cwd := filepath.Join(home, "work")
	params := &session.NewParams{
		ManagedLaunch: true, Harness: harness.DefaultName,
		SandboxImpl:        string(sandboxpolicy.ImplementationTclaudeLayer),
		DarwinRouteCapable: true, DarwinRouteAgentID: agentID,
		SessionID: convID, Dir: cwd, Detached: true,
	}
	// A launch error is evidence failure: it must never be logged and ignored.
	require.NoError(t, session.RunNew(params), "route-capable %s launch must complete production runNew", role)
	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "runNew must persist a session row")
	var out darwinRouteSmokeLaunch
	for _, row := range rows {
		identity, identityErr := db.GetSessionExitLaunchIdentity(row.ID)
		if identityErr != nil || identity.Generation == "" {
			continue
		}
		launch, launchErr := db.GetDarwinRouteLaunch(agentID, convID, identity.Generation)
		if launchErr == nil && launch.State == db.DarwinRouteLaunchActive {
			out = darwinRouteSmokeLaunch{agentID: agentID, convID: convID, gen: identity.Generation, launch: launch, rowID: row.ID, tmux: row.TmuxSession, ready: ready, stop: stop, endpoint: endpoint}
			break
		}
	}
	require.NotNil(t, out.launch, "runNew must activate the exact Darwin route launch contract")
	t.Cleanup(func() {
		_ = os.WriteFile(out.stop, []byte("cleanup"), 0o600)
		if out.tmux != "" {
			_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(out.tmux)).Run()
		}
	})
	return out
}

func waitDarwinRouteSmokeFile(t *testing.T, path string, contains string) string {
	t.Helper()
	var raw []byte
	require.Eventually(t, func() bool {
		var err error
		raw, err = os.ReadFile(path)
		return err == nil && (contains == "" || strings.Contains(string(raw), contains))
	}, 30*time.Second, 50*time.Millisecond, "waiting for %s to contain %q", path, contains)
	return string(raw)
}

func stopDarwinRouteSmokeLaunch(t *testing.T, launch darwinRouteSmokeLaunch, reaper *SessionReaperHandle) {
	t.Helper()
	reaper.Tick()
	require.NoError(t, os.WriteFile(launch.stop, []byte("stop"), 0o600))
	// The helper exits naturally, making the tmux session genuinely dead. A
	// first tick observes the alive row; the second tick performs the reaping.
	require.Eventually(t, func() bool {
		reaper.Tick()
		state, err := session.LoadSessionState(launch.rowID)
		return err == nil && state.Status == session.StatusExited
	}, 15*time.Second, 100*time.Millisecond, "waiting for tmux session %s to exit", launch.tmux)
	reaper.Tick()
}

func assertDarwinRouteSmokeLaunchClosed(t *testing.T, launch darwinRouteSmokeLaunch) {
	t.Helper()
	closed, err := db.GetDarwinRouteLaunch(launch.agentID, launch.convID, launch.gen)
	require.NoError(t, err)
	require.Equal(t, db.DarwinRouteLaunchClosed, closed.State, "reaper must close the exact launch claim")
	sessions, err := clcommon.Default.ListSessions()
	require.NoError(t, err)
	require.NotContains(t, sessions, launch.tmux, "exited route helper must not leave a tmux supervisor session")
}

func darwinRouteSmokePort(t *testing.T, endpoint string) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(endpoint)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return port
}

func serveDarwinRouteSmoke(t *testing.T, handler http.Handler, method, path, convID string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := testharness.JSONRequest(t, method, path, body)
	req = AsAgentPeer(req, convID)
	rec := testharness.Serve(handler, req)
	var out map[string]any
	if rec.Body.Len() != 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec, out
}

func TestDarwinRouteCapabilityIntegratedSmoke(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("TCLAUDE_DARWIN_ROUTE_CAPABILITY_SMOKE") != "1" {
		t.Skip("set TCLAUDE_DARWIN_ROUTE_CAPABILITY_SMOKE=1 on the dedicated macOS evidence workflow")
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	actualHead := strings.TrimSpace(string(head))
	require.Len(t, actualHead, 40)
	if expectedHead := strings.TrimSpace(os.Getenv("EXPECTED_HEAD")); expectedHead != "" {
		require.Equal(t, expectedHead, actualHead, "dedicated evidence must run the requested exact checkout")
	}
	t.Logf("TCL-951 integrated exact checked-out head: %s", actualHead)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TMUX", "")
	work := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(work, 0o700))
	db.ResetForTest()
	cleanupAgentdTestDB(t)
	routeAdapterCloseAll()
	t.Cleanup(routeAdapterCloseAll)

	helper := filepath.Join(work, "tcl951-route-helper")
	data, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(helper, data, 0o700))

	const groupName = "tcl951-integrated-group"
	const publisherConv = "77000000-0000-4000-8000-000000000951"
	const consumerAConv = "77000000-0000-4000-8000-000000000952"
	const consumerBConv = "77000000-0000-4000-8000-000000000953"
	publisherAgent, _, err := db.EnsureAgentForConv(publisherConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	consumerAAgent, _, err := db.EnsureAgentForConv(consumerAConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	consumerBAgent, _, err := db.EnsureAgentForConv(consumerBConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup(groupName, "TCL-951 integrated Seatbelt route evidence")
	require.NoError(t, err)
	for _, conv := range []string{publisherConv, consumerAConv, consumerBConv} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: conv}))
	}
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{PermRoutesPublish, PermRoutesConsume}, "TCL-951"))
	wrongGroupID, err := db.CreateAgentGroup("tcl951-wrong-group", "")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(wrongGroupID, []string{PermRoutesConsume}, "TCL-951"))

	reaper := NewSessionReaperForTest(0, func(string, string) {})
	publisher := startDarwinRouteSmokeLaunch(t, home, helper, "publisher", publisherConv, publisherAgent)
	reaper.Tick()
	pubReady := waitDarwinRouteSmokeFile(t, publisher.ready, "publisher:seatbelt-ready:neighbor-denied")
	t.Logf("TCL-951 integrated publisher readiness: %s", pubReady)
	consumerA := startDarwinRouteSmokeLaunch(t, home, helper, "consumer", consumerAConv, consumerAAgent)
	reaper.Tick()

	handler := BuildHandlerForTest()
	target := "tcp://127.0.0.1:" + strconv.Itoa(publisher.launch.Slots[0])
	rec, route := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": groupName, "name": "integrated", "target": target, "launch_generation": publisher.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)

	// A consumer that selects a group it does not belong to is refused by the
	// production M1 API before the Darwin adapter can allocate a listener.
	rec, _ = serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", consumerAConv, map[string]any{
		"route_id": routeID, "group": "tcl951-wrong-group", "launch_generation": consumerA.gen,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec, leaseA := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", consumerAConv, map[string]any{
		"route_id": routeID, "group": groupName, "launch_generation": consumerA.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	endpointA := leaseA["endpoint"].(string)
	require.Contains(t, consumerA.launch.Slots, darwinRouteSmokePort(t, endpointA), "adapter listener must use the consumer's exact launch pool")
	require.NoError(t, os.WriteFile(consumerA.endpoint, []byte(endpointA), 0o600))
	waitDarwinRouteSmokeFile(t, consumerA.ready, "consumer:opaque-exchange-ready")

	rec, _ = serveDarwinRouteSmoke(t, handler, http.MethodDelete, "/v1/routes/"+routeID, publisherConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	waitDarwinRouteSmokeFile(t, consumerA.ready, "consumer:endpoint-closed")
	require.Eventually(t, func() bool { return len(routeAdapterLeaseIDs()) == 0 }, 5*time.Second, 50*time.Millisecond)
	stopDarwinRouteSmokeLaunch(t, consumerA, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, consumerA)

	// The consumer's idle SessionEnd/reaper path released its exact claim.
	consumerB := startDarwinRouteSmokeLaunch(t, home, helper, "consumer", consumerBConv, consumerBAgent)
	reaper.Tick()

	// Re-publish on the still-live publisher and leave this route to the
	// publisher death path. The consumer must observe closure after reaping.
	rec, routeB := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": groupName, "name": "publisher-death", "target": target, "launch_generation": publisher.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeBID := routeB["id"].(string)
	rec, leaseB := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", consumerBConv, map[string]any{
		"route_id": routeBID, "group": groupName, "launch_generation": consumerB.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	endpointB := leaseB["endpoint"].(string)
	require.Contains(t, consumerB.launch.Slots, darwinRouteSmokePort(t, endpointB), "reused adapter listener must use the new consumer's exact launch pool")
	require.NoError(t, os.WriteFile(consumerB.endpoint, []byte(endpointB), 0o600))
	waitDarwinRouteSmokeFile(t, consumerB.ready, "consumer:opaque-exchange-ready")
	// Kill the now-idle consumer while the route remains published. Its real
	// SessionEnd/reaper transaction must close the exact lease/listener and
	// release the launch claim without withdrawing the publisher route.
	stopDarwinRouteSmokeLaunch(t, consumerB, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, consumerB)
	leaseBRow, err := db.GetAgentRouteLease(leaseB["id"].(string))
	require.NoError(t, err)
	require.Equal(t, db.RouteLeaseClosed, leaseBRow.State)
	routeBRow, err := db.GetAgentRoute(routeBID)
	require.NoError(t, err)
	require.Equal(t, db.RouteStateReady, routeBRow.State)
	require.Eventually(t, func() bool { return len(routeAdapterLeaseIDs()) == 0 }, 5*time.Second, 50*time.Millisecond)

	stopDarwinRouteSmokeLaunch(t, publisher, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, publisher)
	rec, routeView := serveDarwinRouteSmoke(t, handler, http.MethodGet, "/v1/routes/"+routeBID, consumerBConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, db.RouteStatePublisherLost, routeView["state"])

	require.Eventually(t, func() bool {
		darwinRouteAdapterState.Lock()
		adapter := darwinRouteAdapterState.adapter
		darwinRouteAdapterState.Unlock()
		return adapter == nil || (len(adapter.RouteIDs()) == 0 && len(adapter.LeaseIDs()) == 0)
	}, 5*time.Second, 50*time.Millisecond)
	t.Log("TCL-951 integrated route evidence: POSITIVE runNew/Seatbelt/M1/M2 opaque exchange")
	t.Log("TCL-951 disclosure: Partial; Darwin localhost authorization is exact-slot, while same-port local reachability remains the documented Seatbelt limitation")
}
