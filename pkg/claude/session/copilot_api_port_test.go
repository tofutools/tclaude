package session

import (
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

// The INVERSION of this file's previous TestResolveCopilotAPIPort_AllocatesWhenUnset,
// which asserted that the drive without a port allocated one here so that "the
// pane still gets a server". The pane did get a server. Nothing got a drive: only
// agentd creates the RPC session and holds the connection, so a locally chosen
// port bound an unauthenticated loopback endpoint nothing would ever dial
// (TCL-1084).
//
// The old test is the reason this one names the whole reasoning rather than just
// asserting an error: an assertion that a port is allocated looks exactly as
// reasonable as an assertion that it is refused, and what decides between them is
// who is on the other end.
func TestResolveCopilotAPIPort_RefusesTheDriveWithoutAPort(t *testing.T) {
	_, err := ResolveCopilotAPIPort(true, 0)
	require.Error(t, err,
		"the drive with no daemon-allocated port must be refused, not allocated around")
	assert.Contains(t, err.Error(), "tclaude agentd",
		"the refusal must name what is actually missing — the daemon, not the number")
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
