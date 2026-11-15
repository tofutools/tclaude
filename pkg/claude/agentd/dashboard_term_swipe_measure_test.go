package agentd

import (
	"context"
	"fmt"
	"net"
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
)

// TestMeasureTerminalSwipe measures what one wheel notch over a Claude Code
// like fullscreen view costs on the wire, through a real tmux client and the
// real runPTYOverWS: bytes, WebSocket messages, and how long the redraw takes
// to arrive. It runs testdata/swipe-tui.py in a private tmux server (its own
// socket directory, never the tclaude one). Gated, as it needs tmux and
// python3 and only prints numbers:
//
//	TCLAUDE_SWIPE_MEASURE=1 go test ./pkg/claude/agentd/ -run TestMeasureTerminalSwipe -v
func TestMeasureTerminalSwipe(t *testing.T) {
	if os.Getenv("TCLAUDE_SWIPE_MEASURE") == "" {
		t.Skip("set TCLAUDE_SWIPE_MEASURE=1 to measure a terminal swipe")
	}
	for _, bin := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s", bin)
		}
	}
	tui, err := filepath.Abs("testdata/swipe-tui.py")
	if err != nil {
		t.Fatal(err)
	}
	// A unix socket path is short: under os.TempDir, not the test's own dir.
	dir, err := os.MkdirTemp("", "swipe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	tmux := func(args ...string) *exec.Cmd {
		cmd := exec.Command("tmux", append([]string{"-u", "-S", sock, "-f", "/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), "TMUX=")
		return cmd
	}
	if out, err := tmux("new-session", "-d", "-s", "m", "-x", "46", "-y", "38", "python3 "+tui).CombinedOutput(); err != nil {
		t.Fatalf("tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = tmux("kill-server").Run() })

	for _, size := range [][2]int{{46, 38}, {160, 50}} {
		for _, c := range []struct {
			name     string
			features string
			compress bool
		}{
			{"plain", "", false},
			{"sync", "-T sync ", false},
			{"deflate", "", true},
			{"sync+deflate", "-T sync ", true},
		} {
			t.Run(fmt.Sprintf("%s-%dx%d", c.name, size[0], size[1]), func(t *testing.T) {
				attach := fmt.Sprintf("TMUX= exec tmux -u -S %s %sattach-session -f ignore-size -t m", sock, c.features)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					runPTYOverWS(w, r, attach, "", "", nil, "")
				}))
				defer server.Close()
				var wire atomic.Int64
				dialer := websocket.Dialer{EnableCompression: c.compress, NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return &countingConn{Conn: conn, read: &wire}, nil
				}}
				conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				negotiated := resp.Header.Get("Sec-WebSocket-Extensions")
				if err := conn.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"type":"resize","cols":%d,"rows":%d}`, size[0], size[1])); err != nil {
					t.Fatal(err)
				}
				messages := make(chan time.Time, 1024)
				go func() {
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							close(messages)
							return
						}
						messages <- time.Now()
					}
				}()
				quiet := func(gap time.Duration) (n int, last time.Time) {
					for {
						select {
						case at, ok := <-messages:
							if !ok {
								return n, last
							}
							n, last = n+1, at
						case <-time.After(gap):
							return n, last
						}
					}
				}
				quiet(500 * time.Millisecond)
				const notches = 20
				var bytes, msgs int64
				var took time.Duration
				for range notches {
					before := wire.Load()
					sent := time.Now()
					if err := conn.WriteMessage(websocket.BinaryMessage, fmt.Appendf(nil, "\x1b[<64;10;10M")); err != nil {
						t.Fatal(err)
					}
					n, last := quiet(150 * time.Millisecond)
					bytes += wire.Load() - before
					msgs += int64(n)
					took += last.Sub(sent)
				}
				t.Logf("%3dx%-3d %-13s ext=%q  %5d wire bytes/notch  %.1f messages/notch  %v to the last byte (loopback)",
					size[0], size[1], c.name, negotiated, bytes/notches, float64(msgs)/notches, (took / notches).Round(10*time.Microsecond))
			})
		}
	}
}

type countingConn struct {
	net.Conn
	read *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}
