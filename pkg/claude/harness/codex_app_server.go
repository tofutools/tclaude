package harness

import "fmt"

// SupportsCodexAppServer reports whether the harness can use the opt-in
// app-server-backed control path. The legacy Codex pane/send-keys path remains
// the default and is selected by false.
func (h *Harness) SupportsCodexAppServer() bool {
	return h != nil && h.Name == CodexName
}

func (h *Harness) CanCodexAppServer() bool { return h.SupportsCodexAppServer() }

// ResolveCodexAppServer validates and resolves the tri-state launch choice.
// Unset is deliberately false so existing Codex launches are byte-for-byte on
// their historical path until an operator or spawn profile opts in.
func ResolveCodexAppServer(h *Harness, requested *bool) (bool, error) {
	if requested == nil {
		return false, nil
	}
	if *requested && !h.CanCodexAppServer() {
		return false, fmt.Errorf("harness %q has no Codex app-server drive (available only for %s)",
			harnessName(h), CodexName)
	}
	return *requested, nil
}
