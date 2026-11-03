//go:build linux

package portowner_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
)

func reusePortListen(t *testing.T, addr string) *net.TCPListener {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp4", addr)
	require.NoError(t, err)
	return ln.(*net.TCPListener)
}

// Two SO_REUSEPORT listeners share a port. A subtree holding only one of them
// passes the lax proof but not the strict one: the kernel would send some
// dials to the other.
func TestProcessOwnsAllLoopbackListeners_SharedPort(t *testing.T) {
	first := reusePortListen(t, "127.0.0.1:0")
	port := first.Addr().(*net.TCPAddr).Port
	second := reusePortListen(t, first.Addr().String())
	t.Cleanup(func() { _ = second.Close() })

	// Hand the first socket to a child process, which becomes the "agent";
	// the test process keeps only the second.
	f, err := first.File()
	require.NoError(t, err)
	_ = first.Close()
	child := exec.Command("sleep", "60")
	child.ExtraFiles = []*os.File{f}
	require.NoError(t, child.Start())
	_ = f.Close()
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	agent := child.Process.Pid
	assert.True(t, portowner.ProcessOwnsLoopbackPort(agent, port), "lax: one listener is the agent's")
	assert.False(t, portowner.ProcessOwnsAllLoopbackListeners(agent, port), "strict: another listener is not")
	// The test process's subtree includes the child, so it holds both.
	assert.True(t, portowner.ProcessOwnsAllLoopbackListeners(os.Getpid(), port))
}

func TestProcessOwnsAllLoopbackListeners_Basics(t *testing.T) {
	port := listenLoopback(t)
	assert.True(t, portowner.ProcessOwnsAllLoopbackListeners(os.Getpid(), port))
	assert.False(t, portowner.ProcessOwnsAllLoopbackListeners(os.Getpid(), 0))
	assert.False(t, portowner.ProcessOwnsAllLoopbackListeners(1, port))
}
