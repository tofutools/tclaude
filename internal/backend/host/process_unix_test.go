//go:build linux || darwin

package host

import (
	"context"
	"os"
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
