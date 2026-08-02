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
	darwinRouteSmokeRoleEnv        = "TCL951_CHILD_ROLE"
	darwinRouteSmokeReadyEnv       = "TCL951_CHILD_READY"
	darwinRouteSmokeStopEnv        = "TCL951_CHILD_STOP"
	darwinRouteSmokeEndpointEnv    = "TCL951_CHILD_ENDPOINT"
	darwinRouteSmokeHelperRun      = "^TestDarwinRouteSmokeChild$"
	darwinRouteSmokeOpaque         = "tcl951-integrated-opaque"
	darwinRouteSmokeProviderPort   = 45200
	darwinRouteSmokePublisherSlot  = 45201
	darwinRouteSmokeWithdrawalSlot = 45202
	darwinRouteSmokeReusableSlot   = 45203
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
	darwinRouteSmokeWrite(ready, "consumer:endpoint-acquired")
	conn, err := net.DialTimeout("tcp4", endpoint, 5*time.Second)
	if err != nil {
		darwinRouteSmokeWrite(ready, "consumer:dial-failure:"+err.Error())
		t.Fatal(err)
	}
	defer conn.Close()
	darwinRouteSmokeWrite(ready, "consumer:dial-success")
	if _, err := conn.Write([]byte(darwinRouteSmokeOpaque)); err != nil {
		darwinRouteSmokeWrite(ready, "consumer:write-failure:"+err.Error())
		t.Fatal(err)
	}
	darwinRouteSmokeWrite(ready, "consumer:write-success")
	response := make([]byte, len("reply:")+len(darwinRouteSmokeOpaque))
	if _, err := io.ReadFull(conn, response); err != nil {
		darwinRouteSmokeWrite(ready, "consumer:reply-read-failure:"+err.Error())
		t.Fatal(err)
	}
	darwinRouteSmokeWrite(ready, "consumer:reply-read-success")
	if string(response) != "reply:"+darwinRouteSmokeOpaque {
		darwinRouteSmokeWrite(ready, "consumer:reply-unexpected")
		t.Fatal("unexpected route response")
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
	snapshot := sandboxpolicy.EmptySnapshot()
	// Route-capable Seatbelt launches use the production filtered native
	// loopback floor with a concrete provider route. The route slots are exact
	// additional carveouts in that floor, while all other direct
	// binds/neighbors remain denied.
	snapshot.Effective.Network = &sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Loopback: true, Ports: []int{darwinRouteSmokeProviderPort},
		}},
	}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name:  "ANTHROPIC_BASE_URL",
		Value: "http://localhost:" + strconv.Itoa(darwinRouteSmokeProviderPort) + "/v1",
	}}
	snapshotPath, snapshotDigest, err := sandboxpolicy.WriteSnapshotFile(control, snapshot)
	require.NoError(t, err)

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
		SandboxImpl:         string(sandboxpolicy.ImplementationTclaudeLayer),
		SandboxSnapshotPath: snapshotPath, SandboxSnapshotDigest: snapshotDigest,
		DarwinRouteCapable: true, DarwinRouteAgentID: agentID,
		SessionID: convID, Model: "sonnet", Dir: cwd, Detached: true,
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
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		current, err := os.ReadFile(path)
		if err == nil {
			raw = current
			marker := strings.TrimSpace(string(current))
			if contains == "" || strings.Contains(marker, contains) {
				return string(raw)
			}
			if darwinRouteSmokeTerminalMarker(marker) {
				require.Failf(t, "route smoke child reached terminal stage", "marker %q in %s while waiting for %q", marker, path, contains)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Failf(t, "route smoke file condition never satisfied", "waiting for %s to contain %q; last marker %q", path, contains, strings.TrimSpace(string(raw)))
	return string(raw)
}

func darwinRouteSmokeTerminalMarker(marker string) bool {
	for _, prefix := range []string{
		"consumer:endpoint-timeout",
		"consumer:dial-failure:",
		"consumer:write-failure:",
		"consumer:reply-read-failure:",
		"consumer:reply-unexpected",
	} {
		if strings.HasPrefix(marker, prefix) {
			return true
		}
	}
	return false
}

func waitDarwinRouteSmokePublisherChannels(t *testing.T, want uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return GroupRouteBroker().Metrics().PublisherChannels == want
	}, time.Second, time.Millisecond, "waiting for %d attached route publisher channels", want)
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

