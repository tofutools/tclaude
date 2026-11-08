//go:build linux

package portowner_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
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

// A child forked from a thread other than the main one is listed under that
// thread (/proc/<pid>/task/<tid>/children), not the main thread's: the
// subtree must read every thread's, or a Go process's children go missing
// depending on which OS thread ran exec (the SharedPort test's old flake).
func TestProcessSubtree_ChildOfAnotherThread(t *testing.T) {
	started := make(chan *exec.Cmd, 1)
	errs := make(chan error, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		// Lock this goroutine to its own OS thread, and make sure that is
		// not the main thread: keep the main thread taken by a second
		// locked goroutine if it landed there.
		runtime.LockOSThread()
		if unix.Gettid() == os.Getpid() {
			done := make(chan struct{})
			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				if unix.Gettid() == os.Getpid() {
					errs <- errors.New("both goroutines landed on the main thread")
					close(done)
					return
				}
				cmd := exec.Command("sleep", "60")
				if err := cmd.Start(); err != nil {
					errs <- err
				} else {
					started <- cmd
				}
				close(done)
				<-release
			}()
			<-done
			<-release
			runtime.UnlockOSThread()
			return
		}
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			errs <- err
		} else {
			started <- cmd
		}
		// Hold the thread: a thread that exits reparents its children.
		<-release
		runtime.UnlockOSThread()
	}()
	var cmd *exec.Cmd
	select {
	case cmd = <-started:
	case err := <-errs:
		t.Fatalf("starting the child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	assert.True(t, portowner.ProcessInSubtree(os.Getpid(), cmd.Process.Pid),
		"a child forked from another thread is in the subtree")
}
