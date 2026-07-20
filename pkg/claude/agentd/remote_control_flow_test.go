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
	f.AssertSentContains("tmux-cc-1:0.0", "/remote-control", 10*time.Second)
	got, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.True(t, got, "persisted state flips on after enable")

	// Real surface: the pane's modeled remote access is actually ON now, so the
	// disable below exercises the genuine "toggle while on opens a confirm
	// menu" path rather than a no-op.
	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "alive session has a CCSim")
	assert.True(t, cc.RemoteControlOn(), "the pane's modeled remote access is on after enable")

	// Enabling again is a no-op (best-known state already on).
	again := f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, again.Code, "re-enable; body=%s", again.Raw)
	assert.Equal(t, "noop", again.Action, "enabling an already-on session is a no-op")
	assert.True(t, again.RemoteControl)

	// paneKeys snapshots the send-keys that reached this pane, in order.
	paneKeys := func() []string {
		var keys []string
		for _, k := range f.World.Tmux.Sent() {
			if k.Target == "tmux-cc-1:0.0" {
				keys = append(keys, k.Text)
			}
		}
		return keys
	}
	beforeDisable := len(paneKeys())

	// Disable. This is the path that was silently (intermittently) broken:
	// routing the toggle through injectTextAndSubmit sent a SECOND,
	// belt-and-suspenders Enter that — when CC's confirm menu had already
	// rendered — landed on that menu and accepted its default ("keep
	// connected"), tearing it down before Up,Up,Enter could pick "disconnect",
	// so Remote Access stayed ON. The fix submits the toggle exactly once.
	off := f.AsHuman().RemoteControl(conv, "off")
	require.Equal(t, http.StatusOK, off.Code, "disable; body=%s", off.Raw)
	assert.False(t, off.RemoteControl, "disable reports remote control off")
	assert.Equal(t, "disabled", off.Action)
	got, err = db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.False(t, got, "persisted state flips off after disable")

	// Real surface: the pane's modeled remote access actually went OFF —
	// proving the injected keystrokes navigated the confirm menu to
	// "disconnect", not merely that the daemon optimistically recorded the
	// intended state. With the old double-Enter path the menu's default was
	// accepted and this stayed true.
	assert.False(t, cc.RemoteControlOn(),
		"disable must drive the confirm menu to disconnect, leaving the pane's remote access off")

	// And the exact keystrokes: type the toggle, submit ONCE, then Up, Up,
	// Enter to select "disconnect". A second Enter before the Ups is the
	// regression (it accepts the menu's default and leaves remote access on).
	disableKeys := paneKeys()[beforeDisable:]
	assert.Equal(t, []string{"/remote-control", "Enter", "Up", "Up", "Enter"}, disableKeys,
		"disable submits the toggle once then drives the menu (no stray belt-and-suspenders Enter)")

	// Toggle flips from off → on (enable opens no confirm menu).
	tog := f.AsHuman().RemoteControl(conv, "toggle")
	require.Equal(t, http.StatusOK, tog.Code, "toggle; body=%s", tog.Raw)
	assert.True(t, tog.RemoteControl, "toggle flips off→on")
	assert.Equal(t, "enabled", tog.Action)
	assert.True(t, cc.RemoteControlOn(), "the pane's modeled remote access is on again after the toggle")
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
