//go:build linux || darwin

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts cmd and reaps it in the background.
//
// Two problems it solves, both rooted in the launcher (agentd, or the
// `tclaude` CLI) being longer-lived than the terminal window:
//
//   - Zombies. A child the launcher never wait()s for stays a zombie
//     in the process table for the launcher's whole lifetime — the OS
//     only reparents, and so reaps, the children of a process that has
//     *exited*. The terminal window outlives this call, so we can't
//     block; a goroutine wait()s and reaps it whenever the window is
//     finally closed.
//   - Signal bleed. Without a new session the terminal shares the
//     launcher's process group; a Ctrl-C in whatever shell started the
//     launcher would also kill the user's freshly-opened agent
//     windows. Setsid puts the terminal in its own session, so it
//     survives that and a launcher restart.
//
// Shared by the Linux and macOS launchers (the macOS AppleScript path
// runs osascript synchronously and needs none of this).
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	debug := os.Getenv("TCLAUDE_DEBUG") != ""
	go func() {
		err := cmd.Wait()
		if debug && err != nil {
			fmt.Printf("[debug] terminal: %q exited: %v\n", cmd.Path, err)
		}
	}()
	return nil
}
