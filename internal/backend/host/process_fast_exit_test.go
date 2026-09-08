//go:build linux || darwin

package host

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestFastProcessExitRetainsNaturalStatusWhenIdentityDisappears(t *testing.T) {
	for _, stage := range []string{"group", "token"} {
		t.Run(stage, func(t *testing.T) {
			var group func(int) (int, error)
			token := processStartToken
			if stage == "group" {
				group = func(int) (int, error) { return 0, syscall.ESRCH }
			} else {
				group = func(pid int) (int, error) { return pid, nil }
				token = func(int) (string, error) { return "", os.ErrNotExist }
			}
			process, err := startProcess(ProcessSpec{Executable: "/bin/sh", Args: []string{"-c", "exit 7"}}, group, token)
			require.NoError(t, err)
			require.Eventually(t, func() bool { return process.Observe().Exited }, time.Second, 5*time.Millisecond)
			observed := process.Observe()
			require.True(t, observed.Exited)
			require.NotNil(t, observed.ExitCode)
			require.Equal(t, 7, *observed.ExitCode, "identity loss must not kill or erase the natural result")
			require.Empty(t, process.Identity().StartToken)
			require.Zero(t, process.Identity().ProcessGroup)
			_, err = RecoverProcess(process.Identity())
			require.ErrorIs(t, err, ErrProcessIdentityNotLive)
			require.ErrorIs(t, process.Signal(syscall.SIGTERM), os.ErrProcessDone)
		})
	}
}

func TestFastProcessExitDoesNotSuppressOtherIdentityErrors(t *testing.T) {
	_, err := startProcess(ProcessSpec{Executable: "/bin/sh", Args: []string{"-c", "sleep 5"}}, func(int) (int, error) { return 0, syscall.EACCES }, processStartToken)
	require.ErrorIs(t, err, syscall.EACCES)
}

func TestFastProcessExitReportsRealImmediateFailure(t *testing.T) {
	for range 20 {
		process, err := StartProcess(ProcessSpec{Executable: "/bin/sh", Args: []string{"-c", "exit 7"}})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return process.Observe().ExitCode != nil }, time.Second, 5*time.Millisecond)
		require.Equal(t, 7, *process.Observe().ExitCode)
	}
}

// Block the parent-side copy independently of the child's natural exit.
type pendingExitWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pendingExitWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(data), nil
}

func TestFastProcessExitReturnsWhileOutputCollectionIsPending(t *testing.T) {
	writer := &pendingExitWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(writer.release) }) }
	defer unblock()
	type result struct {
		process *Process
		err     error
	}
	started := make(chan result, 1)
	go func() {
		p, err := startProcess(ProcessSpec{Executable: "/bin/sh", Args: []string{"-c", "printf result; exit 7"}, Stdout: writer}, func(int) (int, error) { return 0, syscall.ESRCH }, processStartToken)
		started <- result{p, err}
	}()
	var process *Process
	select {
	case r := <-started:
		require.NoError(t, r.err)
		process = r.process
	case <-time.After(time.Second):
		t.Fatal("start blocked on retained output copy")
	}
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("output copy did not start")
	}
	observed := process.Observe()
	require.True(t, observed.Reaping)
	require.False(t, observed.Running)
	require.False(t, observed.Exited)
	require.Nil(t, observed.ExitCode)

	output := newBoundedOutputFile(t.TempDir(), "stdout", "truncated", "complete", 128)
	pending, pendingErr := observeOutputPending(process.Observe, output)
	require.NoError(t, pendingErr)
	require.True(t, pending)
	require.NoError(t, os.WriteFile(output.path, nil, 0600))
	require.NoError(t, os.WriteFile(output.completePath, nil, 0600))
	runtime := &programRuntime{process: process, stdout: output, stderr: output}
	programObservation, observeErr := runtime.ObserveProgram(context.Background())
	require.NoError(t, observeErr)
	require.Equal(t, ports.WorkloadRunning, programObservation.Workload, "retained collection must remain reconcilable, not settle unknown")
	require.Nil(t, programObservation.ExitCode)
	require.Error(t, runtime.ReleaseProgramResources(context.Background(), runtime.envelope))
	require.ErrorIs(t, process.Signal(syscall.SIGTERM), os.ErrProcessDone)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ack, exited, err := process.Stop(ctx, false)
	require.False(t, ack)
	require.False(t, exited)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	unblock()
	require.Eventually(t, func() bool { return process.Observe().Exited }, time.Second, 5*time.Millisecond)
	require.Equal(t, 7, *process.Observe().ExitCode)
	programObservation, observeErr = runtime.ObserveProgram(context.Background())
	require.NoError(t, observeErr)
	require.Equal(t, ports.WorkloadExited, programObservation.Workload)
	require.Equal(t, 7, *programObservation.ExitCode)
}
