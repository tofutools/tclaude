package common

import "os/exec"

// TmuxSocketName is the named socket for tclaude's independent tmux server.
const TmuxSocketName = "tclaude"

// TmuxCommand creates an exec.Cmd for tmux using the tclaude socket (-L tclaude).
func TmuxCommand(args ...string) *exec.Cmd {
	return exec.Command("tmux", TmuxArgs(args...)...)
}

// TmuxArgs prepends -L tclaude to the given tmux arguments.
func TmuxArgs(args ...string) []string {
	return append([]string{"-L", TmuxSocketName}, args...)
}
