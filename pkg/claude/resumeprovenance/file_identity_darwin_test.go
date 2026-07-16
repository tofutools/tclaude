//go:build darwin

package resumeprovenance

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDarwinStatIdentitySerializationValues(t *testing.T) {
	device, inode, err := darwinStatIdentity(&syscall.Stat_t{Dev: 41, Ino: 73})
	require.NoError(t, err)
	assert.Equal(t, uint64(41), device)
	assert.Equal(t, uint64(73), inode)

	_, _, err = darwinStatIdentity(&syscall.Stat_t{})
	assert.ErrorContains(t, err, "invalid device/inode")
}
