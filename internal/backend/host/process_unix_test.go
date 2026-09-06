//go:build linux || darwin

package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessRecoveryAndExactStop(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	process, err := StartProcess(ProcessSpec{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestHostProcessHelper", "--", marker},
		Env:        []string{"TCLAUDE_HOST_PROCESS_HELPER=1"},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = process.Stop(ctx, true)
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	recovered, err := RecoverProcess(process.Identity())
	require.NoError(t, err)
	require.True(t, recovered.Observe().Running)

	wrong := process.Identity()
	wrong.StartToken += "-replacement"
	_, err = RecoverProcess(wrong)
	require.Error(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	acknowledged, exited, err := recovered.Stop(ctx, false)
	require.NoError(t, err)
	require.True(t, acknowledged)
	require.True(t, exited)
}

func TestRecoverProcessByEnvironmentSelectsMarkedLauncherRoot(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "descendant-started")
	marker := "attempt-marker-with-descendant"
	process, err := StartProcess(ProcessSpec{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestHostMarkedLauncherHelper", "--", ready},
		Env: []string{
			"TCLAUDE_HOST_MARKED_LAUNCHER_HELPER=1",
			"TCLAUDE_HOST_RECOVERY_MARKER=" + marker,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = process.Stop(ctx, true)
	})
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(ready)
		return statErr == nil
	}, time.Second, 10*time.Millisecond)

	recovered, err := RecoverProcessByEnvironment("TCLAUDE_HOST_RECOVERY_MARKER", marker)
	require.NoError(t, err)
	require.Equal(t, process.Identity(), recovered.Identity(),
		"a marker inherited by a child still identifies the launcher root")
}

func TestHostProcessHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_HOST_PROCESS_HELPER") != "1" {
		return
	}
	args := os.Args
	for index, arg := range args {
		if arg == "--" && index+1 < len(args) {
			require.NoError(t, os.WriteFile(args[index+1], []byte("started"), 0o600))
			select {}
		}
	}
	os.Exit(2)
}

func TestHostMarkedLauncherHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_HOST_MARKED_LAUNCHER_HELPER") != "1" {
		return
	}
	args := processHelperArgsAfterDoubleDash(os.Args)
	cmd := exec.Command(os.Args[0], "-test.run=TestHostMarkedDescendantHelper")
	cmd.Env = MergeEnvironment(os.Environ(), []string{
		"TCLAUDE_HOST_MARKED_LAUNCHER_HELPER=",
		"TCLAUDE_HOST_MARKED_DESCENDANT_HELPER=1",
	})
	require.NoError(t, cmd.Start())
	require.NoError(t, os.WriteFile(args[0], []byte("started"), 0o600))
	select {}
}

func TestHostMarkedDescendantHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_HOST_MARKED_DESCENDANT_HELPER") != "1" {
		return
	}
	select {}
}

func processHelperArgsAfterDoubleDash(args []string) []string {
	for index, value := range args {
		if value == "--" && index+1 < len(args) {
			return args[index+1:]
		}
	}
	return nil
}
