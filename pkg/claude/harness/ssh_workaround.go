package harness

import (
	"fmt"
	"runtime"
)

// CanSSHWorkaround reports whether tclaude can give this harness an
// ownership-safe SSH client configuration. Every harness can encounter the
// ownership translation in tclaude's Linux packet sandbox; launch-time
// applicability still limits the historical harness-builtin case to Codex's
// managed sandbox.
func (h *Harness) CanSSHWorkaround() bool {
	return h != nil && runtime.GOOS == "linux"
}

// ResolveSSHWorkaround resolves the profile/spawn tri-state. Linux harnesses
// default to ON so the launch boundary can apply it only to an
// ownership-translating sandbox shape.
func ResolveSSHWorkaround(h *Harness, requested *bool) (bool, error) {
	if requested == nil {
		return h != nil && h.CanSSHWorkaround(), nil
	}
	if *requested && h == nil {
		return false, fmt.Errorf("SSH compatibility workaround requires a harness")
	}
	return *requested && h != nil && h.CanSSHWorkaround(), nil
}
