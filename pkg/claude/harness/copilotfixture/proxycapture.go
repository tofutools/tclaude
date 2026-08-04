package copilotfixture

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyCaptureDialTimeout bounds a pass-through dial so a destination that
// blackholes cannot hang the capture for the whole test timeout.
const proxyCaptureDialTimeout = 30 * time.Second

// Observing which hosts a credential-free Copilot startup actually dials.
//
// The filtered-network contract (TCL-978) names the destinations a first-party
// Copilot launch needs. Those hosts were DERIVED — read out of the pinned CLI's
// shipped runtime module — and a derivation is not an observation: it proves a
// string is in the binary, not that a launch dials it, and it cannot prove the
// list is complete.
//
// This closes as much of that gap as is possible without a credential. Copilot
// honours HTTP_PROXY / HTTPS_PROXY, so pointing them at a local proxy that
// LOGS and then REFUSES every tunnel turns startup into an enumeration of the
// hosts the CLI wanted to reach. No token is involved, nothing leaves the
// machine, and the refusal is what keeps it that way: the proxy answers every
// CONNECT with 502 rather than dialing anything.
//
// What this can and cannot establish, stated plainly because the difference is
// the whole value of the evidence:
//
//   - It CAN show that an unauthenticated startup dials the hosts the contract
//     names, and it can surface a host the contract does NOT name — which is
//     the regression that would matter, since an unnamed destination is one the
//     filtered wall denies.
//   - It CANNOT enumerate post-authentication traffic. A launch that gets past
//     the token exchange may reach further hosts, and no credential-free run
//     can see them. That limitation is recorded rather than papered over; see
//     the test that consumes this.
//
// PASS-THROUGH MODE (TCL-984) closes that second limit, and it is opt-in
// because it is the more dangerous of the two shapes: it lets the traffic
// through, so a run that uses it is a real credentialed session talking to the
// real service. It exists because the refusing mode structurally cannot answer
// the question that mattered — the model host is handed to the session BY the
// token exchange, so a session that never completes one never learns it, and
// the released pack turned out to be missing the host a real account is given.
//
// What keeps that mode honest is that it records no more than the refusing one
// does. It observes the CONNECT authority and whether the tunnel opened; the
// tunnel itself is spliced with a blind io.Copy in both directions and is never
// read, parsed, buffered for inspection or logged. TLS is untouched — no MITM,
// no trust root — so the bytes are unreadable here even in principle. A proxy
// request that is not a CONNECT is REFUSED rather than served, which is what
// makes "this cannot log a URL, a header or a body" a property of the shape
// rather than a promise about the code: plain-HTTP proxying is the only way a
// request line would ever reach this process, and it does not happen.

// invalidCaptureToken is a syntactically well-formed GitHub token that
// authenticates nothing. It exists so the capture scenario can get PAST the
// CLI's local "No authentication information found" check, which otherwise
// exits before a single connection is opened.
//
// It is deliberately all zeroes: a reader must be able to tell at a glance
// that no credential is embedded here, and any secret scanner that sees it
// should be able to as well.
const invalidCaptureToken = "ghu_0000000000000000000000000000000000000"

// ProxyCaptureOptions selects the capture shape. The zero value is the
// refusing capture every credential-free scenario uses.
type ProxyCaptureOptions struct {
	// PassThrough opens the tunnel instead of refusing it, so a credentialed
	// session can complete and its post-token-exchange destinations become
	// observable. Only a local evidence run sets this: it puts real traffic on
	// the wire, which is exactly what the refusing default avoids.
	PassThrough bool

	// UpstreamProxy chains the pass-through dial through another proxy, for a
	// host whose own sandbox forces egress that way. Accepts the usual
	// http://[user:pass@]host:port form. Any credentials in it are used to
	// build one Proxy-Authorization header and are never recorded or logged.
	//
	// Ignored unless PassThrough is set: chaining a refusal would be a way to
	// leak the destination set to a third party for no observational gain.
	UpstreamProxy string

	// AllowedDestinations turns the pass-through capture into a stand-in for
	// the filtered wall: a "host:port" in this list is dialed, and everything
	// else is refused and recorded as refused. Empty means dial everything.
	//
	// This is what makes a capture able to prove SUFFICIENCY rather than only
	// completeness. Observing which hosts a session reaches shows what a pack
	// must contain; running a session against a wall built from the pack itself
	// shows that what the pack contains is enough — and that the destinations
	// left out are ones the session can lose without failing. Without it,
	// "telemetry is optional" would be an assumption about a host that was
	// observed being contacted.
	AllowedDestinations []string
}

