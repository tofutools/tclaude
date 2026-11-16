package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// "Turn on Remote Access & open" (intent "open") on an agent whose Remote
// Access is already on but has no link recorded for this launch — its record
// predates the v342 upgrade, or was missed — switches it off and on, so
// Claude Code announces a fresh link and the follower records it
// (tk_9fbd0bd5). Before, "on" found it already on, changed nothing, and the
// dashboard waited 30 s for a link that never came.
func TestRemoteControlOpen_ArmedWithoutLinkCyclesForAFreshOne(t *testing.T) {
	f := newFlow(t)
	monitor := agentd.StartConvMonitorForTest(t, 10*time.Millisecond)
	if monitor == nil {
		t.Skip("no file watcher here")
	}
	agentd.WaitForConvMonitorStartupForTest(t, monitor)
	const conv = "019ec0b0-5555-6666-7777-888888888888"
	f.HaveAliveSession(conv, "cc-open", "tmux-cc-open", f.TestCwd("open"))

	link := func() string {
		t.Helper()
		sess, err := db.FindSessionByConvID(conv)
		require.NoError(t, err)
		return sess.RCSessionURL
	}
	linkIn := func(what string, not string) string {
		t.Helper()
		var got string
		require.Eventually(t, func() bool {
			got = link()
			return got != "" && got != not
		}, 10*time.Second, 20*time.Millisecond, what)
		return got
	}

	// Off, though the tracked flag says on: the cycle goes by the pane, not
	// the flag, so "open" just switches it on, like "on" (a cycle's disable
	// menu keys would land in the prompt of a pane that is off).
	t.Cleanup(agentd.SetRemoteControlLinkGraceForTest(0))
	require.NoError(t, db.SetSessionRemoteControl("cc-open", true))
	open := f.AsHuman().RemoteControl(conv, "open")
	require.Equal(t, http.StatusOK, open.Code, string(open.Raw))
	require.Equal(t, "enabled", open.Action)
	first := linkIn("the link Remote Access announced", "")
	require.True(t, strings.HasPrefix(first, db.RCSessionURLPrefix), first)

	// On, with its link: nothing to do.
	open = f.AsHuman().RemoteControl(conv, "open")
	require.Equal(t, http.StatusOK, open.Code, string(open.Raw))
	require.Equal(t, "noop", open.Action)

	// On, but the link is not recorded for this launch (as after the upgrade,
	// whose stamp outdates the record already in the transcript).
	sess, err := db.FindSessionByConvID(conv)
	require.NoError(t, err)
	require.NoError(t, db.ClearSessionRCLink(sess.ID))
	require.Empty(t, link())
	sim := f.World.CCs.GetByConvID(conv)
	require.NotNil(t, sim)
	require.True(t, sim.RemoteControlOn(), "the pane is armed")

	// Just switched, a link may still be on its way: no cycle yet.
	t.Cleanup(agentd.SetRemoteControlLinkGraceForTest(time.Minute))
	open = f.AsHuman().RemoteControl(conv, "open")
	require.Equal(t, http.StatusOK, open.Code, string(open.Raw))
	require.Equal(t, "noop", open.Action, "a second tap while a link may still come does not throw it away")

	// Long enough after, with no link come: off, then on, and a fresh link.
	t.Cleanup(agentd.SetRemoteControlLinkGraceForTest(0))
	open = f.AsHuman().RemoteControl(conv, "open")
	require.Equal(t, http.StatusOK, open.Code, string(open.Raw))
	require.Equal(t, "cycled", open.Action, string(open.Raw))
	require.True(t, open.RemoteControl)
	require.True(t, sim.RemoteControlOn(), "Remote Access ends on")
	fresh := linkIn("the fresh link from the cycle", first)
	require.True(t, strings.HasPrefix(fresh, db.RCSessionURLPrefix), fresh)

	st := f.AsHuman().RemoteControl(conv, "link")
	require.Equal(t, fresh, st.SessionURL, "the dashboard's poll gets the fresh link")
}
