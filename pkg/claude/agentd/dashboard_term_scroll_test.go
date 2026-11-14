package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// scrollPaneTmux plays one pane's copy-mode for the scroll frame: copy-mode
// enters it, scroll-up moves -N lines, cancel leaves.
type scrollPaneTmux struct {
	mu       sync.Mutex
	copy     bool
	position int
	actions  []string
}

func (s *scrollPaneTmux) Command(args ...string) *exec.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch args[0] {
	case "display-message":
		on := "0"
		if s.copy {
			on = "1"
		}
		if strings.Contains(args[len(args)-1], "scroll_position") {
			return exec.Command("printf", "%s", fmt.Sprintf("%s %d", on, s.position))
		}
		return exec.Command("printf", "%s", on+" "+on)
	case "copy-mode":
		s.copy = true
	case "send-keys":
		switch args[len(args)-1] {
		case "scroll-up":
			n, _ := strconv.Atoi(args[len(args)-2])
			s.position += n
		case "cancel":
			s.copy, s.position = false, 0
		}
	default:
		return exec.Command("true")
	}
	s.actions = append(s.actions, strings.Join(args, " "))
	return exec.Command("true")
}

func (s *scrollPaneTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func (s *scrollPaneTmux) snapshot() (bool, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copy, append([]string(nil), s.actions...)
}

func TestParseTermScroll(t *testing.T) {
	cases := []struct {
		messageType int
		data        string
		op          string
		notches     int
		isScroll    bool
	}{
		{websocket.TextMessage, `{"type":"scroll","op":"up","notches":2}`, session.ScrollUp, 2, true},
		{websocket.TextMessage, ` {"type":"scroll","op":"live"}`, session.ScrollLive, 0, true},
		{websocket.TextMessage, `{"type":"scroll","op":"sideways"}`, "", 0, true},
		{websocket.TextMessage, `{"type":"resize","cols":80,"rows":24}`, "", 0, false},
		{websocket.TextMessage, `ls -la`, "", 0, false},
		{websocket.BinaryMessage, `{"type":"scroll","op":"up"}`, "", 0, false},
	}
	for _, c := range cases {
		op, notches, isScroll := parseTermScroll(c.messageType, []byte(c.data))
		if op != c.op || notches != c.notches || isScroll != c.isScroll {
			t.Errorf("parseTermScroll(%d, %q) = %q, %d, %v; want %q, %d, %v", c.messageType, c.data, op, notches, isScroll, c.op, c.notches, c.isScroll)
		}
	}
}

// A phone swipe over a Claude Code pane (tmux mouse off) scrolls the pane's
// history in copy-mode through the terminal socket: the frame never reaches
// the PTY, the answer says where the pane is, and a terminal that leaves
// copy-mode behind cancels it on close.
func TestBrowserTerminalScrollFrame(t *testing.T) {
	fake := &scrollPaneTmux{}
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
		runPTYOverWS(w, r, "stty -echo; printf 'READY\\n'; exec cat", "", "sess-x", nil, "")
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
	send(`{"type":"resize","cols":80,"rows":24}`)
	proc := <-started
	t.Cleanup(func() { _ = syscall.Kill(-proc.Pid, syscall.SIGKILL); <-done })

	// read collects frames until want returns true, keeping the PTY output
	// and the last scroll answer apart.
	var output strings.Builder
	var answer map[string]any
	read := func(what string, want func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !want() {
			_ = conn.SetReadDeadline(deadline)
			kind, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for %s: %v (output %q)", what, err, output.String())
			}
			if kind == websocket.TextMessage && strings.HasPrefix(string(data), `{"type":"scroll"`) {
				answer = nil
				if err := json.Unmarshal(data, &answer); err != nil {
					t.Fatal(err)
				}
				continue
			}
			output.Write(data)
		}
	}
	read("READY", func() bool { return strings.Contains(output.String(), "READY") })

	send(`{"type":"scroll","op":"up","notches":2}`)
	read("the scroll answer", func() bool { return answer != nil })
	if answer["copy_mode"] != true || answer["position"] != float64(10) {
		t.Fatalf("answer = %v, want copy_mode and position 10", answer)
	}
	send("typed\n")
	read("input echoed by cat", func() bool { return strings.Contains(output.String(), "typed") })
	if strings.Contains(output.String(), "scroll") {
		t.Fatalf("the scroll frame reached the PTY: %q", output.String())
	}

	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	copyMode, actions := fake.snapshot()
	if copyMode {
		t.Fatalf("the pane was left in copy-mode: %v", actions)
	}
	want := []string{
		"copy-mode -e -t =sess-x:0.0",
		"send-keys -t =sess-x:0.0 -X -N 10 scroll-up",
		"send-keys -t =sess-x:0.0 -X cancel",
	}
	if strings.Join(actions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tmux ran\n%s\nwant\n%s", strings.Join(actions, "\n"), strings.Join(want, "\n"))
	}
}