// ProxyObservation is one destination reached during one labelled phase. It
// carries protocol metadata only — there is deliberately nowhere in it to put
// a URL, a header, a payload or an identity.
type ProxyObservation struct {
	// Phase is the label set by SetPhase when the destination was first seen,
	// so a multi-step capture can attribute a host to startup, a turn, or a
	// resume rather than to the run as a whole.
	Phase string
	Host  string
	Port  int
	// Tunnels counts CONNECT attempts; Dialed and Refused split them by
	// outcome. A refusal is as much evidence as a success — it is what a
	// filtered wall does to an unauthored destination.
	Tunnels int
	Dialed  int
	Refused int
}

// ProxyCapture is a local HTTP proxy that records connection targets.
type ProxyCapture struct {
	listener net.Listener
	opts     ProxyCaptureOptions
	mu       sync.Mutex
	hosts    map[string]int
	phase    string
	observed map[string]*ProxyObservation
	order    []string
	wg       sync.WaitGroup
}

// NewProxyCapture starts a refusing capture proxy bound to loopback and stops
// it when the test ends.
func NewProxyCapture(t *testing.T) *ProxyCapture {
	return NewProxyCaptureWithOptions(t, ProxyCaptureOptions{})
}

// NewProxyCaptureWithOptions starts a capture proxy in the requested shape.
func NewProxyCaptureWithOptions(t *testing.T, opts ProxyCaptureOptions) *ProxyCapture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("copilotfixture: starting the capture proxy: %v", err)
	}
	capture := &ProxyCapture{
		listener: listener,
		opts:     opts,
		hosts:    map[string]int{},
		observed: map[string]*ProxyObservation{},
	}
	capture.wg.Add(1)
	go capture.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		capture.wg.Wait()
	})
	return capture
}

// SetPhase labels the destinations observed from now on.
func (c *ProxyCapture) SetPhase(phase string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phase = phase
}

// Observations returns the recorded destinations in the order they were first
// seen, which is itself part of the evidence: a control-plane host reached
// before a model host is what a token exchange preceding a turn looks like.
func (c *ProxyCapture) Observations() []ProxyObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ProxyObservation, 0, len(c.order))
	for _, key := range c.order {
		out = append(out, *c.observed[key])
	}
	return out
}

// Endpoint is the host:port the proxy variables should point at.
func (c *ProxyCapture) Endpoint() string { return c.listener.Addr().String() }

// Hosts returns the observed destinations (host, without port) in sorted order.
func (c *ProxyCapture) Hosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.hosts))
	for host := range c.hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

func (c *ProxyCapture) serve() {
	defer c.wg.Done()
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() { _ = conn.Close() }()
			c.handle(conn)
		}()
	}
}

