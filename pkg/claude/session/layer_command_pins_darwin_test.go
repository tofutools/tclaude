//go:build darwin

package session

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// freeLoopbackPort is a port nothing listens on, for a route slot the
// renderer reserves itself.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func TestLayerCommandPins(t *testing.T) {
	inputs, normaliseInputs := layerPinFixture(t)
	// Seatbelt has no packet engine: an isolated plan stands in for it.
	isolated := inputs[0]
	isolated.name = "isolated"
	isolated.in.Plan.NetworkPosture = sandboxpolicy.NetworkIsolatedWithAgentd
	inputs = append(inputs, isolated)
	slot := freeLoopbackPort(t)
	normalise := func(s string) string {
		return strings.ReplaceAll(normaliseInputs(s), strconv.Itoa(slot), "$SLOT")
	}
	binary := darwinSeatbeltExecutable
	helper := TclaudeLayerRouteHelper{
		SocketPath: "/home/test/.tclaude/api/agentd.sock", AgentID: "agt_test", ConvID: "conv_test",
		LaunchGeneration: "launch_test", GroupIDs: []int64{42}, ProxyOnly: true,
	}
	pins := map[string]string{}
	var order []string
	pin := func(key string, render func() (string, error)) {
		out, err := render()
		order = append(order, key)
		pins[key] = layerPinRender(normalise, out, err)
	}
	for _, input := range inputs {
		p, in := input.name+"/", input.in
		in.Binary = binary
		command := func(opts tclaudeLayerLaunchOpts) func() (string, error) {
			return func() (string, error) { return tclaudeLayerCommand(in, opts, "exec agent --flag") }
		}
		pin(p+"command", command(tclaudeLayerLaunchOpts{}))
		pin(p+"command+loopback", command(tclaudeLayerLaunchOpts{LoopbackBindPort: 4567}))
		pin(p+"command+route-slots", command(tclaudeLayerLaunchOpts{RouteSlots: []int{slot}, RouteHelper: &helper}))
		pin(p+"command+route-slots-invalid", command(tclaudeLayerLaunchOpts{RouteSlots: []int{0}}))
		pin(p+"command+route-slots+loopback", command(tclaudeLayerLaunchOpts{
			RouteSlots: []int{slot}, RouteHelper: &helper, LoopbackBindPort: 4567}))
		pin(p+"command+route-helper", command(tclaudeLayerLaunchOpts{RouteHelper: &helper}))
		// Production renders a server only through the loopback variant: the
		// plain one's inherited-descriptor form had no caller.
		pin(p+"server", func() (string, error) { return tclaudeLayerServerCommand(in, 0, "exec opencode serve") })
		pin(p+"server+loopback", func() (string, error) { return tclaudeLayerServerCommand(in, 4567, "exec opencode serve") })
	}
	checkLayerPins(t, pins, order)
}
