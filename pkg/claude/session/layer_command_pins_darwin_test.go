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
	isolated.plan.NetworkPosture = sandboxpolicy.NetworkIsolatedWithAgentd
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
	for _, in := range inputs {
		p := in.name + "/"
		pin(p+"command", func() (string, error) {
			return tclaudeLayerCommand(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, "exec agent --flag")
		})
		pin(p+"command+loopback", func() (string, error) {
			return tclaudeLayerCommandWithLoopbackBind(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, 4567, "exec agent --flag")
		})
		pin(p+"command+route-slots", func() (string, error) {
			return tclaudeLayerCommandWithRouteSlots(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, []int{slot}, nil, &helper, "exec agent --flag")
		})
		pin(p+"command+route-slots-invalid", func() (string, error) {
			return tclaudeLayerCommandWithRouteSlots(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, []int{0}, nil, nil, "exec agent --flag")
		})
		pin(p+"command+route-slots+loopback", func() (string, error) {
			return tclaudeLayerCommandWithRouteSlotsAndLoopbackBind(binary, in.phase0WriteDirs, in.privateWriteDirs,
				in.finalHideDirs, in.readOnlyBinds, in.socketPaths, in.plan, []int{slot}, nil, &helper, 4567,
				"exec agent --flag")
		})
		pin(p+"command+route-helper", func() (string, error) {
			return tclaudeLayerCommandWithRouteHelper(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, helper, "exec agent --flag")
		})
		// Production renders a server only through the loopback variant: the
		// plain one's inherited-descriptor form has no caller.
		pin(p+"server", func() (string, error) {
			return tclaudeLayerServerCommandWithLoopbackBind(binary, in.phase0WriteDirs, in.privateWriteDirs,
				in.finalHideDirs, in.readOnlyBinds, in.socketPaths, in.plan, 0, "exec opencode serve")
		})
		pin(p+"server+loopback", func() (string, error) {
			return tclaudeLayerServerCommandWithLoopbackBind(binary, in.phase0WriteDirs, in.privateWriteDirs,
				in.finalHideDirs, in.readOnlyBinds, in.socketPaths, in.plan, 4567, "exec opencode serve")
		})
	}
	checkLayerPins(t, pins, order)
}
