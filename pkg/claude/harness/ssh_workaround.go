package harness

import (
	"fmt"
	"runtime"
)

// CanSSHWorkaround reports whether tclaude can give managed agents an
// ownership-safe SSH client configuration. The workaround exists for Codex's
// Linux user-namespace sandbox, which can expose root-owned system SSH
// drop-ins as nobody:nogroup and make OpenSSH reject them.
func (h *Harness) CanSSHWorkaround() bool {
	return h != nil && h.Name == CodexName && runtime.GOOS == "linux"
}

// ResolveSSHWorkaround resolves the profile/spawn tri-state. Codex defaults to
// ON so ordinary managed agents get working Git-over-SSH; other harnesses
// default to OFF and reject an explicit opt-in.
func ResolveSSHWorkaround(h *Harness, requested *bool) (bool, error) {
	if requested == nil {
		return h != nil && h.CanSSHWorkaround(), nil
	}
	if *requested && (h == nil || h.Name != CodexName) {
		name := ""
		if h != nil {
			name = h.Name
		}
		return false, fmt.Errorf("SSH compatibility workaround is not supported by harness %q", name)
	}
	return *requested && h != nil && h.CanSSHWorkaround(), nil
}
