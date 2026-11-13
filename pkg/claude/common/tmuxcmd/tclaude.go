package tmuxcmd

import "strings"

// The tmux option and hook names tclaude's exit audit is built on. They are
// named here, with the verbs that use them, so no caller spells them out.
const (
	// OptionExitGeneration is the pane-local user option holding the launch
	// identity of the process tclaude started in a pane. A pane-died callback
	// and every later probe compare it against the launch they belong to, so
	// evidence from an earlier process that reused the tmux name is rejected.
	OptionExitGeneration = "@tclaude_exit_generation"
	// OptionRemainOnExit is tmux's remain-on-exit: on keeps a pane whose
	// process exited as a readable corpse instead of closing it, which is what
	// lets the exit status and the dying screen be read afterwards.
	OptionRemainOnExit = "remain-on-exit"
	// HookPaneDied is the tmux hook that fires when a retained pane's process
	// exits.
	HookPaneDied = "pane-died"
)

// SetPaneExitGeneration records the launch identity on the pane.
func (c Client) SetPaneExitGeneration(p PaneTarget, generation string) error {
	return c.SetOption(PaneScope(p), OptionExitGeneration, generation)
}

// ClearPaneExitGeneration removes the pane's launch identity.
func (c Client) ClearPaneExitGeneration(p PaneTarget) error {
	return c.UnsetOption(PaneScope(p), OptionExitGeneration)
}

// SetPaneRemainOnExit sets remain-on-exit on the pane itself, overriding
// whatever the pane would inherit from the user's tmux configuration.
func (c Client) SetPaneRemainOnExit(p PaneTarget, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	return c.SetOption(PaneScope(p), OptionRemainOnExit, value)
}

// SetPaneDiedHook makes tmux run command (tmux command text) when this pane's
// process exits. It only fires for a pane that is retained, so it is paired
// with SetPaneRemainOnExit(p, true).
func (c Client) SetPaneDiedHook(p PaneTarget, command string) error {
	return c.SetHook(PaneScope(p), HookPaneDied, command)
}

// ClearPaneDiedHook removes the pane's own pane-died handler.
func (c Client) ClearPaneDiedHook(p PaneTarget) error {
	return c.UnsetHook(PaneScope(p), HookPaneDied)
}

// PaneDiedHookSupported reports whether the tmux server knows the pane-died
// hook at all; tmux builds too old for it cannot run the exit audit. Any
// failure to ask reads as unsupported.
func (c Client) PaneDiedHookSupported() bool {
	out, err := c.run("show-hooks", "-g", HookPaneDied)
	if err != nil {
		return false
	}
	for _, line := range lines(out) {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == HookPaneDied {
			return true
		}
	}
	return false
}
