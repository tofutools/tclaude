package agentd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Closing the browser must release a PTY whose child is idle in a read. A
// blocking master read must not defer the child hangup until after wg.Wait.
func TestBrowserTerminalDisconnectReapsIdlePTY(t *testing.T) {
	done := make(chan struct{})
	started := make(chan *os.Process, 1)
	previous := termWSTestHook
	termWSTestHook = &TermWSHook{OnStart: func(p *os.Process) { started <- p }}
	t.Cleanup(func() { termWSTestHook = previous })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		runPTYOverWS(w, r, "printf 'READY\\n'; exec cat", "", "", nil)
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":80,"rows":24}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	proc := <-started
	// Ensure a failed regression never leaves a child or handler behind.
	t.Cleanup(func() { _ = syscall.Kill(-proc.Pid, syscall.SIGKILL); <-done })
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("browser disconnect left idle PTY and handler running")
	}
}
