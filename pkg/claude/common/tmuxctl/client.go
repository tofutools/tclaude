// Package tmuxctl talks to a tmux server through control mode (`tmux -C`).
//
// A control-mode client is one long-lived tmux client whose stdin takes tmux
// commands, one per line, and whose stdout carries each command's reply framed
// as
//
//	%begin <time> <number> <flags>
//	...output lines...
//	%end <time> <number> <flags>      (or %error with the same three fields)
//
// interleaved with asynchronous notifications such as %sessions-changed or
// %exit. Replies arrive in the order the commands were written, so the client
// keeps a FIFO of pending requests and hands each closed block to the oldest.
//
// Compared with forking one tmux process per query this removes the per-query
// subprocess, and — the reason it exists — the reply is not subject to the
// client-side terminal sanitising that turned a TAB into '_' when the forking
// process had no UTF-8 locale.
//
// A control client must be attached to a session: with no session tmux closes
// it straight away. The client therefore attaches to (or creates) a dedicated
// hidden session, SessionName. It never starts a server (`-N`): a server
// started from the daemon's own environment would hand every later agent pane
// that environment, which is how a launchd daemon's missing locale reached
// panes in the first place.
package tmuxctl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SessionName is the hidden session the control client attaches to. The
// "__tclaude-" prefix marks tclaude's own plumbing sessions, which every
// session listing skips.
const SessionName = "__tclaude-control"

// PlumbingSessionPrefix is the name prefix of every tmux session tclaude owns
// for its own plumbing rather than for an agent or a shell the operator opened.
const PlumbingSessionPrefix = "__tclaude-"

// IsPlumbingSession reports whether name is one of tclaude's own plumbing
// sessions, which session listings and liveness snapshots skip.
func IsPlumbingSession(name string) bool {
	return strings.HasPrefix(name, PlumbingSessionPrefix)
}

var (
	// ErrNotConnected is returned by Run when no control connection is up:
	// the command was never written.
	ErrNotConnected = errors.New("tmux control mode: not connected")
	// ErrConnectionLost is returned when the connection ended after the
	// command was written and before its reply: tmux may or may not have run
	// it.
	ErrConnectionLost = errors.New("tmux control mode: connection lost")
	// ErrInvalidCommand is returned for a command that cannot be written as
	// one control-mode line (a line break in an argument, a non-word command
	// name). It was never sent.
	ErrInvalidCommand = errors.New("tmux control mode: command not representable")
	// ErrClosed is returned once the client has been closed for good.
	ErrClosed = errors.New("tmux control mode: client closed")
	// ErrTimeout is returned when a reply did not arrive in time. The
	// connection is torn down with it, because replies are positional: a late
	// reply would otherwise be handed to the next caller.
	ErrTimeout = errors.New("tmux control mode: reply timeout")
)

// CommandError is a command tmux ran and rejected (%error). Lines holds the
// error text tmux printed.
type CommandError struct {
	Lines []string
}

func (e *CommandError) Error() string {
	return "tmux: " + strings.Join(e.Lines, "; ")
}

// Notification is one asynchronous control-mode line, such as
// "%sessions-changed" (Name "sessions-changed", Args ""). Name "connected" and
// "disconnected" are synthesised by the client when its connection changes
// state, so a subscriber can resynchronise.
type Notification struct {
	Name string
	Args string
}

// Process is a started control-mode tmux client. Tests substitute fakes.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Kill stops the process; Wait reaps it.
	Kill()
	Wait() error
}

// Config configures a Client.
type Config struct {
	// Start launches the control-mode process. Defaults to ExecStart over
	// the tmux binary with BaseArgs.
	Start func() (Process, error)
	// BaseArgs are the tmux arguments that select the server, e.g.
	// ["-u", "-L", "tclaude"]. Used by the default Start.
	BaseArgs []string
	// Timeout bounds every request. Defaults to 5s.
	Timeout time.Duration
	// ExitWhenAlone makes the client kill its own hidden session, and so
	// disconnect, once it is the only session left on the server. That keeps
	// tmux's exit-when-empty lifetime: the hidden session never holds an
	// otherwise empty server up.
	ExitWhenAlone bool
	// Logger receives state changes. Defaults to slog.Default.
	Logger *slog.Logger
	// OnState is called (outside any lock) on every connection state change.
	OnState func(State)
}

