//go:build linux

package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodexAppServerArgsTargetExactGenerationSocket(t *testing.T) {
	args := [][]byte{[]byte("/opt/codex"), []byte("app-server"), []byte("--listen"), []byte("unix:///private/generation/app.sock")}
	assert.True(t, codexAppServerArgsTargetSocket(args, "/private/generation/app.sock"))
	assert.False(t, codexAppServerArgsTargetSocket(args, "/private/replacement/app.sock"))
	assert.False(t, codexAppServerArgsTargetSocket(
		[][]byte{[]byte("/opt/codex"), []byte("app-server"), []byte("--listen"), []byte("unix:///private/generation/app.sock.extra")},
		"/private/generation/app.sock"), "socket argv proof must be an exact argument match")
}
