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
	proxyOnly := helper
	proxyOnly.ProxyOnly = true
	for _, input := range inputs {
		p, in := input.name+"/", input.in
		in.Binary = binary
		command := func(opts tclaudeLayerLaunchOpts) func() (string, error) {
			return func() (string, error) { return tclaudeLayerCommand(in, opts, "exec agent --flag") }
		}
		server := func(port int) func() (string, error) {
			return func() (string, error) { return tclaudeLayerServerCommand(in, port, "exec opencode serve") }
		}
		pin(p+"command", command(tclaudeLayerLaunchOpts{}))
		pin(p+"command+loopback", command(tclaudeLayerLaunchOpts{LoopbackBindPort: 4567}))
		pin(p+"command+route-slots-none", command(tclaudeLayerLaunchOpts{}))
		pin(p+"command+route-slots", command(tclaudeLayerLaunchOpts{RouteSlots: []int{1}}))
		pin(p+"command+route-slots+loopback", command(tclaudeLayerLaunchOpts{LoopbackBindPort: 4567}))
		pin(p+"command+route-helper", command(tclaudeLayerLaunchOpts{RouteHelper: &helper}))
		pin(p+"command+route-helper-proxy-only", command(tclaudeLayerLaunchOpts{RouteHelper: &proxyOnly}))
		pin(p+"server", server(0))
		pin(p+"server+loopback-zero", server(0))
		pin(p+"server+loopback", server(4567))
	}
	checkLayerPins(t, "layer_command_pins", pins, order)
}
