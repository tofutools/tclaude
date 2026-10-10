//go:build linux || darwin

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/tofutools/tclaude/pkg/noderun"
)

func executeOwnedHubScript(parent context.Context, path, _ string, seconds int64, stdout, stderr io.Writer) noderun.Result {
	if os.Geteuid() == 0 {
		return noderun.Result{ExitCode: 125, Error: "hub scripts cannot run as root"}
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", path)
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := command.Run()
	// Shell children must not outlive a completed, canceled or timed-out job.
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	result := noderun.Result{ExitCode: 0}
	if err != nil {
		result.ExitCode = 125
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				result.ExitCode = 128 + int(status.Signal())
			}
		} else {
			result.Error = "could not execute hub script"
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Error = "script timed out"
		result.TimedOut = true
	}
	return result
}

const execGuardianArgument = "--internal-hub-exec-guardian"

// RunExecGuardian handles only the private subprocess entry point. The parent
// supplies a lifetime pipe and result pipe as FD3/FD4; neither is inherited by
// scripts. The guardian owns the deadline and kills the script group on EOF,
// including when the hub is killed without running deferred cleanup.
func RunExecGuardian() {
	if len(os.Args) < 2 || os.Args[1] != execGuardianArgument {
		return
	}
	if len(os.Args) != 4 || os.Geteuid() == 0 {
		os.Exit(125)
	}
	seconds, err := strconv.ParseInt(os.Args[2], 10, 64)
	if err != nil || seconds < 1 || seconds > 86400 {
		os.Exit(125)
	}
	life, result := os.NewFile(3, "hub-lifetime"), os.NewFile(4, "hub-result")
	if life == nil || result == nil {
		os.Exit(125)
	}
	for _, file := range []*os.File{life, result} {
		info, e := file.Stat()
		if e != nil || info.Mode()&os.ModeNamedPipe == 0 {
			os.Exit(125)
		}
	}
	// ExtraFiles arrives without CLOEXEC; scripts must never inherit the
	// lifetime or result channels.
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = io.Copy(io.Discard, life); cancel() }()
	r := executeOwnedHubScript(ctx, os.Args[3], "", seconds, os.Stdout, os.Stderr)
	_ = json.NewEncoder(result).Encode(r)
	_ = result.Close()
	_ = life.Close()
	cancel()
	os.Exit(0)
}

func executeHubScript(ctx context.Context, path, _ string, seconds int64, stdout, stderr io.Writer) noderun.Result {
	if os.Geteuid() == 0 {
		return noderun.Result{ExitCode: 125, Error: "hub scripts cannot run as root"}
	}
	executable, err := os.Executable()
	if err != nil {
		return noderun.Result{ExitCode: 125, Error: "hub executable unavailable"}
	}
	lifeRead, lifeWrite, err := os.Pipe()
	if err != nil {
		return noderun.Result{ExitCode: 125, Error: "hub guardian unavailable"}
	}
	defer func() { _ = lifeRead.Close(); _ = lifeWrite.Close() }()
	resultRead, resultWrite, err := os.Pipe()
	if err != nil {
		return noderun.Result{ExitCode: 125, Error: "hub guardian unavailable"}
	}
	defer func() { _ = resultRead.Close(); _ = resultWrite.Close() }()
	command := exec.CommandContext(ctx, executable, execGuardianArgument, strconv.FormatInt(seconds, 10), path)
	command.ExtraFiles = []*os.File{lifeRead, resultWrite}
	command.Stdout, command.Stderr = stdout, stderr
	// Cancellation lets the guardian reap the script; killing the guardian first
	// would strand its children. WaitDelay is only a bounded fallback.
	command.Cancel = func() error { return lifeWrite.Close() }
	command.WaitDelay = 5 * time.Second
	if err = command.Start(); err != nil {
		return noderun.Result{ExitCode: 125, Error: "hub guardian start failed"}
	}
	_ = lifeRead.Close()
	_ = resultWrite.Close()
	waitErr := command.Wait()
	var r noderun.Result
	if err = json.NewDecoder(io.LimitReader(resultRead, 4096)).Decode(&r); err != nil {
		return noderun.Result{ExitCode: 125, Error: fmt.Sprintf("hub guardian failed (%v)", waitErr)}
	}
	return r
}
