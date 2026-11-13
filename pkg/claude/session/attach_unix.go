//go:build !windows

package session

import (
	"fmt"
	"os"
	"os/exec"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
)

// attachClientFeatureArgs picks the tmux client flags for this attach. A caller
// that knows what renders this client's output sets the env var (the dashboard's
// PTY bridge does, for OSC 8), and that choice stands even when malformed drops
// it. A native terminal attach leaves it unset and goes by its own TERM.
func attachClientFeatureArgs() []string {
	if features := os.Getenv(clcommon.TmuxClientFeaturesEnv); features != "" {
		return clcommon.TmuxClientFeatureArgs(features)
	}
	return clcommon.NativeTmuxClientFeatureArgs(os.Getenv("TERM"))
}

// attachToSessionWithFlags attaches to a tmux session with optional force flag.
// If force is true, uses -d to detach other clients.
// Runs tmux as a subprocess so we return when the user detaches.
func attachToSessionWithFlags(tmuxSession string, force bool) error {
	argv, err := tmuxcmd.Local().AttachCommand(tmuxcmd.Session(tmuxSession), tmuxcmd.AttachOptions{
		DetachOthers: force, ClientFlags: attachClientFeatureArgs(),
	})
	if err != nil {
		return fmt.Errorf("tmux attach failed: %w", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if err != nil {
		// Exit status 1 typically means session ended or detached - not an error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("tmux attach failed: %w", err)
	}
	return nil
}
