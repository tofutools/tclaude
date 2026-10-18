package agentd_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A restricted peer lists only the background terminals its grants open: the
// shells of groups granting sessions.shell and the agents of groups granting a
// terminal. None is killable from there, and the kill route stays refused.
func TestPeerView_BackgroundTerminalsFollowGrants(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	member := func(group, conv, title string) {
		f.HaveGroup(group)
		f.HaveConvWithTitle(conv, title)
		f.HaveMember(group, conv)
		f.HaveAliveSession(conv, "spwn-"+title, "tmux-"+title, f.TestCwd(title))
	}
	member("shells", "pbg-shell-conv", "sheller")
	member("watched", "pbg-watch-conv", "watcher")
	member("hidden", "pbg-hide-conv", "hider")
	shellOf := func(conv, group string) string {
		sum := sha256.Sum256([]byte(conv + "\x00" + group))
		return fmt.Sprintf("tclaude-term-%x-current", sum[:8])
	}
	groupShell := func(group string) string {
		sum := sha256.Sum256([]byte(group))
		return fmt.Sprintf("tclaude-groupterm-%x", sum[:8])
	}
	for _, name := range []string{shellOf("pbg-shell-conv", "shells"), groupShell("shells"), shellOf("pbg-hide-conv", "hidden"), groupShell("hidden")} {
		f.World.Tmux.MarkAlive(name)
	}
	for slug, group := range map[string]string{agentd.PermSessionsShell: "shells", agentd.PermSessionsWatch: "watched"} {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}

	h := agentd.PeerViewHandler(fh.peer.id.ID())
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/background-terminals", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Terminals []backgroundTerminalRow `json:"terminals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	listed := map[string]backgroundTerminalRow{}
	for _, row := range out.Terminals {
		listed[row.TmuxSession] = row
		require.False(t, row.Killable, "nothing is killable for a restricted peer: %s", row.TmuxSession)
	}
	require.Len(t, listed, 3, "%v", listed)
	require.Equal(t, "shell", listed[shellOf("pbg-shell-conv", "shells")].Kind)
	require.Equal(t, "group_shell", listed[groupShell("shells")].Kind)
	require.Equal(t, "agent", listed["tmux-watcher"].Kind, "a terminal grant lists the group's agents")

	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/background-terminals/kill", map[string]any{"tmux_session": groupShell("shells")}))
	require.Equal(t, 403, rec.Code)
}
