package harness

// HostControlSandboxContract prepares harness-native sandbox settings that
// protect tclaude's host control plane from the launched agent. It is an
// optional descriptor capability: callers leave the launch unchanged when a
// harness has no reviewed implementation.
type HostControlSandboxContract interface {
	PrepareLaunch(SpawnSpec) (SpawnSpec, error)
}

// SupportsHostControlSandbox reports whether the harness owns reviewed
// launch-time protection for tclaude's host control plane.
func (h *Harness) SupportsHostControlSandbox() bool {
	return h != nil && h.HostControlSandbox != nil
}

// PrepareHostControlSandboxLaunch applies the descriptor-owned host-control
// boundary when supported and otherwise preserves the launch unchanged.
func (h *Harness) PrepareHostControlSandboxLaunch(spec SpawnSpec) (SpawnSpec, error) {
	if !h.SupportsHostControlSandbox() {
		return spec, nil
	}
	return h.HostControlSandbox.PrepareLaunch(spec)
}
