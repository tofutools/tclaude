package copilotfixture

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

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

// invalidCaptureToken is a syntactically well-formed GitHub token that
// authenticates nothing. It exists so the capture scenario can get PAST the
// CLI's local "No authentication information found" check, which otherwise
// exits before a single connection is opened.
//
// It is deliberately all zeroes: a reader must be able to tell at a glance
// that no credential is embedded here, and any secret scanner that sees it
// should be able to as well.
const invalidCaptureToken = "ghu_0000000000000000000000000000000000000"

// ProxyCapture is a local HTTP proxy that records connection targets.
type ProxyCapture struct {
	listener net.Listener
	mu       sync.Mutex
	hosts    map[string]int
	wg       sync.WaitGroup
}

// NewProxyCapture starts a capturing proxy bound to loopback and stops it when
// the test ends.
func NewProxyCapture(t *testing.T) *ProxyCapture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("copilotfixture: starting the capture proxy: %v", err)
	}
	capture := &ProxyCapture{listener: listener, hosts: map[string]int{}}
	capture.wg.Add(1)
	go capture.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		capture.wg.Wait()
	})
	return capture
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
	target := request.Host
	if target == "" && request.URL != nil {
		target = request.URL.Host
	}
	if host, _, splitErr := net.SplitHostPort(target); splitErr == nil {
		target = host
	}
	if target = strings.ToLower(strings.TrimSpace(target)); target != "" {
		c.mu.Lock()
		c.hosts[target]++
		c.mu.Unlock()
	}
	// Refused rather than tunnelled: the point is to observe the intent, and a
	// proxy that actually connected would make this test depend on — and send
	// traffic to — the live GitHub services.
	_, _ = fmt.Fprint(conn,
		"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}
