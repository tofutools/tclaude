package agentipctest

import (
	"os"
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
