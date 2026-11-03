package agentd

// Live-page sources: the upstreams the stage (stage.go) proxies to. The rule
// is that a page reaches only the declaring agent's own target, and nothing
// else (epic tcl-qo90av, "Security boundary"):
//
//   - its own port: a loopback port inside the agent's private network
//     namespace (dialled there through nsenter, see netnsdial; everything on
//     that loopback runs for the agent's launch), or, for a
//     host-network agent, a 127.0.0.1 port whose listener belongs to the
//     agent's process tree, proved from the kernel's socket tables on every
//     new connection (portowner);
//   - a static directory or one file inside its worktree (stage_static_unix.go).
//
// Never a host name or an IP: the only address dialled is the literal
// 127.0.0.1, at a port the daemon itself does not own.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/netnsdial"
	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// stageSourceError is an upstream failure whose message is written by the
// daemon and is safe to show in the page's tab (and to the agent).
type stageSourceError struct{ msg string }

func (e *stageSourceError) Error() string { return e.msg }

func stageSourceErrorf(format string, args ...any) error {
	return &stageSourceError{msg: fmt.Sprintf(format, args...)}
}

// stageSourceMessage is the safe text for an upstream error, or "".
func stageSourceMessage(err error) string {
	var se *stageSourceError
	if errors.As(err, &se) {
		return se.msg
	}
	return ""
}

// --- the daemon's own ports ---

var stageDaemonPorts sync.Map // int -> struct{}

// registerStageDaemonPort records a TCP port the daemon listens on. A page
// may never target one, even where the ownership proof would refuse it
// anyway.
func registerStageDaemonPort(port int) {
	if port > 0 {
		stageDaemonPorts.Store(port, struct{}{})
	}
}

func isStageDaemonPort(port int) bool {
	_, ok := stageDaemonPorts.Load(port)
	return ok
}

// --- agent port ---

// stageAgentPort proxies to a port the agent serves. Each new connection
// re-resolves the agent's process tree and re-proves the target before
// dialling; nothing else is ever dialled. It tracks its connections so
// closing the source also ends upgraded (websocket) ones, which ReverseProxy
// would otherwise splice until an end hangs up.
type stageAgentPort struct {
	port      int
	transport *http.Transport

	mu     sync.Mutex
	conns  map[*stageTrackedConn]struct{}
	closed bool
}

// stageAgentProc is where an agent's processes live: the root of its process
// tree, and whether its launch gave it a private network namespace.
type stageAgentProc struct {
	root       int
	privateNet bool
}

// stageResolveAgent resolves an agent's process tree. A seam for tests;
// production resolves the agent's live pane (or OpenCode server) and the
// launch's recorded network posture.
var stageResolveAgent = stageAgentProcess

// stageForeignNetns finds the sandbox entry process (a seam for tests).
var stageForeignNetns = stageForeignNetnsPID

// stageNetnsDial dials 127.0.0.1:port inside pid's network namespace.
var stageNetnsDial = func(ctx context.Context, pid, port int) (net.Conn, error) {
	nsenter, err := session.TrustedNsenterPath()
	if err != nil {
		slog.Warn("stage: no trusted nsenter for a sandboxed page", "err", err)
		return nil, stageSourceErrorf("cannot reach the agent's sandbox network: nsenter is unavailable")
	}
	return netnsdial.Dial(ctx, nsenter, pid, port)
}

func newStageAgentPort(agentID string, port int) (*stageAgentPort, error) {
	if port <= 0 || port > 65535 {
		return nil, stageSourceErrorf("port %d is out of range", port)
	}
	if isStageDaemonPort(port) {
		return nil, stageSourceErrorf("port %d is the daemon's own", port)
	}
	s := &stageAgentPort{port: port, conns: map[*stageTrackedConn]struct{}{}}
	s.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.dial(ctx, agentID)
		},
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 8,
		// Each new connection costs an ownership proof (and, sandboxed, an
		// nsenter); bound how many a page can drive at once.
		MaxConnsPerHost:       32,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 0, // dev servers may hold long-poll requests
		TLSHandshakeTimeout:   0,
		ForceAttemptHTTP2:     false,
	}
	return s, nil
}

