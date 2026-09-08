//go:build linux

package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

func (c *sandboxCgroup) containsCurrentProcess() error {
	if c == nil {
		return fmt.Errorf("resource child has no prepared boundary")
	}
	file, err := c.open()
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	raw, err := readCgroupFile(file, "cgroup.procs")
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(string(raw)), strconv.Itoa(os.Getpid())) {
		return fmt.Errorf("sandbox child is outside its prepared resource boundary")
	}
	return c.verify()
}

// The supervisor remains outside the workload's ceiling, so an OOM cannot kill
// its cleanup owner. clone3 places the bootstrap in the boundary before any
// native setup or workload instructions run. The inner bootstrap still execs
// the sandbox wrapper and owns all descriptor preparation itself.
func superviseSandboxResources(ctx context.Context, _ SandboxChildArtifact, c *sandboxCgroup) error {
	if err := c.verify(); err != nil {
		return err
	}
	boundary, err := c.open()
	if err != nil {
		return err
	}
	defer func() { _ = boundary.Close() }()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(executable, os.Args[1:]...)
	child.Env = MergeEnvironment(os.Environ(), []string{sandboxResourceChildMarker + "=1"})
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(boundary.Fd()), Pdeathsig: syscall.SIGKILL}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start resource-limited workload: %w", err)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	var waitErr error
waiting:
	for {
		select {
		case waitErr = <-done:
			break waiting
		case sig := <-signals:
			_ = child.Process.Signal(sig)
		case <-ctx.Done():
			_ = c.kill()
			waitErr = <-done
			break waiting
		}
	}
	err = c.settle()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resource boundary cleanup:", err)
	}
	code := 0
	if waitErr != nil {
		var exited *exec.ExitError
		if !errors.As(waitErr, &exited) {
			return waitErr
		}
		status, ok := exited.Sys().(syscall.WaitStatus)
		if !ok {
			return waitErr
		}
		if status.Signaled() {
			code = 128 + int(status.Signal())
		} else {
			code = status.ExitStatus()
		}
	}
	// ExecuteSandboxChild otherwise replaces this process with exec. Preserve
	// that contract and the native exit status on the supervised resource path.
	os.Exit(code)
	return nil
}
