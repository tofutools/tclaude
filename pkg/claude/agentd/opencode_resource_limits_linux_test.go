//go:build linux

package agentd

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeManagedServerUsesPreparedResourceCgroupAtClone(t *testing.T) {
	cmd := exec.Command("/bin/true")
	closeFD, err := configureOpenCodeResourceCgroup(cmd, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(closeFD)
	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.UseCgroupFD)
}
