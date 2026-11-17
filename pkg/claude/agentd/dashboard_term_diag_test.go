package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A terminal's Diagnostics overlay turns on a log of its socket with a
// {"type":"diag"} frame, which never reaches the session; the log has each
// frame sent and each input that arrived, read from /api/terminal-diag.
func TestBrowserTerminalDiagnosticsLogTheSocketWithoutTouchingTheStream(t *testing.T) {
	claude, _ := harness.Get("claude")
	done := make(chan struct{})
	started := make(chan *os.Process, 1)
	previous := termWSTestHook
	termWSTestHook = &TermWSHook{OnStart: func(p *os.Process) { started <- p }}
	t.Cleanup(func() { termWSTestHook = previous })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		runPTYOverWS(w, r, "stty raw -echo; printf 'READY'; exec cat", "", "sess-diag", claude, "")
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	send := func(s string) {
		t.Helper()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	// On before the command starts, as a reconnecting overlay sends it.
	send(`{"type":"diag","id":"diag_one"}`)
	send(`{"type":"resize","cols":80,"rows":24}`)
	proc := <-started
	t.Cleanup(func() { _ = syscall.Kill(-proc.Pid, syscall.SIGKILL); <-done })

	var output strings.Builder
	read := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(output.String(), want) {
			_ = conn.SetReadDeadline(deadline)
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for %q: %v (output %q)", want, err, output.String())
			}
			output.Write(data)
		}
	}
	read("READY")
	send("ab")
	send(`{"type":"diag","id":"diag_one"}`)
	send("c")
	read("abc")
	if got := strings.TrimPrefix(output.String(), "READY"); got != "abc" {
		t.Fatalf("the session read %q: a diag frame must never be typed into it", got)
	}

	poll := func(since int64) (int, []termDiagEvent) {
		t.Helper()
		rec := httptest.NewRecorder()
		handleTerminalDiag(rec, httptest.NewRequest(http.MethodGet, "/api/terminal-diag?id=diag_one&since="+strconv.FormatInt(since, 10), nil))
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var body struct {
			NowMS  float64         `json:"now_ms"`
			Events []termDiagEvent `json:"events"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.NowMS == 0 {
			t.Fatalf("bad body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, body.Events
	}
	// The second diag frame restarted the log: it holds "c" in and its echo out.
	deadline := time.Now().Add(5 * time.Second)
	var events []termDiagEvent
	for {
		_, events = poll(0)
		var in, out int
		for _, ev := range events {
			if ev.Kind == "in" {
				in += ev.Bytes
			}
			if ev.Kind == "out" {
				out += ev.Bytes
			}
		}
		if in == 1 && out >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log has %+v, want one input byte and its echo", events)
		}
		time.Sleep(10 * time.Millisecond)
	}
	last := events[len(events)-1].Seq
	if _, newer := poll(last); len(newer) != 0 {
		t.Fatalf("since the last seq there should be nothing new, got %+v", newer)
	}

	send(`{"type":"diag","id":""}`)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if code, _ := poll(0); code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turning diagnostics off should drop the log")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A reconnect's socket takes the log over: the old socket's teardown, which
// can land later, leaves the new log alone.
func TestTerminalDiagnosticsOldSocketTeardownKeepsTheNewLog(t *testing.T) {
	oldSocket, newSocket := &termDiagSocket{}, &termDiagSocket{}
	oldSocket.apply("diag_shared")
	newSocket.apply("diag_shared")
	oldSocket.close()
	if termDiagLookup("diag_shared") == nil {
		t.Fatal("the old socket's teardown dropped the new socket's log")
	}
	newSocket.input(3)
	if events := termDiagLookup("diag_shared").since(0); len(events) != 1 || events[0].Bytes != 3 {
		t.Fatalf("the new socket logs into the served log, got %+v", events)
	}
	newSocket.close()
	if termDiagLookup("diag_shared") != nil {
		t.Fatal("the new socket's own teardown drops it")
	}
}