// handle records one request's target and refuses it.
//
// Both proxy shapes are handled because the CLI uses both: HTTPS goes through
// CONNECT (whose request-URI is the authority), while a plain-HTTP request
// through a proxy carries an absolute URI and a Host header.
func (c *ProxyCapture) handle(conn net.Conn) {
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	// Kept WITH its port: record splits the two, and the port is contract
	// evidence in its own right — a pack authorizes host and port together, so
	// an observation that lost the port could not be checked against one.
	target := request.Host
	if target == "" && request.URL != nil {
		target = request.URL.Host
	}
	target = strings.ToLower(strings.TrimSpace(target))

	// Pass-through applies to CONNECT only. A plain-HTTP proxy request is
	// refused in BOTH shapes, which is what keeps a request line, a header set
	// and a body out of this process entirely rather than merely unlogged.
	if !c.opts.PassThrough || request.Method != http.MethodConnect {
		c.record(target, false)
		// Refused rather than tunnelled: the point is to observe the intent, and
		// a proxy that actually connected would make this test depend on — and
		// send traffic to — the live GitHub services.
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}

	// The authority is used verbatim as the dial target, so a CONNECT whose
	// authority is not host:port is refused rather than guessed at.
	authority := target
	if _, _, err := net.SplitHostPort(authority); err != nil {
		c.record(target, false)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	if !c.permits(authority) {
		// The wall's own answer: refused without dialing, and recorded as such,
		// so a phase run this way reads as "what a filtered launch would do".
		c.record(target, false)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	upstream, err := c.dial(authority)
	if err != nil {
		c.record(target, false)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	defer func() { _ = upstream.Close() }()
	c.record(target, true)
	if _, err := fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	// Blind splice. Both directions are io.Copy and nothing else: the bytes are
	// TLS records this process has no key for, and there is no branch here that
	// could inspect, buffer or record them even if it did.
	var streams sync.WaitGroup
	streams.Add(2)
	go func() {
		defer streams.Done()
		_, _ = io.Copy(upstream, reader)
		closeWrite(upstream)
	}()
	go func() {
		defer streams.Done()
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
	}()
	streams.Wait()
}

// permits answers whether the configured wall admits this authority. An empty
// allow list is "no wall", not "deny everything": a capture whose whole job is
// enumeration must not silently become a refusal when nobody configured one.
func (c *ProxyCapture) permits(authority string) bool {
	if len(c.opts.AllowedDestinations) == 0 {
		return true
	}
	return slices.ContainsFunc(c.opts.AllowedDestinations, func(allowed string) bool {
		return strings.EqualFold(strings.TrimSpace(allowed), authority)
	})
}

// dial reaches the destination directly, or through the configured upstream
// proxy when the host sandbox forces egress that way.
func (c *ProxyCapture) dial(authority string) (net.Conn, error) {
	via := strings.TrimSpace(c.opts.UpstreamProxy)
	if via == "" {
		return net.DialTimeout("tcp", authority, proxyCaptureDialTimeout)
	}
	parsed, err := url.Parse(via)
	if err != nil {
		return nil, fmt.Errorf("parsing the upstream proxy: %w", err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("the upstream proxy names no host")
	}
	// Credentials live in this header and nowhere else: they are not stored on
	// the capture, not attributed to an observation and not printed on failure.
	authorization := ""
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		authorization = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString(
			[]byte(parsed.User.Username()+":"+password)) + "\r\n"
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, proxyCaptureDialTimeout)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n",
		authority, authority, authorization); err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn),
		&http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		// The status is the whole diagnostic: an upstream refusal is the host's
		// own wall, not the CLI's behaviour, and a capture that silently folded
		// the two together would read as a Copilot finding.
		return nil, fmt.Errorf("upstream proxy refused with status %d", response.StatusCode)
	}
	return conn, nil
}

// record accounts one destination under the current phase.
func (c *ProxyCapture) record(target string, dialed bool) {
	if target == "" {
		return
	}
	host, port := target, 0
	if h, p, err := net.SplitHostPort(target); err == nil {
		host = h
		if parsed, convErr := strconv.Atoi(p); convErr == nil {
			port = parsed
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hosts[host]++
	key := c.phase + "|" + host + "|" + strconv.Itoa(port)
	observation := c.observed[key]
	if observation == nil {
		observation = &ProxyObservation{Phase: c.phase, Host: host, Port: port}
		c.observed[key] = observation
		c.order = append(c.order, key)
	}
	observation.Tunnels++
	if dialed {
		observation.Dialed++
		return
	}
	observation.Refused++
}

func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
