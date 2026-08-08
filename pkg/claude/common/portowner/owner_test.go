package portowner_test

import (
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
)

// listenLoopback binds a real 127.0.0.1 listener and returns its port. The
// tests below deliberately prove things about an actual kernel socket rather
// than a fake: the whole value of this package is that it reads the kernel's
// own answer, so a stubbed one would test nothing.
func listenLoopback(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

// The test process owns a listener it just bound. This is the case that must
// say yes, and it is the one a launched harness reproduces one exec deeper.
func TestProcessOwnsLoopbackPort_OwnListener(t *testing.T) {
	port := listenLoopback(t)
	assert.True(t, portowner.ProcessOwnsLoopbackPort(os.Getpid(), port),
		"a process must be recognised as owning the loopback listener it holds")
}

// The bind race this package exists to close: something else holds the port.
// PID 1 stands in for "not my subtree" — it is guaranteed to exist and is
// guaranteed not to be an ancestor of this test's listener.
func TestProcessOwnsLoopbackPort_ForeignOwnerIsRefused(t *testing.T) {
	port := listenLoopback(t)
	assert.False(t, portowner.ProcessOwnsLoopbackPort(1, port),
		"a listener outside the subtree must not be reported as owned")
}

// Nothing is listening, so there is nothing to own. A caller that treated this
// as ownership would talk to whatever bound the port next.
func TestProcessOwnsLoopbackPort_UnboundPortIsNotOwned(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	assert.False(t, portowner.ProcessOwnsLoopbackPort(os.Getpid(), port),
		"a port with no listener must not be reported as owned")
}

// Out-of-range and sentinel values must fail closed rather than reaching the
// platform lookup with a nonsense argument.
func TestProcessOwnsLoopbackPort_RejectsUnusablePorts(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		assert.False(t, portowner.ProcessOwnsLoopbackPort(os.Getpid(), port),
			"port %d is not a usable loopback port and must fail closed", port)
	}
}

// A root pid that names no real subtree must fail closed. 0 and negatives are
// not pids; 1 is init, which tclaude never launches a harness as.
func TestProcessSubtree_RejectsNonSubtreeRoots(t *testing.T) {
	for _, root := range []int{0, -1, 1} {
		assert.Empty(t, portowner.ProcessSubtree(root),
			"pid %d does not name a launched subtree", root)
	}
}

// The subtree always contains its own root, which is what lets a caller verify
// a harness that never forked.
func TestProcessSubtree_ContainsRoot(t *testing.T) {
	assert.Contains(t, portowner.ProcessSubtree(os.Getpid()), os.Getpid(),
		"a subtree must contain the process it is rooted at")
}
