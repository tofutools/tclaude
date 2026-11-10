package tmuxctl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer plays the tmux side of a control connection over pipes.
type fakeServer struct {
	t       *testing.T
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter

	mu      sync.Mutex
	n       int
	handler func(cmd string) (reply []string, isErr bool, hold bool)
	killed  bool
	done    chan struct{}
	cmds    chan string
}

func newFakeServer(t *testing.T, handler func(cmd string) ([]string, bool, bool)) *fakeServer {
	s := &fakeServer{t: t, handler: handler, done: make(chan struct{}), cmds: make(chan string, 100)}
	s.stdinR, s.stdinW = io.Pipe()
	s.stdoutR, s.stdoutW = io.Pipe()
	return s
}

func (s *fakeServer) Stdin() io.WriteCloser { return s.stdinW }
func (s *fakeServer) Stdout() io.Reader     { return s.stdoutR }
func (s *fakeServer) Kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.killed {
		s.killed = true
		_ = s.stdoutW.CloseWithError(io.EOF)
		_ = s.stdinR.Close()
	}
}
func (s *fakeServer) Wait() error { return nil }

func (s *fakeServer) write(lines ...string) {
	for _, l := range lines {
		if _, err := io.WriteString(s.stdoutW, l+"\n"); err != nil {
			return
		}
	}
}

func (s *fakeServer) block(lines []string, isErr bool) {
	s.mu.Lock()
	s.n++
	guard := fmt.Sprintf("1700000000 %d 1", s.n)
	s.mu.Unlock()
	end := "%end"
	if isErr {
		end = "%error"
	}
	s.write("%begin " + guard)
	s.write(lines...)
	s.write(end + " " + guard)
}

// serve answers the startup command and then every command line.
func (s *fakeServer) serve() {
	go func() {
		s.write("%begin 1700000000 1 0", "%end 1700000000 1 0", "%sessions-changed")
		sc := bufio.NewScanner(s.stdinR)
		for sc.Scan() {
			cmd := sc.Text()
			s.cmds <- cmd
			reply, isErr, hold := s.handler(cmd)
			if hold {
				continue
			}
			s.block(reply, isErr)
		}
	}()
}

func holdListSessions(cmd string) ([]string, bool, bool) {
	return nil, false, strings.HasPrefix(cmd, "list-sessions")
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func connectFake(t *testing.T, handler func(cmd string) ([]string, bool, bool), timeout time.Duration) (*Client, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, handler)
	c := New(Config{Timeout: timeout, Logger: quietLogger(), Start: func() (Process, error) {
		srv.serve()
		return srv, nil
	}})
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c, srv
}

func okHandler(cmd string) ([]string, bool, bool) {
	switch {
	case strings.HasPrefix(cmd, "list-sessions"):
		return []string{"$1 0 alpha", "$2 1 beta"}, false, false
	case strings.HasPrefix(cmd, "bad"):
		return []string{"unknown command: bad"}, true, false
	case strings.HasPrefix(cmd, "trick"):
		// A payload line that looks like a block end with another guard.
		return []string{"%end 1 2 1", "after"}, false, false
	}
	return nil, false, false
}

func TestRunReturnsReplyLines(t *testing.T) {
	c, _ := connectFake(t, okHandler, 2*time.Second)
	got, err := c.Run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "$1 0 alpha|$2 1 beta" {
		t.Fatalf("got %q", got)
	}
	if !c.Connected() {
		t.Fatal("not connected after a good reply")
	}
}

func TestCommandErrorKeepsConnection(t *testing.T) {
	c, _ := connectFake(t, okHandler, 2*time.Second)
	_, err := c.Run("bad")
	var ce *CommandError
	if !errors.As(err, &ce) || !strings.Contains(ce.Error(), "unknown command") {
		t.Fatalf("err = %v, want CommandError", err)
	}
	if _, err := c.Run("list-sessions"); err != nil {
		t.Fatalf("connection unusable after %%error: %v", err)
	}
}

func TestPayloadLookingLikeEndDoesNotCloseBlock(t *testing.T) {
	c, _ := connectFake(t, okHandler, 2*time.Second)
	got, err := c.Run("trick")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "%end 1 2 1|after" {
		t.Fatalf("got %q", got)
	}
}

