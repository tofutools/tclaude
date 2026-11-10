//go:build linux || darwin

package agentd

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
)

// relaunchSignals are forwarded from the waiting helper to agentd. The
// terminal's SIGINT/SIGQUIT/SIGHUP reach agentd too (it stays in the same
// process group); agentd's shutdown handling takes a second one as a no-op.
// A signal the helper was started with ignored (nohup's SIGHUP, a
// background job's SIGINT) is left alone: handling it would reset it to
// the default in agentd, which an exec in place keeps ignored.
var relaunchSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// runRelaunch starts argv with the binary exe in a fresh process and
// returns the helper's exit status.
func runRelaunch(mode, exe string, argv []string) int {
	sigs := make(chan os.Signal, 4)
	if mode == relaunchWait {
		// Before the child exists, so no signal is missed or kills the helper.
		for _, s := range relaunchSignals {
			if !signal.Ignored(s) {
				signal.Notify(sigs, s)
			}
		}
		defer signal.Stop(sigs)
	}
	for {
		cmd := relaunchCommand(mode, exe, argv)
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Restart failed: %v\n", err)
			return 1
		}
		if mode == relaunchDetach {
			_ = cmd.Process.Release()
			return 0
		}
		code, sig := relaunchWaitChild(cmd, sigs)
		if sig == 0 && code == relaunchHandBackExit {
			// agentd restarted and handed back (relaunchHandBack). A
			// signal that came while it stopped was meant for it: stop
			// as it would have.
			select {
			case s := <-sigs:
				sig, _ = s.(syscall.Signal)
			default:
				continue
			}
		}
		if sig != 0 {
			// Die of the same signal, so the shell reports what agentd did.
			signal.Stop(sigs)
			signal.Reset(sig)
			_ = syscall.Kill(os.Getpid(), sig)
			return 128 + int(sig)
		}
		return code
	}
}

// relaunchCommand builds the agentd process: the helper's launch
// environment (the old daemon's, captured before startup changed anything)
// and stdio, in its own session when
// detached, otherwise in the helper's process group, so Ctrl+C and job
// control reach it as before. A waited-for agentd is told the helper's
// PID, so its next restart hands back here (relaunchHandBack).
func relaunchCommand(mode, exe string, argv []string) *exec.Cmd {
	env := launchEnviron
	if mode == relaunchDetach {
		// Nothing waits for a detached daemon to hand back.
		env = slices.Clip(env)
	} else {
		env = append(slices.Clip(env), relaunchHelperVar+"="+strconv.Itoa(os.Getpid()))
	}
	cmd := &exec.Cmd{Path: exe, Args: argv, Env: env, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if mode == relaunchDetach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	return cmd
}

// relaunchWaitChild forwards sigs to the started cmd until it exits, and
// returns its exit code, or the signal that ended it.
func relaunchWaitChild(cmd *exec.Cmd, sigs <-chan os.Signal) (int, syscall.Signal) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	if err == nil {
		return 0, 0
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 0, ws.Signal()
		}
		return ws.ExitStatus(), 0
	}
	return 1, 0
}
