package common

import "os/exec"

// TmuxSocketName is the named socket for tclaude's independent tmux server.
const TmuxSocketName = "tclaude"

// Tmux is the boundary surface flow tests inject through. The default
// LiveTmux runs the real tmux binary; tests assign a fake to Default
// at setup and restore via t.Cleanup. This is the interface-based
// alternative to rewire's compile-time mocking.
type Tmux interface {
	Command(args ...string) *exec.Cmd
}

// Default is the package-wide Tmux instance every caller hits via the
// TmuxCommand facade. Production starts on LiveTmux; tests overwrite
// during their setup. Single global var = goroutine-unsafe across
// parallel tests on the same package, same as the rewire approach.
var Default Tmux = LiveTmux{}

// LiveTmux is the production impl: forks `tmux -L tclaude <args>`.
// Exported so tests can wrap it (e.g., a recording proxy that
// forwards to LiveTmux for some calls and to a fake for others).
type LiveTmux struct{}

// Command builds an exec.Cmd that invokes the real tmux binary.
func (LiveTmux) Command(args ...string) *exec.Cmd {
	return exec.Command("tmux", TmuxArgs(args...)...)
}

// TmuxCommand is a thin facade over Default.Command. Kept so the
// 48 existing call sites don't need to be rewritten in this diff;
// new code is welcome to call clcommon.Default.Command directly.
func TmuxCommand(args ...string) *exec.Cmd {
	return Default.Command(args...)
}

// TmuxArgs prepends -L tclaude to the given tmux arguments.
func TmuxArgs(args ...string) []string {
	return append([]string{"-L", TmuxSocketName}, args...)
}