func TestArgumentsAreQuoted(t *testing.T) {
	c, srv := connectFake(t, okHandler, 2*time.Second)
	if _, err := c.Run("display-message", "-p", "a; kill-server", "it's $HOME"); err != nil {
		t.Fatal(err)
	}
	var last string
	deadline := time.After(2 * time.Second)
	for !strings.HasPrefix(last, "display-message") {
		select {
		case last = <-srv.cmds:
		case <-deadline:
			t.Fatal("command never reached the server")
		}
	}
	want := `display-message '-p' 'a; kill-server' "it's \$HOME"`
	if last != want {
		t.Fatalf("wire line = %s, want %s", last, want)
	}
	if _, err := c.Run("x", "a\nb"); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("a line break in an argument must be refused")
	}
	if _, err := c.Run("kill-server; x"); err == nil {
		t.Fatal("a non-word command name must be refused")
	}
}

func waitCmd(t *testing.T, srv *fakeServer, prefix string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case cmd := <-srv.cmds:
			if strings.HasPrefix(cmd, prefix) {
				return
			}
		case <-deadline:
			t.Fatalf("%s never reached the server", prefix)
		}
	}
}

func waitDisconnected(t *testing.T, c *Client) State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.State(); !st.Connected {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client did not disconnect")
	return State{}
}

// A bare line outside a reply block (a hook's output) is ignored: the
// connection stays up and the pending request still gets its own reply.
func TestBareLineOutsideBlockIsIgnored(t *testing.T) {
	var srv *fakeServer
	c, srv := connectFake(t, func(cmd string) ([]string, bool, bool) {
		if strings.HasPrefix(cmd, "list-sessions") {
			srv.write("created by a hook")
			return []string{"$1 0 alpha"}, false, false
		}
		return nil, false, false
	}, 2*time.Second)
	got, err := c.Run("list-sessions")
	if err != nil || strings.Join(got, "|") != "$1 0 alpha" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !c.Connected() {
		t.Fatal("a hook line dropped the connection")
	}
}

func TestSlowReplyTimesOutAndDisconnects(t *testing.T) {
	c, _ := connectFake(t, holdListSessions, 100*time.Millisecond)
	_, err := c.Run("list-sessions")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	waitDisconnected(t, c)
}

func TestServerExitFailsPending(t *testing.T) {
	c, srv := connectFake(t, holdListSessions, 2*time.Second)
	errc := make(chan error, 1)
	go func() { _, err := c.Run("list-sessions"); errc <- err }()
	waitCmd(t, srv, "list-sessions")
	srv.write("%exit")
	if err := <-errc; !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("err = %v", err)
	}
	if st := waitDisconnected(t, c); !strings.Contains(st.Reason, "server exited") {
		t.Fatalf("reason = %q", st.Reason)
	}
}

func TestNotificationsFanOut(t *testing.T) {
	srv := newFakeServer(t, okHandler)
	c := New(Config{Timeout: 2 * time.Second, Logger: quietLogger(), Start: func() (Process, error) {
		srv.serve()
		return srv, nil
	}})
	ch, cancel := c.Subscribe()
	defer cancel()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for !seen["connected"] || !seen["sessions-changed"] {
		select {
		case n := <-ch:
			seen[n.Name] = true
		case <-deadline:
			t.Fatalf("saw %v", seen)
		}
	}
	srv.write("%window-close @3")
	for {
		select {
		case n := <-ch:
			if n.Name == "window-close" {
				if n.Args != "@3" {
					t.Fatalf("args = %q", n.Args)
				}
				return
			}
		case <-deadline:
			t.Fatal("window-close not delivered")
		}
	}
}

func TestStartupRefusalIsAnError(t *testing.T) {
	srv := newFakeServer(t, okHandler)
	c := New(Config{Timeout: 2 * time.Second, Logger: quietLogger(), Start: func() (Process, error) {
		go func() {
			srv.write("no server running on /tmp/x")
			srv.Kill()
		}()
		return srv, nil
	}})
	if err := c.Connect(); err == nil || !strings.Contains(err.Error(), "no server running") {
		t.Fatalf("Connect err = %v", err)
	}
	if c.Connected() {
		t.Fatal("connected after refusal")
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"":        "''",
		"plain":   "'plain'",
		"#{x} ;":  "'#{x} ;'",
		`a'b\c"$`: `"a'b\\c\"\$"`,
	}
	for in, want := range cases {
		got, err := Quote(in)
		if err != nil || got != want {
			t.Errorf("Quote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
