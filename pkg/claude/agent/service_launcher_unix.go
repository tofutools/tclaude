//go:build linux || darwin

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// startDetachedServe starts `tclaude agentd serve` in its own session, its
// output appended to logFile, and does not wait for it: the launcher exits
// while agentd keeps running.
func startDetachedServe(argv, env []string, logFile string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// appendLauncherLog appends msg to the launcher's log, best effort.
func appendLauncherLog(logFile, msg string) {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(msg + "\n")
}
