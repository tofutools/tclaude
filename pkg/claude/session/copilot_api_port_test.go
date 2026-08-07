package session

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agentd case: the daemon allocated the port before forking this process
// precisely so it holds the number first, so `session new` must take it as
// given rather than choosing again.
func TestResolveCopilotAPIPort_KeepsTheSuppliedPort(t *testing.T) {
	port, err := ResolveCopilotAPIPort(true, 4599)
	require.NoError(t, err)
	assert.Equal(t, 4599, port,
		"a port supplied by the daemon must survive unchanged; re-allocating "+
			"would hand the pane a port nobody is watching")
}

// The direct `tclaude session new --copilot-api` case: no daemon is waiting on
// the number, so choosing it here is safe and the pane still gets a server.
func TestResolveCopilotAPIPort_AllocatesWhenUnset(t *testing.T) {
	port, err := ResolveCopilotAPIPort(true, 0)
	require.NoError(t, err)
	assert.Positive(t, port, "the API drive without a port must allocate one")
	assert.LessOrEqual(t, port, 65535)

	// It must be a port that can actually be bound — the allocator releases it
	// again, which is the bind-close-exec gap this design accepts knowingly.
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err, "the allocated port must be free after allocation")
	require.NoError(t, listener.Close())
}

// A port without the drive is refused rather than dropped. Silently ignoring it
// would leave the caller believing they configured a channel that was never
// built: the pane would run on send-keys with no embedded server at all.
func TestResolveCopilotAPIPort_RefusesPortWithoutTheDrive(t *testing.T) {
	_, err := ResolveCopilotAPIPort(false, 4599)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--copilot-api",
		"the refusal must name the flag that is missing")
}

// The ordinary send-keys launch, and every non-Copilot harness: no drive, no
// port, no change to anything.
func TestResolveCopilotAPIPort_QuietWithoutEither(t *testing.T) {
	port, err := ResolveCopilotAPIPort(false, 0)
	require.NoError(t, err)
	assert.Zero(t, port)
}

// A supplied port outside the TCP range is a caller error, not something to
// clamp or re-allocate around: whatever produced it is wrong, and quietly
// substituting a different number would hide that.
func TestResolveCopilotAPIPort_RejectsOutOfRangePorts(t *testing.T) {
	for _, port := range []int{-1, 65536, 1 << 20} {
		_, err := ResolveCopilotAPIPort(true, port)
		assert.Error(t, err, "port %d is not usable and must be refused", port)
	}
}
