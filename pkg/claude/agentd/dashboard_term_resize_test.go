package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests exercise runPTYOverWS against a real PTY and a real WebSocket,
// with `sh` scripts that report what size their tty actually had. They guard
// the startup-ordering contract behind the dashboard's "terminal attaches at
// minimal width until the first browser resize" bug: the command must be born
// at the client's size (never 0x0), and a resize message must re-sync a client
// even when the kernel considers it a no-op.

// dialPTYWS serves runPTYOverWS(script) on a throwaway HTTP server and returns
// a connected client-side WebSocket.
func dialPTYWS(t *testing.T, script string) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runPTYOverWS(w, r, script, "", "", nil)
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial terminal websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendResize(t *testing.T, conn *websocket.Conn, cols, rows int) {
	t.Helper()
	raw, err := json.Marshal(termResizeMsg{Type: "resize", Cols: cols, Rows: rows})
	if err != nil {
		t.Fatalf("marshal resize: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("send resize: %v", err)
	}
}

// collectUntil reads terminal output until want appears (returning everything
// read) or the deadline passes (failing with what WAS read).
func collectUntil(t *testing.T, conn *websocket.Conn, want string, timeout time.Duration) string {
	t.Helper()
	var output strings.Builder
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(output.String(), want) {
			return output.String()
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q in terminal output, got %q (read: %v)", want, output.String(), err)
		}
		output.Write(data)
	}
}

// The PTY command must observe the client's size the moment it starts: the
// bridge holds the command back until the opening resize message has arrived
// and starts the PTY at that size. A command that reads its tty size first
// thing — as a starting tmux client does — must never see 0x0 or a default.
func TestTermWS_CommandStartsAtTheClientsInitialSize(t *testing.T) {
	conn := dialPTYWS(t, "stty size")
	sendResize(t, conn, 101, 31)
	collectUntil(t, conn, "31 101", 5*time.Second)
}

// A client that never reports a size (died mid-handshake, pre-resize protocol)
// still gets its command started — at 80x24, the least-wrong fallback — after
// a bounded wait, never on a 0x0 PTY.
func TestTermWS_SilentClientGetsDefaultSizeAfterBoundedWait(t *testing.T) {
	restore := initialResizeWait
	initialResizeWait = 100 * time.Millisecond
	t.Cleanup(func() { initialResizeWait = restore })

	conn := dialPTYWS(t, "stty size")
	collectUntil(t, conn, "24 80", 5*time.Second)
}

// Re-sending an UNCHANGED size must still reach the command as a SIGWINCH.
// The kernel only raises SIGWINCH when TIOCSWINSZ changes the size, but the
// dashboard's post-open refit re-sends the same grid precisely to repair a
// client that missed the original signal during startup — so the bridge
// delivers the signal explicitly on every applied resize.
func TestTermWS_SameSizeResizeStillDeliversSIGWINCH(t *testing.T) {
	script := `trap 'echo WINCHED' WINCH; stty size; i=0; while [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done`
	conn := dialPTYWS(t, script)
	sendResize(t, conn, 101, 31)
	collectUntil(t, conn, "31 101", 5*time.Second)

	// Same size again: Setsize is a kernel-level no-op, the explicit group
	// signal is what the trap must observe.
	sendResize(t, conn, 101, 31)
	collectUntil(t, conn, "WINCHED", 5*time.Second)
}