// State is a snapshot of the connection for diagnostics.
type State struct {
	Connected bool
	// Reason says why the client is in this state ("connected", "server
	// exited", "reply timeout", "protocol error: …", "alone on server", …).
	Reason string
	Since  time.Time
	// Connects counts successful connections since start.
	Connects int64
	// Requests and Failures count requests sent over control mode.
	Requests int64
	Failures int64
}

// Client is a control-mode tmux client with reconnect. It is safe for
// concurrent use.
type Client struct {
	cfg Config

	mu     sync.Mutex
	conn   *conn
	closed bool
	state  State
	subs   map[int]chan Notification
	nextID int
}

// New returns a Client that is not yet connected; call Connect.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Start == nil {
		base := append([]string(nil), cfg.BaseArgs...)
		cfg.Start = func() (Process, error) { return ExecStart("tmux", base) }
	}
	return &Client{cfg: cfg, subs: map[int]chan Notification{}, state: State{Reason: "not connected", Since: time.Now()}}
}

// ControlArgs are the tmux arguments, after the server-selecting BaseArgs,
// that start the control client: never start a server, attach to the hidden
// session or create it running a process that does nothing.
func ControlArgs() []string {
	return []string{"-N", "-C", "new-session", "-A", "-s", SessionName, "-x", "80", "-y", "24",
		"sleep", "2147483647"}
}

// ExecStart starts `binary <base> <ControlArgs>` as the control process.
func ExecStart(binary string, base []string) (Process, error) {
	cmd := exec.Command(binary, append(append([]string(nil), base...), ControlArgs()...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &stderr}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *strings.Builder
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.Reader     { return p.stdout }
func (p *execProcess) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
func (p *execProcess) Wait() error {
	err := p.cmd.Wait()
	if detail := strings.TrimSpace(p.stderr.String()); err != nil && detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if l.n <= 0 {
		return len(b), nil
	}
	keep := b
	if len(keep) > l.n {
		keep = keep[:l.n]
	}
	l.n -= len(keep)
	_, _ = l.w.Write(keep)
	return len(b), nil
}

// State returns the current connection state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Connected reports whether a control connection is up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Subscribe returns a channel of notifications and a cancel func. A
// subscriber that falls behind loses notifications; each subscriber should
// treat any notification as "re-read what you care about", not as a delta.
func (c *Client) Subscribe() (<-chan Notification, func()) {
	ch := make(chan Notification, 64)
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.subs[id] = ch
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if _, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(ch)
		}
		c.mu.Unlock()
	}
}

func (c *Client) publish(n Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- n:
		default:
		}
	}
}

// Connect starts a control connection if none is up. It returns once the
// startup command's reply has arrived (the connection is usable) or failed.
func (c *Client) Connect() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	p, err := c.cfg.Start()
	if err != nil {
		return fmt.Errorf("start tmux control client: %w", err)
	}
	cn := newConn(c, p)
	if err := cn.awaitReady(c.cfg.Timeout); err != nil {
		cn.shutdown(err)
		return err
	}

	c.mu.Lock()
	if c.closed || c.conn != nil {
		c.mu.Unlock()
		cn.shutdown(ErrClosed)
		if c.closed {
			return ErrClosed
		}
		return nil
	}
	c.conn = cn
	c.state.Connected = true
	c.state.Reason = "connected"
	c.state.Since = time.Now()
	c.state.Connects++
	st := c.state
	c.mu.Unlock()

	c.cfg.Logger.Info("tmux control mode: connected", "session", SessionName, "module", "tmuxctl")
	c.notifyState(st)
	c.publish(Notification{Name: "connected"})

	// Session-scoped setup. Failures are not fatal: an older tmux without a
	// flag still answers queries correctly.
	if _, err := c.Run("set-option", "-t", "="+SessionName+":", "destroy-unattached", "on"); err != nil {
		c.cfg.Logger.Debug("tmux control mode: destroy-unattached not set", "error", err, "module", "tmuxctl")
	}
	if _, err := c.Run("refresh-client", "-f", "no-output"); err != nil {
		c.cfg.Logger.Debug("tmux control mode: no-output flag not set", "error", err, "module", "tmuxctl")
	}
	if c.cfg.ExitWhenAlone {
		go c.exitIfAlone()
	}
	return nil
}

