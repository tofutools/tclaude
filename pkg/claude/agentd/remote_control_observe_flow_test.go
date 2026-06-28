package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRemoteControl_StatusObservesPaneAndSelfHeals proves the readback path:
// `status` reads the live pane's /rc footer pill rather than echoing the
// tracked flag, so it stays correct after a human toggles remote control
// directly in the pane — and it self-heals the drifted tracked flag.
func TestRemoteControl_StatusObservesPaneAndSelfHeals(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec0a0-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-1", "tmux-cc-1", "/work")
	f.HaveMember("crew", conv)

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc, "alive session has a CCSim")

	// Baseline: tracked flag off and the pane shows no pill — status observes
	// "off" from the pane and agrees.
	st := f.AsHuman().RemoteControl(conv, "status")
	require.Equal(t, http.StatusOK, st.Code, "status; body=%s", st.Raw)
	assert.False(t, st.RemoteControl)
	assert.Equal(t, "off", st.Observed, "status reads the pane")
	assert.Equal(t, "pane", st.Source)

	// Human arms remote control DIRECTLY IN THE PANE, bypassing tclaude — the
	// exact drift case the old best-known flag could not see.
	cc.Receive("/remote-control")
	cc.Receive("Enter")
	require.True(t, cc.RemoteControlOn(), "in-pane toggle armed the pane")
	tracked, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	require.False(t, tracked, "tracked flag is stale/off — it drifted from the pane")

	// status now reads the pane, reports ON (the answer to "can I connect"),
	// surfaces the claude.ai/code link, and self-heals the tracked flag.
	st = f.AsHuman().RemoteControl(conv, "status")
	require.Equal(t, http.StatusOK, st.Code, "status; body=%s", st.Raw)
	assert.True(t, st.RemoteControl, "status observes the armed pane, not the stale flag")
	assert.Equal(t, "on", st.Observed)
	assert.Equal(t, "pane", st.Source)
	assert.Contains(t, st.SessionURL, "claude.ai/code/", "armed pill carries the connect link")

	healed, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.True(t, healed, "tracked flag self-healed to match the pane")
}

// TestRemoteControl_ToggleDirectionFromObservedState proves on/off/toggle pick
// their direction from the OBSERVED pane, not a drifted tracked flag: with the
// pane armed (in-pane) but the tracked flag still off, `on` is correctly a
// no-op instead of re-injecting the toggle.
func TestRemoteControl_ToggleDirectionFromObservedState(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec0a1-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-1", "tmux-cc-1", "/work")
	f.HaveMember("crew", conv)

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)

	// Arm in-pane so tracked=off drifts from pane=on.
	cc.Receive("/remote-control")
	cc.Receive("Enter")
	require.True(t, cc.RemoteControlOn())
	tracked, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	require.False(t, tracked, "tracked flag still off (drift)")

	// `on` would, on the stale flag alone, re-inject the toggle (and so
	// DISABLE the already-on pane). Reading the pane first makes it a no-op.
	on := f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, on.Code, "on; body=%s", on.Raw)
	assert.Equal(t, "noop", on.Action, "on is a no-op — the observed pane is already armed")
	assert.True(t, on.RemoteControl)
	assert.True(t, cc.RemoteControlOn(), "the pane stayed armed (no stray toggle re-injected)")

	healed, err := db.RemoteControlForConv(conv)
	require.NoError(t, err)
	assert.True(t, healed, "the direction read self-healed the tracked flag")
}

// TestRemoteControl_StatusReportsFailedPill proves the failed branch: an armed
// pane whose connection failed renders a red /rc pill, which status reports as
// observed="failed" (still armed, but not reachable).
func TestRemoteControl_StatusReportsFailedPill(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec0a2-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-1", "tmux-cc-1", "/work")
	f.HaveMember("crew", conv)

	cc := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, cc)

	// Arm through tclaude, then model the relay connection failing.
	on := f.AsHuman().RemoteControl(conv, "on")
	require.Equal(t, http.StatusOK, on.Code, "on; body=%s", on.Raw)
	require.True(t, cc.RemoteControlOn())
	cc.SetRemoteControlFailed(true)

	st := f.AsHuman().RemoteControl(conv, "status")
	require.Equal(t, http.StatusOK, st.Code, "status; body=%s", st.Raw)
	assert.Equal(t, "failed", st.Observed, "red pill reads as failed")
	assert.True(t, st.RemoteControl, "failed is still armed (toggled on)")
	assert.Contains(t, st.Note, "FAILED", "note explains it's not reachable")
}
