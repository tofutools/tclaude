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

	"github.com/tofutools/tclaude/pkg/launcher"
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
	env := launchEnviron
	if msg, ok := os.LookupEnv(launcher.LoginEnvErrorVar); ok {
		// The restart that started this helper could not refresh the
		// environment: the agentd it starts tells the operator.
		env = append(slices.Clip(env), launcher.LoginEnvErrorVar+"="+msg)
	}
	for {
		cmd := relaunchCommand(mode, exe, argv, env)
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Restart failed: %v\n", err)
			return 1
		}
		if mode == relaunchDetach {
			_ = cmd.Process.Release()
			return 0
		}
		code, sig, forwarded := relaunchWaitChild(cmd, sigs)
		if sig == 0 && code == relaunchHandBackExit {
			// agentd restarted and handed back (relaunchHandBack). A
			// signal sent while it stopped for the restart (Ctrl+C) was
			// meant to stop it, which its shutdown took as a no-op: stop
			// instead of starting it again.
			if forwarded == 0 {
				select {
				case s := <-sigs:
					forwarded, _ = s.(syscall.Signal)
				default:
				}
			}
			if forwarded == 0 {
				// A restart: like a fresh start, with the login shell's
				// environment as it is now.
				env = relaunchRestartEnviron(launchEnviron)
				continue
			}
			sig = forwarded
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

// relaunchRestartEnviron is the environment the waiting helper starts
// agentd with again after a hand-back; tests swap it.
var relaunchRestartEnviron = daemonRestartEnviron

// relaunchCommand builds the agentd process: env (the helper's launch
// environment, which the old daemon built for it, or a fresh one after a
// hand-back) and stdio, in its own session when
// detached, otherwise in the helper's process group, so Ctrl+C and job
// control reach it as before. A waited-for agentd is told the helper's
// PID, so its next restart hands back here (relaunchHandBack).
func relaunchCommand(mode, exe string, argv, env []string) *exec.Cmd {
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
// returns its exit code, or the signal that ended it, and the last signal
// it forwarded (0 for none).
func relaunchWaitChild(cmd *exec.Cmd, sigs <-chan os.Signal) (code int, sig, forwarded syscall.Signal) {
	done := make(chan struct{})
	stopped := make(chan syscall.Signal)
	go func() {
		var last syscall.Signal
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
				if ss, ok := s.(syscall.Signal); ok {
					last = ss
				}
			case <-done:
				stopped <- last
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	forwarded = <-stopped
	if err == nil {
		return 0, 0, forwarded
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 0, ws.Signal(), forwarded
		}
		return ws.ExitStatus(), 0, forwarded
	}
	return 1, 0, forwarded
}
