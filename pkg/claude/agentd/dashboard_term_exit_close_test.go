package agentd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// A browser terminal whose command exits on its own says why in the close
// frame, and whether dashboard.close_terminal_tabs wants the tab closed.
func TestBrowserTerminalExitCloseCode(t *testing.T) {
	no := false
	yes := true
	cases := []struct {
		name   string
		target config.TerminalTabTarget
		alive  bool
		cfg    *config.Config
		want   int
	}{
		{"group shell ended closes by default", config.TerminalTabGroupTerminal, false, nil, termCloseSessionEndedClose},
		{"agent dir shell ended closes by default", config.TerminalTabAgentTerminal, false, nil, termCloseSessionEndedClose},
		{"agent window ended keeps by default", config.TerminalTabAgentWindow, false, nil, termCloseSessionEndedKeep},
		{"detach keeps by default", config.TerminalTabGroupTerminal, true, nil, termCloseDetachedKeep},
		{"opt-out keeps an ended shell", config.TerminalTabGroupTerminal, false, &config.Config{Dashboard: &config.DashboardConfig{
			CloseTerminalTabs: &config.TerminalTabCloseConfig{SessionEnded: &config.TerminalTabCloseTargets{GroupTerminal: &no}},
		}}, termCloseSessionEndedKeep},
		{"opt-in closes on detach", config.TerminalTabAgentWindow, true, &config.Config{Dashboard: &config.DashboardConfig{
			CloseTerminalTabs: &config.TerminalTabCloseConfig{Detached: &config.TerminalTabCloseTargets{AgentWindow: &yes}},
		}}, termCloseDetachedClose},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := termWSTestHook
			termWSTestHook = &TermWSHook{
				SessionAlive: func(string) bool { return tc.alive },
				Config:       func() *config.Config { return tc.cfg },
			}
			t.Cleanup(func() { termWSTestHook = previous })
			if got := dialExitingTerminal(t, tc.target); got != tc.want {
				t.Fatalf("close code = %d, want %d", got, tc.want)
			}
		})
	}
}

// Routes without a terminal kind (the TUI page) keep the plain close.
func TestBrowserTerminalExitWithoutTargetClosesPlainly(t *testing.T) {
	previous := termWSTestHook
	termWSTestHook = &TermWSHook{SessionAlive: func(string) bool { return false }}
	t.Cleanup(func() { termWSTestHook = previous })
	if got := dialExitingTerminal(t, ""); got >= 4000 {
		t.Fatalf("close code = %d, want no terminal-exit code", got)
	}
}

// A command killed from outside — systemd SIGTERMing agentd's whole cgroup on a
// restart, tmux clients included — is not a detach or a session end: the plain
// close keeps the browser's restart watch, so the terminal reconnects.
func TestBrowserTerminalKilledCommandClosesPlainly(t *testing.T) {
	previous := termWSTestHook
	termWSTestHook = &TermWSHook{SessionAlive: func(string) bool { return true }}
	t.Cleanup(func() { termWSTestHook = previous })
	for name, command := range map[string]string{
		"tmux client SIGTERM exit": "printf '[terminated]\\n'; exit 1",
		"wrapper killed by signal": "printf '[terminated]\\n'; kill -TERM $$",
	} {
		t.Run(name, func(t *testing.T) {
			if got := dialTerminalCommand(t, command, config.TerminalTabAgentWindow); got >= 4000 {
				t.Fatalf("close code = %d, want no terminal-exit code", got)
			}
		})
	}
}

func dialExitingTerminal(t *testing.T, target config.TerminalTabTarget) int {
	t.Helper()
	return dialTerminalCommand(t, "printf 'BYE\\n'", target)
}

func dialTerminalCommand(t *testing.T, command string, target config.TerminalTabTarget) int {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		runPTYOverWS(w, r, command, "tclaude-term-test", "", nil, target)
	}))
	t.Cleanup(func() { server.Close(); <-done })
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":80,"rows":24}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr.Code
			}
			return 0
		}
	}
}
