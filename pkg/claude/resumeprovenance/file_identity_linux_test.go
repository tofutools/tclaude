//go:build linux

package resumeprovenance

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinuxStatIdentitySerializationValues(t *testing.T) {
	device, inode, err := linuxStatIdentity(&syscall.Stat_t{Dev: 41, Ino: 73})
	require.NoError(t, err)
	assert.Equal(t, uint64(41), device)
	assert.Equal(t, uint64(73), inode)

	_, _, err = linuxStatIdentity(&syscall.Stat_t{})
	assert.ErrorContains(t, err, "invalid device/inode")
}
