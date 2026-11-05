package agentd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// socketTmux points every tclaude tmux call at a private tmux server, so a
// test drives real tmux without ever touching the operator's `-L tclaude` one.
type socketTmux struct{ bin, socket string }

func (s socketTmux) Command(args ...string) *exec.Cmd {
	return exec.Command(s.bin, append([]string{"-S", s.socket}, args...)...)
}

func (s socketTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (s socketTmux) clients(t *testing.T, session string) []string {
	t.Helper()
	out, err := s.Command("list-clients", "-t", "="+session, "-F", "#{client_tty}").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// startPrivateTmux starts a detached session on a private socket and routes
// clcommon.Default to it for the test.
func startPrivateTmux(t *testing.T, session string) socketTmux {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	// Short and private: tmux socket paths are length-limited.
	dir, err := os.MkdirTemp("/tmp", "tcl-own-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	tm := socketTmux{bin: bin, socket: filepath.Join(dir, "s")}
	out, err := tm.Command("new-session", "-d", "-x", "80", "-y", "24", "-s", session, "sh", "-c", "while :; do sleep 1; done").CombinedOutput()
	require.NoError(t, err, "create tmux: %s", out)
	t.Cleanup(func() { _ = tm.Command("kill-server").Run() })
	prev := clcommon.Default
	clcommon.Default = tm
	t.Cleanup(func() { clcommon.Default = prev })
	return tm
}

// openWebTerminal serves one browser terminal attached (without -d) to session,
// tearing down through runPTYOverWS exactly as a dashboard window does.
func openWebTerminal(t *testing.T, tm socketTmux, session string) *websocket.Conn {
	t.Helper()
	attach := fmt.Sprintf("exec %s -S %s attach-session -t %s",
		shellSingleQuote(tm.bin), shellSingleQuote(tm.socket), shellSingleQuote("="+session))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runPTYOverWS(w, r, attach, session, "", nil, "")
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	sendResize(t, conn, 80, 24)
	return conn
}

// Closing one browser terminal detaches only its own tmux client. Two web
// windows on one session (or a tab that outlived its agent and redialed onto a
// new agent reusing the label) used to detach each other: each teardown
// dropped every client of the session.
func TestTermWS_TeardownDetachesOnlyItsOwnClient(t *testing.T) {
	const session = "own-client-probe"
	tm := startPrivateTmux(t, session)

	var teardowns atomic.Int32
	t.Cleanup(SetTermWSHookForTest(&TermWSHook{OnTeardown: func() { teardowns.Add(1) }}))

	first := openWebTerminal(t, tm, session)
	require.Eventually(t, func() bool { return len(tm.clients(t, session)) == 1 },
		5*time.Second, 20*time.Millisecond, "first web terminal never attached")
	firstTTY := tm.clients(t, session)[0]
	openWebTerminal(t, tm, session)
	require.Eventually(t, func() bool { return len(tm.clients(t, session)) == 2 },
		5*time.Second, 20*time.Millisecond, "second web terminal never attached")

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return teardowns.Load() == 1 },
		5*time.Second, 20*time.Millisecond, "closed terminal never tore down")

	remaining := tm.clients(t, session)
	assert.Len(t, remaining, 1, "closing one web terminal must leave the other attached")
	assert.NotContains(t, remaining, firstTTY, "the closed terminal's own client must be gone")
}

// A teardown whose client is already gone (displaced by another attach, or
// exited) detaches nothing, and a blank session issues no tmux command.
func TestDetachOwnTmuxClient_GoneClientAndBlankSession(t *testing.T) {
	const session = "own-client-gone"
	tm := startPrivateTmux(t, session)
	openWebTerminal(t, tm, session)
	require.Eventually(t, func() bool { return len(tm.clients(t, session)) == 1 },
		5*time.Second, 20*time.Millisecond, "web terminal never attached")

	detachOwnTmuxClient(session, "/dev/pts/not-a-client")
	assert.Len(t, tm.clients(t, session), 1, "a tty that is not attached must not detach anyone")

	rec := &detachRecTmux{}
	clcommon.Default = rec
	detachOwnTmuxClient("", "/dev/pts/1")
	assert.Empty(t, rec.calls, "blank session must issue no tmux command")
}
