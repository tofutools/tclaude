//go:build linux || darwin

package host

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