func assertDarwinRouteSmokeSlotReleased(t *testing.T, slot int) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	var claims int
	require.NoError(t, d.QueryRow(
		"SELECT COUNT(*) FROM darwin_route_slot_claims WHERE slot = ?", slot,
	).Scan(&claims))
	require.Zero(t, claims, "reaped consumer slot must have no durable claim")
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(slot)))
	require.NoError(t, err, "reaped consumer listener must release its exact slot")
	require.NoError(t, listener.Close())
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
	// One slot per launch keeps the exact claim/listener we observe below
	// unambiguous while preserving the production allocator and RunNew path.
	t.Setenv("TCLAUDE_DARWIN_ROUTE_SLOT_COUNT", "1")
	work := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(work, 0o700))
	db.ResetForTest()
	cleanupAgentdTestDB(t)
	routeAdapterCloseAll()
	t.Cleanup(routeAdapterCloseAll)

	// The production allocator is kernel-ephemeral. This dedicated evidence
	// cell uses the existing reservation seam to make A -> B reuse exact and
	// deterministic: B is offered A's port only after A's listener is reaped.
	slotQueue := [][]int{
		{darwinRouteSmokePublisherSlot},
		{darwinRouteSmokeWithdrawalSlot},
		{darwinRouteSmokeReusableSlot},
		{darwinRouteSmokeReusableSlot},
	}
	restoreSlotAllocator := session.SetDarwinRouteSlotAllocatorForTest(func() (*session.DarwinRouteSlotReservation, error) {
		if len(slotQueue) == 0 {
			return nil, fmt.Errorf("integrated route smoke exhausted deterministic slot queue")
		}
		reservation, reserveErr := session.ReserveDarwinRouteSlotsAt(slotQueue[0])
		if reserveErr != nil {
			return nil, reserveErr
		}
		slotQueue = slotQueue[1:]
		return reservation, nil
	})
	t.Cleanup(restoreSlotAllocator)

	helper := filepath.Join(work, "tcl951-route-helper")
	data, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(helper, data, 0o700))

	const groupName = "tcl951-integrated-group"
	const publisherConv = "77000000-0000-4000-8000-000000000951"
	const withdrawalConv = "77000000-0000-4000-8000-000000000952"
	const consumerAConv = "77000000-0000-4000-8000-000000000953"
	const consumerBConv = "77000000-0000-4000-8000-000000000954"
	publisherAgent, _, err := db.EnsureAgentForConv(publisherConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	withdrawalAgent, _, err := db.EnsureAgentForConv(withdrawalConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	consumerAAgent, _, err := db.EnsureAgentForConv(consumerAConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	consumerBAgent, _, err := db.EnsureAgentForConv(consumerBConv, "TCL-951 integrated smoke")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup(groupName, "TCL-951 integrated Seatbelt route evidence")
	require.NoError(t, err)
	for _, conv := range []string{publisherConv, withdrawalConv, consumerAConv, consumerBConv} {
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
	withdrawal := startDarwinRouteSmokeLaunch(t, home, helper, "consumer", withdrawalConv, withdrawalAgent)
	reaper.Tick()

	handler := BuildHandlerForTest()
	target := "tcp://127.0.0.1:" + strconv.Itoa(publisher.launch.Slots[0])
	rec, route := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": groupName, "name": "integrated", "target": target, "launch_generation": publisher.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	waitDarwinRouteSmokePublisherChannels(t, 1)
	routeID := route["id"].(string)

	// A consumer that selects a group it does not belong to is refused by the
	// production M1 API before the Darwin adapter can allocate a listener.
	rec, _ = serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", withdrawalConv, map[string]any{
		"route_id": routeID, "group": "tcl951-wrong-group", "launch_generation": withdrawal.gen,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec, leaseWithdrawal := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", withdrawalConv, map[string]any{
		"route_id": routeID, "group": groupName, "launch_generation": withdrawal.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	endpointWithdrawal := leaseWithdrawal["endpoint"].(string)
	require.Contains(t, withdrawal.launch.Slots, darwinRouteSmokePort(t, endpointWithdrawal), "adapter listener must use the consumer's exact launch pool")
	require.NoError(t, os.WriteFile(withdrawal.endpoint, []byte(endpointWithdrawal), 0o600))
	waitDarwinRouteSmokeFile(t, withdrawal.ready, "consumer:opaque-exchange-ready")

	rec, _ = serveDarwinRouteSmoke(t, handler, http.MethodDelete, "/v1/routes/"+routeID, publisherConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	waitDarwinRouteSmokeFile(t, withdrawal.ready, "consumer:endpoint-closed")
	require.Eventually(t, func() bool { return len(routeAdapterLeaseIDs()) == 0 }, 5*time.Second, 50*time.Millisecond)
	waitDarwinRouteSmokePublisherChannels(t, 0)
	stopDarwinRouteSmokeLaunch(t, withdrawal, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, withdrawal)

	// Consumer A is intentionally killed while its route remains published.
	// This is the idle SessionEnd/reaper path whose exact claim and listener
	// must disappear without disturbing the publisher's unrelated claim.
	consumerA := startDarwinRouteSmokeLaunch(t, home, helper, "consumer", consumerAConv, consumerAAgent)
	reaper.Tick()
	rec, routeIdle := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": groupName, "name": "idle-consumer", "target": target, "launch_generation": publisher.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	waitDarwinRouteSmokePublisherChannels(t, 1)
	idleRouteID := routeIdle["id"].(string)
	rec, leaseA := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", consumerAConv, map[string]any{
		"route_id": idleRouteID, "group": groupName, "launch_generation": consumerA.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	endpointA := leaseA["endpoint"].(string)
	consumerASlot := darwinRouteSmokePort(t, endpointA)
	require.Contains(t, consumerA.launch.Slots, consumerASlot, "adapter listener must use consumer A's exact launch pool")
	require.NoError(t, os.WriteFile(consumerA.endpoint, []byte(endpointA), 0o600))
	waitDarwinRouteSmokeFile(t, consumerA.ready, "consumer:opaque-exchange-ready")
	_, err = session.ReserveDarwinRouteSlotsAt([]int{consumerASlot})
	require.Error(t, err, "allocator must not reclaim consumer A's exact slot while its endpoint is live")
	// Kill the now-idle consumer while the route remains published. Its real
	// SessionEnd/reaper transaction must close the exact lease/listener and
	// release the launch claim without withdrawing the publisher route.
	stopDarwinRouteSmokeLaunch(t, consumerA, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, consumerA)
	assertDarwinRouteSmokeSlotReleased(t, consumerASlot)
	leaseARow, err := db.GetAgentRouteLease(leaseA["id"].(string))
	require.NoError(t, err)
	require.Equal(t, db.RouteLeaseClosed, leaseARow.State)
	idleRouteRow, err := db.GetAgentRoute(idleRouteID)
	require.NoError(t, err)
	require.Equal(t, db.RouteStateReady, idleRouteRow.State)
	require.Eventually(t, func() bool { return len(routeAdapterLeaseIDs()) == 0 }, 5*time.Second, 50*time.Millisecond)
	publisherStillActive, err := db.GetDarwinRouteLaunch(publisher.agentID, publisher.convID, publisher.gen)
	require.NoError(t, err)
	require.Equal(t, db.DarwinRouteLaunchActive, publisherStillActive.State, "idle consumer cleanup must retain unrelated publisher claim")

	// Consumer B is launched only after A's exact slot is released. Its real
	// adapter endpoint must use B's exact newly claimed pool and transfer bytes
	// before the publisher-death assertion below.
	consumerB := startDarwinRouteSmokeLaunch(t, home, helper, "consumer", consumerBConv, consumerBAgent)
	reaper.Tick()
	rec, routeDeath := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": groupName, "name": "publisher-death", "target": target, "launch_generation": publisher.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	waitDarwinRouteSmokePublisherChannels(t, 2)
	deathRouteID := routeDeath["id"].(string)
	rec, leaseB := serveDarwinRouteSmoke(t, handler, http.MethodPost, "/v1/routes/open", consumerBConv, map[string]any{
		"route_id": deathRouteID, "group": groupName, "launch_generation": consumerB.gen,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	endpointB := leaseB["endpoint"].(string)
	require.Equal(t, consumerASlot, consumerB.launch.Slots[0], "consumer B must reclaim consumer A's exact released slot")
	require.Equal(t, consumerASlot, darwinRouteSmokePort(t, endpointB), "consumer B adapter listener must use the reclaimed exact slot")
	require.NoError(t, os.WriteFile(consumerB.endpoint, []byte(endpointB), 0o600))
	waitDarwinRouteSmokeFile(t, consumerB.ready, "consumer:opaque-exchange-ready")
	t.Logf("TCL-951 exact slot release/reuse: consumerA=%d released; consumerB=%d exact listener active", consumerASlot, darwinRouteSmokePort(t, endpointB))

	// The endpoint is still live here: only now is the publisher session
	// stopped/reaped, so closure is attributable to publisher death rather than
	// an earlier consumer withdrawal or idle-consumer cleanup.
	stopDarwinRouteSmokeLaunch(t, publisher, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, publisher)
	assertDarwinRouteSmokeSlotReleased(t, publisher.launch.Slots[0])
	waitDarwinRouteSmokeFile(t, consumerB.ready, "consumer:endpoint-closed")
	rec, routeView := serveDarwinRouteSmoke(t, handler, http.MethodGet, "/v1/routes/"+deathRouteID, consumerBConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, db.RouteStatePublisherLost, routeView["state"])
	deathLeaseRow, err := db.GetAgentRouteLease(leaseB["id"].(string))
	require.NoError(t, err)
	require.Equal(t, db.RouteLeaseClosed, deathLeaseRow.State)
	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("tcp4", endpointB, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return false
		}
		return true
	}, 5*time.Second, 50*time.Millisecond, "publisher death must close/refuse the live consumer endpoint")
	stopDarwinRouteSmokeLaunch(t, consumerB, reaper)
	assertDarwinRouteSmokeLaunchClosed(t, consumerB)

	require.Eventually(t, func() bool {
		darwinRouteAdapterState.Lock()
		adapter := darwinRouteAdapterState.adapter
		darwinRouteAdapterState.Unlock()
		return adapter == nil || (len(adapter.RouteIDs()) == 0 && len(adapter.LeaseIDs()) == 0)
	}, 5*time.Second, 50*time.Millisecond)
	t.Log("TCL-951 integrated route evidence: POSITIVE runNew/Seatbelt/M1/M2 opaque exchange")
	t.Log("TCL-951 disclosure: Partial; Darwin localhost authorization is exact-slot, while same-port local reachability remains the documented Seatbelt limitation")
}
