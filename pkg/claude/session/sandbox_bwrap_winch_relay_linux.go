//go:build linux

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const tclaudeLayerWinchRelayCommand = "tclaude-layer-winch-relay"

type bwrapChildStatus struct {
	ChildPID int `json:"child-pid"`
}

// tclaudeLayerWinchRelayCmd stays outside bubblewrap and outside the terminal
// I/O path. Bubblewrap inherits stdin/stdout/stderr directly; this process only
// turns the host PTY's SIGWINCH notification into the same fixed signal for the
// disconnected sandbox process group.
func tclaudeLayerWinchRelayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                tclaudeLayerWinchRelayCommand + " <bwrap> [args...]",
		Short:              "Relay terminal resize notifications into tclaude-layer (internal)",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			defer signal.Stop(winch)

			code, err := runTclaudeLayerWinchRelay(args, winch)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tclaude: terminal resize relay: %v\n", err)
				os.Exit(125)
			}
			os.Exit(code)
		},
	}
	return cmd
}

// runTclaudeLayerWinchRelay launches one bubblewrap argv, learns the
// host-visible identity of its initial sandbox process from bubblewrap itself,
// and forwards SIGWINCH to that process group. --new-session makes the
// reported child its own process-group leader; signaling the group rather than
// the one process is load-bearing because production runs the TUI beneath
// `sh -c <harness command>`.
func runTclaudeLayerWinchRelay(argv []string, winch <-chan os.Signal) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 125, fmt.Errorf("missing bubblewrap command")
	}

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 125, fmt.Errorf("create bubblewrap status pipe: %w", err)
	}
	defer func() { _ = statusR.Close() }()

	childArgs := make([]string, 0, len(argv)+2)
	childArgs = append(childArgs, "--json-status-fd", "3")
	childArgs = append(childArgs, argv[1:]...)
	child := exec.Command(argv[0], childArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.ExtraFiles = []*os.File{statusW}
	if err := child.Start(); err != nil {
		_ = statusW.Close()
		return 125, fmt.Errorf("start bubblewrap: %w", err)
	}
	_ = statusW.Close()

	waited := false
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	defer func() {
		if waited {
			return
		}
		_ = child.Process.Kill()
		<-waitCh
	}()

	var status bwrapChildStatus
	if err := json.NewDecoder(statusR).Decode(&status); err != nil {
		if errors.Is(err, io.EOF) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		_ = child.Process.Kill()
		<-waitCh
		waited = true
		return 125, fmt.Errorf("read bubblewrap child identity: %w", err)
	}
	if status.ChildPID <= 0 {
		return 125, fmt.Errorf("bubblewrap reported invalid child pid %d", status.ChildPID)
	}
	pidfd, err := unix.PidfdOpen(status.ChildPID, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		return 125, fmt.Errorf("pin bubblewrap child pid %d: %w", status.ChildPID, err)
	}
	defer func() { _ = unix.Close(pidfd) }()

	pgid, err := unix.Getpgid(status.ChildPID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		return 125, fmt.Errorf("resolve bubblewrap child process group: %w", err)
	}
	if pgid != status.ChildPID {
		return 125, fmt.Errorf(
			"bubblewrap --new-session child pid %d is not its process-group leader (pgid %d)",
			status.ChildPID, pgid,
		)
	}

	for {
		select {
		case _, ok := <-winch:
			if !ok {
				winch = nil
				continue
			}
			if err := signalPinnedTclaudeLayerGroup(pidfd, pgid); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				return 125, fmt.Errorf("forward SIGWINCH to sandbox process group: %w", err)
			}
		case waitErr := <-waitCh:
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
	}
}

// signalPinnedTclaudeLayerGroup verifies that the pidfd-pinned group leader is
// still alive before addressing its process group. No user-selected PID or
// signal reaches this sink: both the pidfd and pgid came from bubblewrap and
// SIGWINCH is a compile-time constant.
func signalPinnedTclaudeLayerGroup(pidfd, pgid int) error {
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
		return err
	}
	return unix.Kill(-pgid, syscall.SIGWINCH)
}

func tclaudeLayerRelayExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 125, fmt.Errorf("wait for bubblewrap: %w", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return 125, fmt.Errorf("inspect bubblewrap exit status: %w", err)
	}
	switch {
	case status.Exited():
		return status.ExitStatus(), nil
	case status.Signaled():
		return 128 + int(status.Signal()), nil
	default:
		return 125, fmt.Errorf("bubblewrap exited with unsupported wait status %v", status)
	}
}
