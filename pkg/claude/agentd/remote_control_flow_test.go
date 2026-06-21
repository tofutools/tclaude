package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRemoteControl_ToggleOnOff drives the remote-control toggle end to end
// through the daemon mux against a live CC pane: enable → no-op → disable →
// toggle. It asserts BOTH surfaces — the persisted best-known state
// (sessions.remote_control, the source the dashboard + CLI read) and the
// `/remote-control` token actually reaching the pane.
func TestRemoteControl_ToggleOnOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec010-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-1", "tmux-cc-1", "/work")
	f.HaveMember("crew", conv)

	// Default: off until armed.
	st := f.AsHuman().RemoteControl(conv, "status")
	require.Equal(t, http.StatusOK, st.Code, "status; body=%s", st.Raw)
	assert.False(t, st.RemoteControl, "fresh session reports remote control off")
	assert.Equal(t, "status", st.Action)

	// Enable.
	on := f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, on.Code, "enable; body=%s", on.Raw)
	assert.True(t, on.RemoteControl, "enable reports remote control on")
	assert.Equal(t, "enabled", on.Action)
	f.AssertSentContains("tmux-cc-1:0.0", "/remote-control", 2*time.Second)
	got, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.True(t, got, "persisted state flips on after enable")

	// Enabling again is a no-op (best-known state already on).
	again := f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, again.Code, "re-enable; body=%s", again.Raw)
	assert.Equal(t, "noop", again.Action, "enabling an already-on session is a no-op")
	assert.True(t, again.RemoteControl)

	// Disable (exercises the confirm-Enter path).
	off := f.AsHuman().RemoteControl(conv, "off")
	require.Equal(t, http.StatusOK, off.Code, "disable; body=%s", off.Raw)
	assert.False(t, off.RemoteControl, "disable reports remote control off")
	assert.Equal(t, "disabled", off.Action)
	got, err = db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.False(t, got, "persisted state flips off after disable")

	// Toggle flips from off → on.
	tog := f.AsHuman().RemoteControl(conv, "toggle")
	require.Equal(t, http.StatusOK, tog.Code, "toggle; body=%s", tog.Raw)
	assert.True(t, tog.RemoteControl, "toggle flips off→on")
	assert.Equal(t, "enabled", tog.Action)
}

// TestRemoteControl_NoLiveSessionRefuses: a mutating intent needs a live pane;
// status still answers from the last-known state.
func TestRemoteControl_NoLiveSessionRefuses(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec011-1111-2222-3333-444444444444"
	// A member with no alive session (never spawned a pane).
	f.HaveMember("crew", conv)

	on := f.AsHuman().RemoteControl(conv, "on")
	assert.Equal(t, http.StatusServiceUnavailable, on.Code,
		"enable with no live pane is 503; body=%s", on.Raw)

	// status degrades gracefully rather than erroring.
	st := f.AsHuman().RemoteControl(conv, "status")
	assert.Equal(t, http.StatusOK, st.Code, "status with no live pane still answers; body=%s", st.Raw)
	assert.False(t, st.RemoteControl)
}

// TestRemoteControl_CodexUnsupported pins the harness gate: a harness with no
// built-in remote access (Codex → RemoteControlCommand "") refuses the toggle
// with a clear 409, the same way compact refuses a harness without /compact.
func TestRemoteControl_CodexUnsupported(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec012-1111-2222-3333-444444444444"
	f.HaveAliveCodexSession(conv, "cx-1", "tmux-cx-1", "/work")
	f.HaveMember("crew", conv)

	res := f.AsHuman().RemoteControl(conv, "on")
	assert.Equal(t, http.StatusConflict, res.Code,
		"Codex has no remote access; the toggle must refuse with 409; body=%s", res.Raw)
}
