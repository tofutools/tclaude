package sandboxproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Carriage names the proxy protocol that delivered a request. It exists for
// audit and disclosure only. No evaluation path reads it: the policy layer is
// carriage-blind by construction, which is what keeps the two carriages from
// drifting apart.
type Carriage string

const (
	CarriageHTTP   Carriage = "http"
	CarriageSOCKS5 Carriage = "socks5"
)

// handshakeTimeout bounds how long a connection may take to state what it
// wants before evaluation. It does not bound an established tunnel.
const handshakeTimeout = 30 * time.Second

// Config configures one proxy server.
type Config struct {
	// Rules is materialized launch intent. Packs must already be expanded.
	Rules sandboxpolicy.NetworkRules
	// Dialer performs host-side resolution and the guarded connection. nil
	// uses a zero-value Dialer, which reads no ambient proxy environment.
	Dialer *Dialer
	// OnDecision, when set, observes every policy decision the server makes.
	// It is called before the client learns the outcome and must not block.
	OnDecision func(Carriage, Target, Decision)
	// OnError, when set, observes non-policy failures: malformed carriage
	// handshakes and upstream connection errors.
	OnError func(Carriage, error)
}

// Server carries and filters cooperative traffic for one sandbox. It serves
// both carriages on a single listener.
type Server struct {
	evaluator  *Evaluator
	dialer     *Dialer
	onDecision func(Carriage, Target, Decision)
	onError    func(Carriage, error)

	// baseCtx bounds every upstream connection attempt to the server's own
	// lifetime, so Close aborts dials in flight as well as carried tunnels.
	baseCtx context.Context
	stop    context.CancelFunc

	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]struct{}
	listener net.Listener
}

// New builds a server from materialized launch intent.
func New(cfg Config) (*Server, error) {
	evaluator, err := NewEvaluator(cfg.Rules)
	if err != nil {
		return nil, err
	}
	return newServer(evaluator, cfg), nil
}

// NewFromRuleSet builds a server from already-compiled gateway IR.
func NewFromRuleSet(
	rules sandboxpolicy.FilteredNetworkRuleSet,
	cfg Config,
) (*Server, error) {
	evaluator, err := NewEvaluatorFromRuleSet(rules)
	if err != nil {
		return nil, err
	}
	return newServer(evaluator, cfg), nil
}

func newServer(evaluator *Evaluator, cfg Config) *Server {
	dialer := cfg.Dialer
	if dialer == nil {
		dialer = &Dialer{}
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Server{
		evaluator:  evaluator,
		dialer:     dialer,
		onDecision: cfg.OnDecision,
		onError:    cfg.OnError,
		baseCtx:    ctx,
		stop:       stop,
		conns:      make(map[net.Conn]struct{}),
	}
}

// Evaluator exposes the shared policy evaluator. Both carriages call it; a
// caller may use it to answer the same question without a connection.
func (s *Server) Evaluator() *Evaluator { return s.evaluator }

// Serve accepts connections until the listener fails or Close is called.
func (s *Server) Serve(l net.Listener) error {
	defer func() { _ = l.Close() }()
	if !s.adopt(l) {
		return nil
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			return err
		}
		if !s.track(conn) {
			_ = conn.Close()
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.untrack(conn)
			s.handle(conn)
		}()
	}
}

// Close stops accepting and tears down every carried connection. A proxy
// failure must be a sandbox failure, never a quietly unfiltered sandbox.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.stop()
	listener := s.listener
	s.listener = nil
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.conns = nil
	s.mu.Unlock()
	// Closing the listener is what actually stops accepting: Serve is blocked
	// in Accept and cannot reach its own deferred close until it returns.
	if listener != nil {
		_ = listener.Close()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	return nil
}

// adopt records the listener so Close can unblock the accept loop. It reports
// false when the server was already closed.
func (s *Server) adopt(l net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.listener = l
	return true
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	if s.conns != nil {
		delete(s.conns, conn)
	}
	s.mu.Unlock()
	_ = conn.Close()
}

// handle reads just enough to tell the carriages apart. SOCKS5 always opens
// with version byte 0x05; every HTTP request opens with a method token, whose
// first byte is an uppercase ASCII letter. Nothing beyond this dispatch knows
// which carriage is in play.
func (s *Server) handle(conn net.Conn) {
	reader := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	prefix, err := reader.Peek(1)
	if err != nil {
		return
	}
	switch {
	case prefix[0] == socks5Version:
		s.serveSOCKS5(conn, reader)
	case prefix[0] >= 'A' && prefix[0] <= 'Z':
		s.serveHTTP(conn, reader)
	default:
		s.reportError(Carriage(""), fmt.Errorf(
			"unrecognized proxy carriage (first byte 0x%02x)", prefix[0]))
	}
}

func (s *Server) report(carriage Carriage, target Target, decision Decision) {
	if s.onDecision != nil {
		s.onDecision(carriage, target, decision)
	}
}

func (s *Server) reportError(carriage Carriage, err error) {
	if err != nil && s.onError != nil {
		s.onError(carriage, err)
	}
}

// connect runs the shared authorize-then-dial sequence. Both carriages call
// exactly this, so the decision a client observes cannot depend on how it
// asked.
//
// The returned Decision is the effective verdict, reported to the audit hook
// exactly once. A non-nil error alongside an allowed verdict is an upstream
// transport failure, which a carriage must render differently from a refusal.
func (s *Server) connect(
	ctx context.Context,
	carriage Carriage,
	target Target,
) (net.Conn, Decision, error) {
	decision := s.evaluator.Evaluate(target)
	if !decision.Allowed() {
		s.report(carriage, target, decision)
		return nil, decision, nil
	}
	conn, resolved, err := s.dialer.Connect(ctx, s.evaluator, target)
	if resolved.Verdict == VerdictUnresolvable {
		// A name that will not resolve is an upstream failure, not a policy
		// refusal. Rendering it as one would tell a client its profile
		// forbids a destination the profile in fact allows.
		s.report(carriage, target, decision)
		s.reportError(carriage, err)
		return nil, decision, err
	}
	if !resolved.Allowed() {
		s.report(carriage, target, resolved)
		return nil, resolved, nil
	}
	s.report(carriage, target, decision)
	if err != nil {
		s.reportError(carriage, err)
		return nil, decision, err
	}
	return conn, decision, nil
}

// bufferedConn preserves bytes the carriage parser already pulled into its
// reader. A client may pipeline payload immediately after its CONNECT request
// or SOCKS5 request — a TLS ClientHello commonly arrives in the same segment —
// and those bytes live in the bufio.Reader, not in the socket. Reading the raw
// connection from here on would silently drop them.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if half, ok := c.Conn.(writeCloser); ok {
		return half.CloseWrite()
	}
	return c.Close()
}

// pipe joins an authorized tunnel and returns when either side finishes.
func pipe(client io.ReadWriteCloser, upstream io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
}

type writeCloser interface{ CloseWrite() error }

func closeWrite(conn io.ReadWriteCloser) {
	if half, ok := conn.(writeCloser); ok {
		_ = half.CloseWrite()
		return
	}
	_ = conn.Close()
}
