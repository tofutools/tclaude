package sandboxproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
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
	// RouteResolver resolves reserved synthetic route identities through the
	// caller's M1 authority. It is optional so existing Internet-only proxy
	// launches retain their exact behavior; a route request without it fails
	// closed and never reaches ordinary DNS or dial policy.
	RouteResolver RouteResolver
	// RouteIdentity binds every route request to the consumer's stable group,
	// agent, conversation, launch generation, and open M1 lease.
	RouteIdentity RouteIdentity
}

// RouteLeaseReleaser closes the M5 lease selected for one route connection.
// It is optional so existing test and embedding resolvers remain source
// compatible; production authorities implement it to bind lease lifetime to
// the upstream connection lifetime.
type RouteLeaseReleaser interface {
	ReleaseRoute(context.Context, RouteResolution) error
}

// RouteIdentityProvider selects the exact target-group generation for a
// request when a launch belongs to more than one explicit group. The provider
// receives only the opaque route selector and must return a generation-bound
// identity; it cannot broaden the launch's agent/conversation authority.
type RouteIdentityProvider interface {
	IdentityForRoute(context.Context, RouteRequest) (RouteIdentity, error)
}

// Server carries and filters cooperative traffic for one sandbox. It serves
// both carriages on a single listener.
type Server struct {
	evaluator  *Evaluator
	dialer     *Dialer
	onDecision func(Carriage, Target, Decision)
	onError    func(Carriage, error)
	route      RouteResolver
	identity   RouteIdentity

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
		route:      cfg.RouteResolver,
		identity:   cfg.RouteIdentity,
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

// Close stops accepting and tears down every carried connection, both halves
// of each. A proxy failure must be a sandbox failure, never a quietly
// unfiltered sandbox.
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
	if target.Kind == TargetKindRoute {
		return s.connectRoute(ctx, carriage, target)
	}
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
	// The upstream half is tracked alongside the client half. Without it a
	// stalled tunnel survives Close: the client conn dies, the client-to-
	// upstream copy finishes, and the upstream-to-client copy stays blocked
	// reading a socket nothing closes — leaking a goroutine and a socket per
	// tunnel, which is exactly what Close promises not to do.
	if !s.track(conn) {
		_ = conn.Close()
		return nil, decision, fmt.Errorf("proxy is closing")
	}
	return conn, decision, nil
}

// connectRoute performs the same authorize-then-dial sequence as the normal
// proxy path, but through the M1 route authority rather than DNS and the
// Internet policy evaluator. The endpoint is required to be loopback and is
// dialled by IP literal, so an authority result cannot turn a route name into
// an arbitrary host or ambient-proxy escape.
func (s *Server) connectRoute(
	ctx context.Context,
	carriage Carriage,
	target Target,
) (net.Conn, Decision, error) {
	refused := Decision{
		Verdict: VerdictNotAuthorized,
		RouteID: target.RouteID,
		Detail:  refusalDetail(target, VerdictNotAuthorized),
	}
	if s.route == nil || strings.TrimSpace(s.identity.AgentID) == "" ||
		strings.TrimSpace(s.identity.ConvID) == "" || strings.TrimSpace(s.identity.LaunchGeneration) == "" {
		s.report(carriage, target, refused)
		return nil, refused, nil
	}
	request := RouteRequest{Identity: s.identity, RouteID: target.RouteID, Port: target.Port}
	if provider, ok := s.route.(RouteIdentityProvider); ok {
		identity, identityErr := provider.IdentityForRoute(ctx, request)
		if identityErr != nil {
			s.report(carriage, target, refused)
			s.reportError(carriage, fmt.Errorf("resolve named route group: %w", identityErr))
			return nil, refused, nil
		}
		request.Identity = identity
	}
	if !request.Identity.valid() {
		s.report(carriage, target, refused)
		return nil, refused, nil
	}
	resolution, err := s.route.ResolveRoute(ctx, request)
	if err != nil {
		s.report(carriage, target, refused)
		s.reportError(carriage, fmt.Errorf("resolve named route: %w", err))
		return nil, refused, nil
	}
	if err := validateRouteResolution(request, resolution); err != nil {
		s.releaseRoute(resolution)
		s.report(carriage, target, refused)
		s.reportError(carriage, fmt.Errorf("validate named route: %w", err))
		return nil, refused, nil
	}
	decision := Decision{Verdict: VerdictAllowed, RouteID: target.RouteID}
	conn, err := s.dialer.ConnectRoute(ctx, resolution.Endpoint)
	s.report(carriage, target, decision)
	if err != nil {
		s.releaseRoute(resolution)
		s.reportError(carriage, err)
		return nil, decision, err
	}
	leaseConn := &routeLeaseConn{Conn: conn, release: func() {
		s.releaseRoute(resolution)
	}}
	if !s.track(leaseConn) {
		_ = conn.Close()
		leaseConn.releaseOnce.Do(leaseConn.release)
		return nil, decision, fmt.Errorf("proxy is closing")
	}
	return leaseConn, decision, nil
}

func (s *Server) releaseRoute(resolution RouteResolution) {
	releaser, ok := s.route.(RouteLeaseReleaser)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = releaser.ReleaseRoute(ctx, resolution)
}

type routeLeaseConn struct {
	net.Conn
	release     func()
	releaseOnce sync.Once
}

func (c *routeLeaseConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}

func (c *routeLeaseConn) CloseWrite() error {
	half, ok := c.Conn.(writeCloser)
	if !ok {
		return fmt.Errorf("route upstream does not support write half-close")
	}
	return half.CloseWrite()
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
