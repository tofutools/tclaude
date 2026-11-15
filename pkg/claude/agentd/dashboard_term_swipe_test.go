package agentd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// swipeTmux records every tmux command and runs none of them.
type swipeTmux struct {
	mu   sync.Mutex
	runs []string
}

func (r *swipeTmux) Command(args ...string) *exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, strings.Join(args, " "))
	return exec.Command("true")
}

func (r *swipeTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func (r *swipeTmux) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runs...)
}

// A phone swipe over a Claude Code pane is the desktop wheel: xterm sends the
// pane SGR wheel reports, which reach Claude Code's fullscreen TUI byte for
// byte so it scrolls its own history. agentd never turns tmux mouse on for
// the pane, never enters copy-mode, and drops a scroll frame from a phone tab
// still running the build that asked agentd to scroll the pane.
func TestBrowserTerminalSwipeReachesClaudeCodeAsTheWheel(t *testing.T) {
	claude, ok := harness.Get("claude")
	if !ok || claude.WantsTmuxScrollback() {
		t.Fatal("Claude Code must keep its own scrollback (tmux mouse off)")
	}
	fake := &swipeTmux{}
	previousTmux := common.Default
	common.Default = fake
	t.Cleanup(func() { common.Default = previousTmux })

	done := make(chan struct{})
	started := make(chan *os.Process, 1)
	previous := termWSTestHook
	termWSTestHook = &TermWSHook{OnStart: func(p *os.Process) { started <- p }}
	t.Cleanup(func() { termWSTestHook = previous })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		runPTYOverWS(w, r, "stty raw -echo; printf 'READY'; exec cat", "", "sess-cc", claude, "")
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	send := func(kind int, s string) {
		t.Helper()
		if err := conn.WriteMessage(kind, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	// A stale scroll frame before the command starts is dropped too.
	send(websocket.TextMessage, `{"type":"scroll","op":"up","notches":1}`)
	send(websocket.TextMessage, `{"type":"resize","cols":80,"rows":24}`)
	proc := <-started
	t.Cleanup(func() { _ = syscall.Kill(-proc.Pid, syscall.SIGKILL); <-done })

	var output strings.Builder
	read := func(what, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(output.String(), want) {
			_ = conn.SetReadDeadline(deadline)
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for %s: %v (output %q)", what, err, output.String())
			}
			output.Write(data)
		}
	}
	read("READY", "READY")

	// Three notches back and one forward, as xterm encodes a wheel while the
	// application has asked for SGR mouse reporting; then a stale scroll
	// frame, then a key.
	const wheel = "\x1b[<64;40;12M\x1b[<64;40;12M\x1b[<64;40;12M\x1b[<65;40;12M"
	for i := 0; i < len(wheel); i += len("\x1b[<64;40;12M") {
		send(websocket.TextMessage, wheel[i:i+len("\x1b[<64;40;12M")])
	}
	send(websocket.TextMessage, `{"type":"scroll","op":"live"}`)
	send(websocket.TextMessage, "1")
	// Read until the whole sequence is in: the wheel reports themselves
	// contain a "1" (";12M"), so waiting for the key alone can stop before
	// the key has arrived.
	read("the wheel reports and the key", wheel+"1")
	if got := strings.TrimPrefix(output.String(), "READY"); got != wheel+"1" {
		t.Fatalf("the pane read %q, want the wheel reports and the key %q", got, wheel+"1")
	}

	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	for _, run := range fake.snapshot() {
		if strings.Contains(run, "mouse") || strings.Contains(run, "copy-mode") || strings.Contains(run, "-X") {
			t.Fatalf("tmux ran %q: a Claude Code pane's mouse and copy-mode are not agentd's to change (all: %q)", run, fake.snapshot())
		}
	}
}
