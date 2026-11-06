//go:build linux

package session

import (
	"testing"
)

func TestLayerCommandPins(t *testing.T) {
	inputs, normalise := layerPinFixture(t)
	const binary = "/usr/bin/bwrap"
	helper := *testRouteHelper()
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
		pin(p+"command+route-slots-none", func() (string, error) {
			return tclaudeLayerCommandWithRouteSlots(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, nil, nil, nil, "exec agent --flag")
		})
		pin(p+"command+route-slots", func() (string, error) {
			return tclaudeLayerCommandWithRouteSlots(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, []int{1}, nil, nil, "exec agent --flag")
		})
		pin(p+"command+route-slots+loopback", func() (string, error) {
			return tclaudeLayerCommandWithRouteSlotsAndLoopbackBind(binary, in.phase0WriteDirs, in.privateWriteDirs,
				in.finalHideDirs, in.readOnlyBinds, in.socketPaths, in.plan, nil, nil, nil, 4567, "exec agent --flag")
		})
		pin(p+"command+route-helper", func() (string, error) {
			return tclaudeLayerCommandWithRouteHelper(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, helper, "exec agent --flag")
		})
		proxyOnly := helper
		proxyOnly.ProxyOnly = true
		pin(p+"command+route-helper-proxy-only", func() (string, error) {
			return tclaudeLayerCommandWithRouteHelper(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, proxyOnly, "exec agent --flag")
		})
		pin(p+"server", func() (string, error) {
			return tclaudeLayerServerCommand(binary, in.phase0WriteDirs, in.privateWriteDirs, in.finalHideDirs,
				in.readOnlyBinds, in.socketPaths, in.plan, "exec opencode serve")
		})
		pin(p+"server+loopback-zero", func() (string, error) {
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
