//go:build darwin

package host

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinProcessStartTokenReportsReapedPIDAbsent(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	_, err := processStartToken(pid)
	require.True(t, errors.Is(err, os.ErrNotExist), "reaped pid %d returned %v", pid, err)
}
