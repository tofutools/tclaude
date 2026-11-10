package agentd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// The relaunch helper (launcher.RestartRelaunch). On macOS the tray's status
// item does not come back after an exec in place, which keeps the PID the
// window server registered the old Cocoa image under. So a restart there
// re-executes into this thin, non-GUI helper instead, and the helper starts
// `agentd serve` in a fresh process. Exec-ing into the helper first, rather
// than starting the new daemon from the old one, drops every descriptor the
// old daemon held (the singleton lock, the dashboard port) before the new
// one starts.
const (
	relaunchCmdName = "relaunch"
	// relaunchDetach: start agentd in its own session and exit, for a
	// detached daemon nothing waits on.
	relaunchDetach = "detach"
	// relaunchWait: stay as the foreground job, forward signals, and exit
	// with agentd's status, so a shell (or whatever started agentd) sees
	// the same job.
	relaunchWait = "wait"
)

// relaunchHelperArgv returns the helper command line that relaunches the
// serve command line args with the binary at exe, or ok=false when args is
// not a serve command line. The helper is the same binary's
// `agentd relaunch` (or `tclaude-agentd relaunch`).
func relaunchHelperArgv(exe string, args []string, detach bool) ([]string, bool) {
	var prefix []string
	switch {
	case len(args) >= 3 && args[1] == "agentd" && args[2] == "serve":
		prefix = []string{args[0], "agentd"}
	case len(args) >= 2 && args[1] == "serve":
		prefix = []string{args[0]}
	default:
		return nil, false
	}
	mode := relaunchWait
	if detach {
		mode = relaunchDetach
	}
	out := append(prefix, relaunchCmdName, mode, exe, "--")
	return append(out, args...), true
}

// relaunchAvailable reports whether the binary at exe takes the helper
// command line helper (argv[0] first): a self-update may have installed a
// binary that predates the relaunch helper, which then re-executes in place
// instead. Tests swap it.
var relaunchAvailable = func(exe string, helper []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append([]string{selfupdate.CheckArgsFlag}, helper[1:]...)...)
	cmd.Env = launchEnviron
	return cmd.Run() == nil
}

// parseRelaunchArgs reads `<mode> <exe> -- <argv...>`.
func parseRelaunchArgs(args []string) (mode, exe string, argv []string, err error) {
	if len(args) < 4 || args[2] != "--" {
		return "", "", nil, fmt.Errorf("usage: relaunch detach|wait <exe> -- <argv...>")
	}
	mode, exe, argv = args[0], args[1], args[3:]
	if mode != relaunchDetach && mode != relaunchWait {
		return "", "", nil, fmt.Errorf("relaunch: unknown mode %q", mode)
	}
	if !filepath.IsAbs(exe) {
		return "", "", nil, fmt.Errorf("relaunch: %q is not an absolute path", exe)
	}
	return mode, exe, argv, nil
}

func relaunchCmd() *cobra.Command {
	return &cobra.Command{
		Use: relaunchCmdName + " detach|wait <exe> -- <argv...>", Hidden: true,
		Short:              "Start agentd again in a fresh process after a restart (internal)",
		DisableFlagParsing: true,
		PersistentPreRunE:  func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			mode, exe, argv, err := parseRelaunchArgs(args)
			if err != nil {
				return err
			}
			os.Exit(runRelaunch(mode, exe, argv))
			return nil
		},
	}
}

// Hand-back. A daemon the waiting helper started does not exec into a
// second helper when it restarts: that would nest one helper per restart in
// the shell job. It exits with relaunchHandBackExit instead, and its helper
// starts agentd again. The helper says it takes the hand-back by setting
// relaunchHelperVar to its PID in the environment it starts agentd with; a
// helper from an older binary does not, and the daemon then relaunches
// through a helper of its own, as before.
const (
	relaunchHelperVar    = "TCLAUDE_AGENTD_RELAUNCH_HELPER"
	relaunchHandBackExit = 76
)

// relaunchHelperPID is the PID of the waiting helper that started this
// process, 0 when none did. Read once, at startup: init removes the
// variable from the environment, so the agents and processes agentd starts
// do not inherit it, and launchEnviron leaves it out, so a restart's launch
// comparison does not see it come and go.
var relaunchHelperPID = func() int {
	pid, err := strconv.Atoi(os.Getenv(relaunchHelperVar))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
}()

func init() { _ = os.Unsetenv(relaunchHelperVar) }

// relaunchParentPID is this process's parent; tests swap it.
var relaunchParentPID = os.Getppid

// relaunchHandBack reports whether a relaunch restart should hand back to
// the waiting helper that started this daemon: that helper is still the
// parent (not exited, the daemon reparented), and it would start the same
// binary path (a self-update that moved the binary relaunches through a new
// helper, which starts the new path).
func relaunchHandBack(path, startPath string) bool {
	return relaunchHelperPID != 0 && relaunchParentPID() == relaunchHelperPID && path != "" && path == startPath
}