func (c *Client) notifyState(st State) {
	if c.cfg.OnState != nil {
		c.cfg.OnState(st)
	}
}

// connLost is called by a conn's reader when it ends.
func (c *Client) connLost(cn *conn, reason string) {
	c.mu.Lock()
	if c.conn != cn {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	c.state.Connected = false
	c.state.Reason = reason
	c.state.Since = time.Now()
	st := c.state
	c.mu.Unlock()
	c.cfg.Logger.Info("tmux control mode: disconnected, using exec fallback", "reason", reason, "module", "tmuxctl")
	c.notifyState(st)
	c.publish(Notification{Name: "disconnected", Args: reason})
}

// Close tears the connection down for good.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	cn := c.conn
	c.mu.Unlock()
	if cn != nil {
		cn.shutdown(ErrClosed)
	}
}

// Disconnect tears the current connection down (if any) with reason; a later
// Connect may bring it back.
func (c *Client) Disconnect(reason string) {
	c.mu.Lock()
	cn := c.conn
	c.mu.Unlock()
	if cn != nil {
		cn.shutdown(errors.New(reason))
	}
}

// Run sends one tmux command and returns its output lines. args[0] is the
// command name; every argument is quoted for tmux's command parser, so no
// argument can start a second command. It returns ErrNotConnected without
// trying to connect.
func (c *Client) Run(args ...string) ([]string, error) {
	line, err := CommandLine(args...)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cn := c.conn
	if cn != nil {
		c.state.Requests++
	}
	c.mu.Unlock()
	if cn == nil {
		return nil, ErrNotConnected
	}
	out, err := cn.do(line, c.cfg.Timeout)
	if err != nil {
		var cmdErr *CommandError
		if !errors.As(err, &cmdErr) {
			c.mu.Lock()
			c.state.Failures++
			c.mu.Unlock()
		}
	}
	return out, err
}

// exitIfAlone kills the hidden session when it is the only one left, so the
// server can exit when empty as it would without a control client.
func (c *Client) exitIfAlone() {
	lines, err := c.Run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return
	}
	for _, l := range lines {
		if l != SessionName {
			return
		}
	}
	c.mu.Lock()
	cn := c.conn
	c.mu.Unlock()
	if cn == nil {
		return
	}
	cn.setLostReason("alone on server")
	// The reply may never come: killing our own session ends this client.
	_, _ = c.Run("kill-session", "-t", "="+SessionName)
}

// CommandLine renders args as one tmux command line. The command name is
// emitted bare (it must be a plain word); every other argument is quoted.
func CommandLine(args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("%w: empty command", ErrInvalidCommand)
	}
	for _, r := range args[0] {
		if (r < 'a' || r > 'z') && r != '-' {
			return "", fmt.Errorf("%w: invalid command name %q", ErrInvalidCommand, args[0])
		}
	}
	var b strings.Builder
	b.WriteString(args[0])
	for _, a := range args[1:] {
		q, err := Quote(a)
		if err != nil {
			return "", err
		}
		b.WriteByte(' ')
		b.WriteString(q)
	}
	return b.String(), nil
}

