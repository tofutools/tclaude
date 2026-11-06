package session

import (
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Golden pins for the launch-spec dispatchers (WrapTclaudeLayerSpec* and
// WrapTclaudeLayerServerSpec*): what reaches the platform renderers from a
// spec's contract — a group-route helper, a Darwin route-slot pool with or
// without its pre-reservation, a loopback port — and the refusals on the way,
// spec building's included. Regenerate as the renderer pins are.

// freeLoopbackPort is a port nothing listens on, for a route slot the
// renderer or a pre-reservation takes.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func dispatchPinRouteHelper(proxyOnly bool) *TclaudeLayerRouteHelper {
	// The helper's binary must exist; the test binary stands in ($EXE).
	exe, _ := os.Executable()
	return &TclaudeLayerRouteHelper{
		BinaryPath:        exe,
		SocketPath:        "/home/test/.tclaude/api/agentd.sock",
		AgentID:           "agt_test",
		ConvID:            "conv_test",
		LaunchGeneration:  "launch_test",
		HandoffSocketPath: "/home/test/.tclaude/data/route-helper-handoff.sock",
		// A server's caller supplies the credential descriptor; a pane
		// launch's renderer computes its own.
		CredentialFD: 4,
		GroupIDs:     []int64{42},
		ProxyOnly:    proxyOnly,
	}
}

func TestLayerDispatchPins(t *testing.T) {
	env := newLayerPinEnv(t)
	slot := freeLoopbackPort(t)
	normalise := func(s string) string {
		return strings.ReplaceAll(env.normalise(s), strconv.Itoa(slot), "$SLOT")
	}
	binary := "/usr/bin/bwrap"
	if runtime.GOOS == "darwin" {
		binary = "/usr/bin/sandbox-exec"
	}
	pins := map[string]string{}
	var order []string
	pin := func(key string, out string, err error) {
		order = append(order, key)
		pins[key] = layerPinRender(normalise, out, err)
	}
	type launch struct {
		name   string
		mutate func(*testing.T, *TclaudeLayerLaunchInput)
		server bool
		port   int
	}
	slots := func(reserve bool, helper *TclaudeLayerRouteHelper) func(*testing.T, *TclaudeLayerLaunchInput) {
		return func(t *testing.T, in *TclaudeLayerLaunchInput) {
			in.DarwinRouteSlots, in.RouteHelper = []int{slot}, helper
			if reserve {
				reservation, err := ReserveDarwinRouteSlotsAt([]int{slot})
				require.NoError(t, err)
				t.Cleanup(func() { _ = reservation.Release() })
				in.DarwinRouteReservation = reservation
			}
		}
	}
	helper := func(proxyOnly bool) func(*testing.T, *TclaudeLayerLaunchInput) {
		return func(_ *testing.T, in *TclaudeLayerLaunchInput) { in.RouteHelper = dispatchPinRouteHelper(proxyOnly) }
	}
	launches := []launch{
		{name: "pane"},
		{name: "pane+loopback", port: 4567},
		{name: "pane+loopback-invalid", port: 70000},
		{name: "pane+route-helper", mutate: helper(false)},
		{name: "pane+route-helper-proxy-only", mutate: helper(true)},
		{name: "pane+route-slots", mutate: slots(false, dispatchPinRouteHelper(true))},
		{name: "pane+route-slots-without-authority", mutate: slots(false, nil)},
		{name: "pane+route-slots+reservation", mutate: slots(true, dispatchPinRouteHelper(true))},
		{name: "pane+route-slots+reservation+loopback", mutate: slots(true, dispatchPinRouteHelper(true)), port: 4567},
		{name: "server", server: true},
		{name: "server+loopback", server: true, port: 4567},
		{name: "server+route-helper", server: true, mutate: helper(false)},
	}
	for _, plan := range []struct {
		name     string
		snapshot func() *sandboxpolicy.Snapshot
	}{
		{"host-open", func() *sandboxpolicy.Snapshot { return nil }},
		{"packet", func() *sandboxpolicy.Snapshot { return engineLaunchSnapshot(sandboxpolicy.NetworkEnginePacket) }},
		{"proxy", func() *sandboxpolicy.Snapshot { return engineLaunchSnapshot(sandboxpolicy.NetworkEngineProxy) }},
	} {
		for _, l := range launches {
			key := plan.name + "/" + l.name
			var reservation *DarwinRouteSlotReservation
			spec, err := env.spec(t, plan.name, plan.snapshot(), func(in *TclaudeLayerLaunchInput) {
				if l.mutate != nil {
					l.mutate(t, in)
				}
				reservation = in.DarwinRouteReservation
			})
			// The next launch takes the same slot: whatever this one's render
			// left reserved goes once it is pinned.
			key, release := key, func() { _ = reservation.Release() }
			if err != nil {
				pin(key, "", err)
				release()
				continue
			}
			if l.server {
				out, err := WrapTclaudeLayerServerSpecWithLoopbackBind(binary, spec, l.port, "exec opencode serve")
				pin(key, out, err)
				release()
				continue
			}
			out, err := WrapTclaudeLayerSpecWithLoopbackBind(binary, spec, l.port, "exec agent --flag")
			pin(key, out, err)
			release()
		}
	}
	checkLayerPins(t, "layer_dispatch_pins", pins, order)
}