func (s *stageAgentPort) dial(ctx context.Context, agentID string) (net.Conn, error) {
	conn, err := s.dialTarget(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return s.track(conn)
}

func (s *stageAgentPort) dialTarget(ctx context.Context, agentID string) (net.Conn, error) {
	if isStageDaemonPort(s.port) {
		return nil, stageSourceErrorf("port %d is the daemon's own", s.port)
	}
	proc, err := stageResolveAgent(agentID)
	if err != nil {
		return nil, err
	}
	if proc.privateNet {
		// The launch recorded a private network namespace: dial inside it,
		// never on the host. Its loopback is the sandbox's own; the
		// shallowest process in another namespace is the sandbox's entry.
		nsPID := stageForeignNetns(proc.root)
		if nsPID <= 0 {
			return nil, stageSourceErrorf("the agent's sandbox network is not running")
		}
		conn, err := stageNetnsDial(ctx, nsPID, s.port)
		if errors.Is(err, netnsdial.ErrRefused) {
			return nil, stageSourceErrorf("nothing is listening on :%d in the agent's sandbox", s.port)
		}
		if err != nil {
			if stageSourceMessage(err) != "" {
				return nil, err
			}
			return nil, stageSourceErrorf("cannot reach :%d in the agent's sandbox", s.port)
		}
		return conn, nil
	}
	// Host network: every listener a dial to 127.0.0.1:port could reach must
	// belong to the agent's process tree.
	if !portowner.ProcessOwnsAllLoopbackListeners(proc.root, s.port) {
		if portowner.HasLoopbackListener(s.port) {
			return nil, stageSourceErrorf("the listener on :%d does not belong to the agent", s.port)
		}
		return nil, stageSourceErrorf("nothing is listening on 127.0.0.1:%d", s.port)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)))
	if err != nil {
		return nil, stageSourceErrorf("nothing is answering on 127.0.0.1:%d", s.port)
	}
	return conn, nil
}

// stageTrackedConn removes itself from its source when closed.
type stageTrackedConn struct {
	net.Conn
	src  *stageAgentPort
	once sync.Once
}

func (c *stageTrackedConn) Close() error {
	c.once.Do(func() {
		c.src.mu.Lock()
		delete(c.src.conns, c)
		c.src.mu.Unlock()
	})
	return c.Conn.Close()
}

func (s *stageAgentPort) track(conn net.Conn) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = conn.Close()
		return nil, stageSourceErrorf("this page's source has closed")
	}
	tc := &stageTrackedConn{Conn: conn, src: s}
	s.conns[tc] = struct{}{}
	return tc, nil
}

// RoundTrip presents the request to the dev server as if the browser had
// opened http://localhost:<port> itself, so host and websocket-origin checks
// (Vite's among them) pass.
func (s *stageAgentPort) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	hp := net.JoinHostPort("localhost", strconv.Itoa(s.port))
	out.URL.Scheme = "http"
	out.URL.Host = hp
	out.Host = hp
	if out.Header.Get("Origin") != "" {
		out.Header.Set("Origin", "http://"+hp)
	}
	return s.transport.RoundTrip(out)
}

// Close ends the source: idle and upgraded connections alike.
func (s *stageAgentPort) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]*stageTrackedConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	s.transport.CloseIdleConnections()
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

// stageAgentProcess resolves an agent's process tree: its live pane, or for
// OpenCode the server process its tools run under, and whether the launch's
// recorded sandbox posture gave it a private network namespace. The posture
// comes from the launch record, the same decision the sandbox made
// (session.TclaudeLayerSharesHostLoopback), never from guessing at the
// namespaces processes in the tree happen to be in.
func stageAgentProcess(agentID string) (stageAgentProc, error) {
	conv := humanActionLiveConv(agentID)
	if conv == "" {
		return stageAgentProc{}, stageSourceErrorf("the agent is not running")
	}
	rows, err := db.FindSessionsByConvID(conv)
	if err != nil {
		return stageAgentProc{}, stageSourceErrorf("the agent's session could not be read")
	}
	for _, row := range rows {
		if row == nil || !session.IsTmuxSessionAlive(row.TmuxSession) {
			continue
		}
		pid := liveDebugProcessRoot(row)
		if pid <= 1 {
			continue
		}
		private, err := stageLaunchHasPrivateNet(row)
		if err != nil {
			return stageAgentProc{}, stageSourceErrorf("the agent's sandbox posture could not be read")
		}
		return stageAgentProc{root: pid, privateNet: private}, nil
	}
	return stageAgentProc{}, stageSourceErrorf("the agent has no live process")
}

// stageLaunchHasPrivateNet reports whether a launch runs in a private network
// namespace. Only Linux builds one; on macOS a Seatbelt-confined agent shares
// host loopback (or cannot listen at all).
func stageLaunchHasPrivateNet(row *db.SessionRow) (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(row.SandboxImplementation)
	if err != nil {
		return false, err
	}
	effective := sandboxpolicy.EffectiveProfile{}
	if row.EffectiveSandbox != nil {
		effective = row.EffectiveSandbox.Effective
	}
	shares, _, err := session.TclaudeLayerSharesHostLoopback(implementation, effective)
	if err != nil {
		return false, err
	}
	return !shares, nil
}

// --- reloads ---

// subscribeReload returns a channel that receives (coalesced) whenever the
// page's source asks open views to reload, and a cancel func.
func (p *stagePage) subscribeReload() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	if p.reloadSubs == nil {
		p.reloadSubs = map[chan struct{}]struct{}{}
	}
	p.reloadSubs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.reloadSubs, ch)
		p.mu.Unlock()
	}
}

// notifyReload tells every open view to reload.
func (p *stagePage) notifyReload() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for ch := range p.reloadSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// closeStageUpstream releases an upstream that holds resources (a watcher,
// idle connections).
func closeStageUpstream(up http.RoundTripper) {
	if c, ok := up.(io.Closer); ok {
		_ = c.Close()
	}
}