// Quote quotes one argument for tmux's command parser: always in double
// quotes, with \, " and $ escaped and a leading ~ escaped (inside double
// quotes tmux expands $VARIABLE, a leading ~ and backslash escapes; nothing
// else is special there, so ;, #, { and ' are literal). A newline or carriage return would end the command
// line and is refused.
func Quote(s string) (string, error) {
	if strings.ContainsAny(s, "\n\r") {
		return "", fmt.Errorf("%w: argument contains a line break", ErrInvalidCommand)
	}
	q := quoteEscaper.Replace(s)
	// A word that starts with ~ is home-directory expanded, quoted or not;
	// \~ keeps it literal.
	if strings.HasPrefix(q, "~") {
		q = `\` + q
	}
	return `"` + q + `"`, nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)

// ---- one connection ----

type request struct {
	done chan result
}

type result struct {
	lines []string
	err   error
}

type conn struct {
	c *Client
	p Process

	writeMu sync.Mutex

	mu         sync.Mutex
	pending    []*request
	dead       bool
	deadErr    error
	lostReason string
	ready      chan error
	readyOnce  sync.Once
	finished   chan struct{}
	inflight   chan struct{}
}

func newConn(c *Client, p Process) *conn {
	cn := &conn{c: c, p: p, ready: make(chan error, 1), finished: make(chan struct{}), inflight: make(chan struct{}, 1)}
	go cn.readLoop()
	return cn
}

func (cn *conn) markReady(err error) {
	cn.readyOnce.Do(func() { cn.ready <- err })
}

func (cn *conn) awaitReady(timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-cn.ready:
		return err
	case <-t.C:
		return fmt.Errorf("%w waiting for the control client to start", ErrTimeout)
	}
}

func (cn *conn) setLostReason(r string) {
	cn.mu.Lock()
	cn.lostReason = r
	cn.mu.Unlock()
}

func (cn *conn) do(line string, timeout time.Duration) ([]string, error) {
	req := &request{done: make(chan result, 1)}
	// One request in flight at a time. Replies are matched by position, so
	// with nothing else outstanding a forged block (should anything ever
	// smuggle %begin/%end lines into the stream) has no request to land on
	// and is dropped, instead of being handed to the next caller.
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case cn.inflight <- struct{}{}:
	case <-t.C:
		return nil, fmt.Errorf("%w: waiting for an earlier request", ErrNotConnected)
	case <-cn.finished:
		return nil, fmt.Errorf("%w: connection closed", ErrNotConnected)
	}
	defer func() { <-cn.inflight }()
	cn.writeMu.Lock()
	cn.mu.Lock()
	if cn.dead {
		err := cn.deadErr
		cn.mu.Unlock()
		cn.writeMu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrNotConnected, err)
	}
	cn.pending = append(cn.pending, req)
	cn.mu.Unlock()
	_, werr := io.WriteString(cn.p.Stdin(), line+"\n")
	cn.writeMu.Unlock()
	if werr != nil {
		cn.shutdown(fmt.Errorf("write: %w", werr))
	}

	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(timeout)
	select {
	case r := <-req.done:
		return r.lines, r.err
	case <-t.C:
		cn.shutdown(ErrTimeout)
		r := <-req.done
		if r.err == nil {
			// The reply raced the timeout; it is still the right one.
			return r.lines, nil
		}
		return nil, fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
}

// shutdown kills the process; the reader then fails every pending request.
func (cn *conn) shutdown(err error) {
	cn.mu.Lock()
	if !cn.dead {
		cn.dead = true
		cn.deadErr = err
		if cn.lostReason == "" {
			cn.lostReason = err.Error()
		}
	}
	cn.mu.Unlock()
	cn.p.Kill()
	<-cn.finished
}

func (cn *conn) fail(err error) {
	cn.mu.Lock()
	pending := cn.pending
	cn.pending = nil
	if !cn.dead {
		cn.dead = true
		cn.deadErr = err
	}
	if cn.lostReason == "" {
		cn.lostReason = err.Error()
	}
	cn.mu.Unlock()
	for _, r := range pending {
		r.done <- result{err: fmt.Errorf("%w: %v", ErrConnectionLost, err)}
	}
}

func (cn *conn) hasPending() bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return len(cn.pending) > 0
}

func (cn *conn) popPending() *request {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if len(cn.pending) == 0 {
		return nil
	}
	r := cn.pending[0]
	cn.pending = cn.pending[1:]
	return r
}

func (cn *conn) readLoop() {
	defer close(cn.finished)
	err := cn.read()
	cn.fail(err)
	_ = cn.p.Stdin().Close()
	cn.p.Kill()
	waitErr := cn.p.Wait()
	if waitErr != nil {
		cn.markReady(fmt.Errorf("%v: %w", err, waitErr))
	} else {
		cn.markReady(err)
	}
	cn.mu.Lock()
	reason := cn.lostReason
	cn.mu.Unlock()
	if waitErr != nil && reason == "server exited" {
		reason = fmt.Sprintf("server exited (%v)", waitErr)
	}
	cn.c.connLost(cn, reason)
}

// read parses the control stream until it ends. It returns why it ended.
func (cn *conn) read() error {
	br := bufio.NewReaderSize(cn.p.Stdout(), 64*1024)
	startup := true // the first block answers the startup command, not a request
	var (
		inBlock bool
		guard   string // "<time> <number> <flags>" of the open block
		lines   []string
	)
	for {
		raw, err := br.ReadString('\n')
		if err != nil {
			if raw == "" {
				if errors.Is(err, io.EOF) {
					cn.setLostReasonIfEmpty("server exited")
					cn.mu.Lock()
					reason := cn.lostReason
					cn.mu.Unlock()
					return fmt.Errorf("control client exited: %s", reason)
				}
				return err
			}
		}
		line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")

		if inBlock {
			if g, ok := blockEnd(line, "%end"); ok && g == guard {
				inBlock = false
				cn.finishBlock(&startup, lines, nil)
				lines = nil
				continue
			}
			if g, ok := blockEnd(line, "%error"); ok && g == guard {
				inBlock = false
				cn.finishBlock(&startup, nil, &CommandError{Lines: lines})
				lines = nil
				continue
			}
			lines = append(lines, line)
			continue
		}

		if g, ok := blockEnd(line, "%begin"); ok {
			if !startup && !cn.hasPending() {
				cn.c.cfg.Logger.Debug("tmux control mode: ignoring a reply block nothing is waiting for", "module", "tmuxctl")
				continue
			}
			inBlock = true
			guard = g
			lines = nil
			continue
		}
		if !strings.HasPrefix(line, "%") {
			// A bare line outside a reply block is not framing. Before the
			// startup reply it is usually why tmux refused to start ("no
			// server running on ..."), which the EOF that follows reports.
			// After it, it is output a hook printed (a user's
			// session-created hook firing for the hidden session, say).
			// Neither may end the connection: block matching by guard keeps
			// replies correct, and dropping the connection over a chatty
			// hook would only recreate the hidden session and fire the hook
			// again.
			if startup {
				cn.setLostReasonIfEmpty(truncate(line, 200))
			} else {
				cn.c.cfg.Logger.Debug("tmux control mode: ignoring a line outside a reply", "line", truncate(line, 200), "module", "tmuxctl")
			}
			continue
		}
		name, args, _ := strings.Cut(line[1:], " ")
		if name == "exit" {
			if args != "" {
				cn.setLostReasonIfEmpty("server exited: " + args)
			} else {
				cn.setLostReasonIfEmpty("server exited")
			}
			cn.markReady(errors.New("control client exited during startup"))
			return errors.New("control client got %exit")
		}
		if name == "begin" || name == "end" || name == "error" {
			continue // a stray framing line, not a notification
		}
		cn.c.publish(Notification{Name: name, Args: args})
		if name == "sessions-changed" && cn.c.cfg.ExitWhenAlone && !startup {
			go cn.c.exitIfAlone()
		}
	}
}

func (cn *conn) setLostReasonIfEmpty(r string) {
	cn.mu.Lock()
	if cn.lostReason == "" {
		cn.lostReason = r
	}
	cn.mu.Unlock()
}

func (cn *conn) finishBlock(startup *bool, lines []string, err error) {
	if *startup {
		*startup = false
		if err != nil {
			cn.markReady(err)
		} else {
			cn.markReady(nil)
		}
		return
	}
	r := cn.popPending()
	if r == nil {
		// A reply nobody asked for: positional matching is broken.
		cn.c.cfg.Logger.Warn("tmux control mode: reply without a request", "module", "tmuxctl")
		return
	}
	if err != nil {
		r.done <- result{err: err}
		return
	}
	r.done <- result{lines: lines}
}

// blockEnd parses "<marker> <time> <number> <flags>" and returns the three
// fields as one guard string.
func blockEnd(line, marker string) (string, bool) {
	rest, ok := strings.CutPrefix(line, marker+" ")
	if !ok {
		return "", false
	}
	fields := strings.Split(rest, " ")
	if len(fields) != 3 {
		return "", false
	}
	for _, f := range fields {
		if f == "" {
			return "", false
		}
		for _, r := range f {
			if r < '0' || r > '9' {
				return "", false
			}
		}
	}
	return rest, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
