package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// A peer opens this node's tclaude shells (a group's, a member's directory
// shell) only under sessions.shell on the group: terminal grants do not reach
// a shell, and a granted open lands in the very tmux session a local click
// would.
func TestFederation_TerminalShellOpenNeedsShellGrant(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const conv = "fed-terminal-shell"
	f.HaveGroup("team")
	_, err := db.SetAgentGroupDefaultCwd("team", f.TestCwd("team"))
	require.NoError(t, err)
	f.HaveConvWithTitle(conv, "shelly")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "shell-runtime", "tclaude-shell-runtime", f.TestCwd("work"))
	f.HaveGroup("other")
	f.HaveConvWithTitle("fed-terminal-outsider", "outsider")
	f.HaveMember("other", "fed-terminal-outsider")
	f.HaveAliveSession("fed-terminal-outsider", "outsider-runtime", "tclaude-outsider-runtime", f.TestCwd("out"))
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	outsider, err := db.AgentIDForConv("fed-terminal-outsider")
	require.NoError(t, err)
	original := clcommon.Default
	mock := &terminalTmux{Tmux: original, options: map[string]string{}, pane: "%1", windows: "1", version: "tmux 3.4"}
	clcommon.Default = mock
	t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })

	// open sends one open; a granted one is attached and read once, as a
	// viewer does, before the next.
	open := func(shell, agent string) proto.SessionAnswerPayload {
		t.Helper()
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		sid := proto.NewEnvelopeID()
		p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, proto.SessionOpenPayload{Shell: shell, Agent: agent, Group: "team", Stream: sid, Key: kp.Pub, Cols: 80, Rows: 24}))
		ans := terminalAnswer(t, p, sid)
		if ans.OK {
			c := fedPeerStream(t, p, sid, kp, ans.Key, true)
			_ = terminalRead(t, c)
			_ = c.Close()
		}
		return ans
	}

	attach := map[string]any{"peer": "bob", "slug": agentd.PermSessionsAttach, "scope": "group=team"}
	require.Equal(t, 200, fedHuman(t, f, http.MethodPost, "/v1/federation/grants", attach).Code)
	ans := open(proto.ShellGroup, "")
	require.False(t, ans.OK, "a terminal grant does not reach a shell")
	require.Contains(t, ans.Reason, agentd.PermSessionsShell)

	shell := map[string]any{"peer": "bob", "slug": agentd.PermSessionsShell, "scope": "group=team"}
	require.Equal(t, 200, fedHuman(t, f, http.MethodPost, "/v1/federation/grants", shell).Code)
	require.False(t, open(proto.ShellCurrent, outsider).OK, "an agent outside the granted group has no shell here")
	ans = open(proto.ShellGroup, "")
	require.True(t, ans.OK, ans.Reason)
	_, _, attached := mock.snapshot()
	require.True(t, strings.Contains(strings.Join(attached, " "), "tclaude-groupterm-"), "the group's own shell: %v", attached)

	ans = open(proto.ShellCurrent, aid)
	require.True(t, ans.OK, ans.Reason)
	_, _, attached = mock.snapshot()
	require.True(t, strings.Contains(strings.Join(attached, " "), "tclaude-term-"), "the member's directory shell: %v", attached)

}
