package agentipctest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsolateSocketEnv(t *testing.T) {
	t.Setenv(socketEnv, "/real/daemon.sock")

	t.Run("isolated test", func(t *testing.T) {
		IsolateSocketEnv(t)
		assert.Empty(t, os.Getenv(socketEnv))
	})

	assert.Equal(t, "/real/daemon.sock", os.Getenv(socketEnv), "subtest cleanup restores parent environment")
}

func TestShortSocketDirLeavesRoomForCanonicalSocket(t *testing.T) {
	dir := ShortSocketDir(t)
	stable := filepath.Join(dir, ".tclaude", "api", "agentd-socket", "agentd.sock")
	assert.LessOrEqual(t, len(stable)+1, maxSocketPathLen)
}
