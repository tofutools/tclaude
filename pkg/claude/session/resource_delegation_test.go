package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// insideTclaudeTmuxServer decides whether the -N "never start a server" guard
// is applied, so a false negative is a fail-open: the caller silently creates a
// second tmux server instead of failing with a clear error. It has to hold up
// against a configured tmux.socket_name, including from a sandboxed agent that
// cannot read the config that names it.
func TestInsideTclaudeTmuxServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// tmuxEnv is $TMUX as tmux sets it inside a pane.
		tmuxEnv string
		// socketName is what THIS process resolves tmux.socket_name to.
		socketName string
		// socketEnv marks a process carrying the agentd socket variable —
		// every sandboxed agent, and also a daemon started with --socket.
		socketEnv bool
		// configReadable distinguishes those two: only the sandboxed agent is
		// actually blind to the config naming the socket.
		configReadable bool
		want           bool
	}{
		{name: "outside tmux", tmuxEnv: "", socketName: "tclaude"},
		{
			name: "default socket", tmuxEnv: "/tmp/tmux-1000/tclaude,123,0",
			socketName: "tclaude", want: true,
		},
		{
			name: "configured socket", tmuxEnv: "/tmp/tmux-1000/work-2,123,0",
			socketName: "work-2", want: true,
		},
		{
			name: "operator's own tmux server", tmuxEnv: "/tmp/tmux-1000/default,123,0",
			socketName: "tclaude", want: false,
		},
		{
			// The agent resolves "tclaude" because ~/.tclaude/data is denied to
			// it, while its pane actually lives on "work-2". Comparing names
			// says no; the pane says otherwise, so fail closed.
			name: "sandboxed agent on a configured socket", tmuxEnv: "/tmp/tmux-1000/work-2,123,0",
			socketName: "tclaude", socketEnv: true, want: true,
		},
		{
			// Same fail-closed rule must not fire when there is no pane at all.
			name: "sandboxed agent outside tmux", tmuxEnv: "",
			socketName: "tclaude", socketEnv: true, want: false,
		},
		{
			// A daemon started with --socket carries the same variable but can
			// read its config, so its resolved name is trustworthy and the
			// foreign socket really is foreign. Failing closed here would add
			// -N to a daemon that legitimately starts tclaude's tmux server.
			name: "daemon with a custom socket inside a foreign tmux",
			tmuxEnv: "/tmp/tmux-1000/default,123,0", socketName: "tclaude",
			socketEnv: true, configReadable: true, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if tc.configReadable {
				dir := filepath.Join(home, ".tclaude", "data")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "config.json"), []byte(`{}`), 0o600))
			}
			t.Setenv("TMUX", tc.tmuxEnv)
			socketEnv := ""
			if tc.socketEnv {
				socketEnv = filepath.Join(home, ".tclaude", "api", "agentd.sock")
			}
			t.Setenv(agentipc.SocketEnv, socketEnv)
			defer clcommon.SetTmuxSocketNameForTest(tc.socketName)()

			assert.Equal(t, tc.want, insideTclaudeTmuxServer())
		})
	}
}
