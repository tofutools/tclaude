package sandboxproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// originBanner is written by the test origin as soon as a connection is
// accepted, so an allowed request must observe real carried bytes rather than
// only a success reply.
const originBanner = "tclaude-test-origin\n"

// testOrigin is a real TCP server standing in for an upstream destination.
type testOrigin struct {
	addr     *net.TCPAddr
	mu       sync.Mutex
	accepted int
	received []byte
}

func newTestOrigin(t *testing.T) *testOrigin {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen origin: %v", err)
	}
	origin := &testOrigin{addr: listener.Addr().(*net.TCPAddr)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			origin.mu.Lock()
			origin.accepted++
			origin.mu.Unlock()
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, originBanner)
				// Hold the connection open until the peer finishes, so a
				// carried tunnel is observable in both directions, and keep
				// what arrived so client-to-origin carriage is checkable.
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						origin.mu.Lock()
						origin.received = append(origin.received, buf[:n]...)
						origin.mu.Unlock()
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return origin
}

func (o *testOrigin) connections() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.accepted
}

// waitForReceived polls until the origin has read want, so the assertion does
// not race the tunnel's own copy loop.
func (o *testOrigin) waitForReceived(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		got := string(o.received)
		o.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	o.mu.Lock()
	got := string(o.received)
	o.mu.Unlock()
	t.Fatalf("origin received %q, want it to contain %q", got, want)
}

// decisionRecord is what the audit hook observed for one request.
type decisionRecord struct {
	Carriage Carriage
	Target   Target
	Decision Decision
}

type testProxy struct {
	addr    string
	origin  *testOrigin
	mu      sync.Mutex
	records []decisionRecord
	dialed  []string
}

// startProxy runs a server whose upstream dials are redirected to the test
// origin, so policy behavior is exercised without any real resolution.
func startProxy(
	t *testing.T,
	rules sandboxpolicy.NetworkRules,
	resolve map[string][]string,
) *testProxy {
	t.Helper()
	return startProxyToAddr(t, rules, resolve, "")
}

// startProxyToAddr redirects upstream dials to upstreamAddr, or to the byte-
// banner test origin when it is empty.
func startProxyToAddr(
	t *testing.T,
	rules sandboxpolicy.NetworkRules,
	resolve map[string][]string,
	upstreamAddr string,
) *testProxy {
	t.Helper()
	origin := newTestOrigin(t)
	if upstreamAddr == "" {
		upstreamAddr = origin.addr.String()
	}
	proxy := &testProxy{origin: origin}
	dialer := &Dialer{
		Timeout: 5 * time.Second,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			texts, ok := resolve[host]
			if !ok {
				return nil, fmt.Errorf("no test resolution for %q", host)
			}
			out := make([]netip.Addr, 0, len(texts))
			for _, text := range texts {
				addr, err := netip.ParseAddr(text)
				if err != nil {
					return nil, err
				}
				out = append(out, addr)
			}
			return out, nil
		},
		DialAddr: func(
			ctx context.Context,
			network, addr string,
		) (net.Conn, error) {
			proxy.mu.Lock()
			proxy.dialed = append(proxy.dialed, addr)
			proxy.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstreamAddr)
		},
	}
	server, err := New(Config{
		Rules:  rules,
		Dialer: dialer,
		OnDecision: func(carriage Carriage, target Target, decision Decision) {
			proxy.mu.Lock()
			proxy.records = append(proxy.records,
				decisionRecord{carriage, target, decision})
			proxy.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	proxy.addr = listener.Addr().String()
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return proxy
}

func (p *testProxy) lastDecision(t *testing.T) decisionRecord {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.records) == 0 {
		t.Fatal("the audit hook observed no decision")
	}
	return p.records[len(p.records)-1]
}

func (p *testProxy) dialedAddrs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.dialed...)
}

// carriageOutcome is what a client observed. It is deliberately carriage-
// independent so the two carriages can be compared directly.
type carriageOutcome struct {
	Allowed  bool
	Refused  bool
	Carried  bool
	Verdict  Verdict
	Response string
}

// httpConnect drives the HTTP carriage.
func httpConnect(t *testing.T, proxyAddr, authority string) carriageOutcome {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority,
	); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	outcome := carriageOutcome{
		Verdict:  Verdict(resp.Header.Get(RefusalHeader)),
		Response: resp.Status,
	}
	switch resp.StatusCode {
	case http.StatusOK:
		outcome.Allowed = true
		banner := make([]byte, len(originBanner))
		if _, err := io.ReadFull(reader, banner); err == nil &&
			string(banner) == originBanner {
			outcome.Carried = true
		}
	case http.StatusForbidden:
		outcome.Refused = true
		body, _ := io.ReadAll(resp.Body)
		outcome.Response = string(body)
	}
	return outcome
}

// socks5Connect drives the SOCKS5 carriage. An IP-literal host is sent as
// ATYP=IPV4/IPV6 and a name as ATYP=DOMAINNAME, which is exactly how a real
// socks5h client states the same two target kinds.
func socks5Connect(t *testing.T, proxyAddr, host string, port int) carriageOutcome {
	t.Helper()
	reply, reader, conn := socks5Request(t, proxyAddr, socks5CmdConnect, host, port)
	defer func() { _ = conn.Close() }()
	outcome := carriageOutcome{Response: fmt.Sprintf("0x%02x", reply)}
	switch reply {
	case socks5ReplySucceeded:
		outcome.Allowed = true
		banner := make([]byte, len(originBanner))
		if _, err := io.ReadFull(reader, banner); err == nil &&
			string(banner) == originBanner {
			outcome.Carried = true
		}
	case socks5ReplyNotAllowedByRuleset:
		outcome.Refused = true
	}
	return outcome
}

// socks5Request performs the greeting and one request, returning the reply
// code and a reader positioned after the reply.
func socks5Request(
	t *testing.T,
	proxyAddr string,
	command byte,
	host string,
	port int,
) (byte, *bufio.Reader, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{
		socks5Version, 0x01, socks5AuthNone,
	}); err != nil {
		t.Fatalf("write socks greeting: %v", err)
	}
	reader := bufio.NewReader(conn)
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(reader, greeting); err != nil {
		t.Fatalf("read socks greeting reply: %v", err)
	}
	if greeting[0] != socks5Version || greeting[1] != socks5AuthNone {
		t.Fatalf("socks greeting reply = %v, want no-auth accepted", greeting)
	}
	request := []byte{socks5Version, command, 0x00}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.Is4() {
			v4 := addr.As4()
			request = append(request, socks5ATYPIPv4)
			request = append(request, v4[:]...)
		} else {
			v6 := addr.As16()
			request = append(request, socks5ATYPIPv6)
			request = append(request, v6[:]...)
		}
	} else {
		request = append(request, socks5ATYPDomain, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write socks request: %v", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(reader, head); err != nil {
		t.Fatalf("read socks reply: %v", err)
	}
	switch head[3] {
	case socks5ATYPIPv4:
		if _, err := io.ReadFull(reader, make([]byte, 4+2)); err != nil {
			t.Fatalf("read socks reply address: %v", err)
		}
	case socks5ATYPIPv6:
		if _, err := io.ReadFull(reader, make([]byte, 16+2)); err != nil {
			t.Fatalf("read socks reply address: %v", err)
		}
	default:
		t.Fatalf("socks reply address type 0x%02x is unexpected", head[3])
	}
	return head[1], reader, conn
}

// equivalenceCase names one policy question both carriages must answer the
// same way.
type equivalenceCase struct {
	name  string
	rules sandboxpolicy.NetworkRules
	host  string
	port  int
}

func equivalenceCases() []equivalenceCase {
	domainList := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com", IncludeSubdomains: true, Ports: []int{443}},
	}, nil)
	denyOverlap := listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "example.com",
			IncludeSubdomains: true}},
		[]sandboxpolicy.NetworkAllowEntry{{Host: "evil.example.com"}},
	)
	cidrList := listRules([]sandboxpolicy.NetworkAllowEntry{
		{CIDR: "10.20.0.0/16", Ports: []int{5432}},
	}, nil)
	loopbackList := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Loopback: true, Ports: []int{8080}},
	}, nil)
	privateResolver := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "rebind.example", Ports: []int{443}},
	}, nil)
	openDeny := openRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}})

	return []equivalenceCase{
		{"allowed apex", domainList, "example.com", 443},
		{"allowed subdomain", domainList, "api.example.com", 443},
		{"label-bound sibling", domainList, "badexample.com", 443},
		{"narrowed port", domainList, "example.com", 80},
		{"deny beats allow", denyOverlap, "evil.example.com", 443},
		{"deny leaves siblings alone", denyOverlap, "other.example.com", 443},
		{"cidr literal", cidrList, "10.20.0.5", 5432},
		{"cidr literal wrong port", cidrList, "10.20.0.5", 5433},
		{"name is never cidr-matched", cidrList, "db.example.com", 5432},
		{"loopback literal", loopbackList, "127.0.0.1", 8080},
		{"loopback name", loopbackList, "localhost", 8080},
		{"loopback unauthored port", loopbackList, "127.0.0.1", 9090},
		{"name resolving into private space", privateResolver,
			"rebind.example", 443},
		{"open baseline allows", openDeny, "example.com", 443},
		{"open baseline denies", openDeny, "tracker.example", 443},
		{"open baseline does not reach loopback", openDeny, "127.0.0.1", 8080},
	}
}

func equivalenceResolutions() map[string][]string {
	return map[string][]string{
		"example.com":       {"93.184.216.34"},
		"api.example.com":   {"93.184.216.34"},
		"a.b.example.com":   {"93.184.216.34"},
		"badexample.com":    {"93.184.216.35"},
		"evil.example.com":  {"93.184.216.36"},
		"other.example.com": {"93.184.216.37"},
		"db.example.com":    {"10.20.0.5"},
		"tracker.example":   {"93.184.216.38"},
		// The rebinding case: an authorized name whose answer points into
		// private space.
		"rebind.example": {"10.1.2.3"},
		"localhost":      {"127.0.0.1"},
	}
}

// TestCarriageEquivalence is the anti-drift gate for the two-carriage
// decision. Every case is asked over HTTP CONNECT and over SOCKS5 and the two
// results are compared to each other as equality — there is deliberately no
// second, independently written expectation list that could be edited into
// agreement with a regression.
func TestCarriageEquivalence(t *testing.T) {
	for _, tc := range equivalenceCases() {
		t.Run(tc.name, func(t *testing.T) {
			authority := net.JoinHostPort(tc.host, strconv.Itoa(tc.port))

			httpProxy := startProxy(t, tc.rules, equivalenceResolutions())
			httpOutcome := httpConnect(t, httpProxy.addr, authority)
			httpRecord := httpProxy.lastDecision(t)

			socksProxy := startProxy(t, tc.rules, equivalenceResolutions())
			socksOutcome := socks5Connect(t, socksProxy.addr, tc.host, tc.port)
			socksRecord := socksProxy.lastDecision(t)

			if !reflect.DeepEqual(httpRecord.Decision, socksRecord.Decision) {
				t.Fatalf("carriages decided differently:\n http: %+v\nsocks: %+v",
					httpRecord.Decision, socksRecord.Decision)
			}
			if !reflect.DeepEqual(httpRecord.Target, socksRecord.Target) {
				t.Fatalf("carriages parsed different targets:\n http: %+v\nsocks: %+v",
					httpRecord.Target, socksRecord.Target)
			}
			if httpRecord.Carriage != CarriageHTTP ||
				socksRecord.Carriage != CarriageSOCKS5 {
				t.Fatalf("carriages were not recorded distinctly: %q and %q",
					httpRecord.Carriage, socksRecord.Carriage)
			}
			if httpOutcome.Allowed != socksOutcome.Allowed ||
				httpOutcome.Refused != socksOutcome.Refused {
				t.Fatalf("clients observed different outcomes:\n http: %+v\nsocks: %+v",
					httpOutcome, socksOutcome)
			}

			// Anti-vacuous: an allowed case must show bytes actually carried
			// from the origin, and a refused case must show the refusal
			// reaching the client in its own protocol's vocabulary.
			allowed := httpRecord.Decision.Allowed()
			if allowed {
				if !httpOutcome.Carried || !socksOutcome.Carried {
					t.Fatalf("an allowed case carried no origin bytes:\n http: %+v\nsocks: %+v",
						httpOutcome, socksOutcome)
				}
				if httpProxy.origin.connections() == 0 ||
					socksProxy.origin.connections() == 0 {
					t.Fatal("an allowed case never reached the origin")
				}
				return
			}
			if !httpOutcome.Refused || !socksOutcome.Refused {
				t.Fatalf("a refused case was not refused to the client:\n http: %+v\nsocks: %+v",
					httpOutcome, socksOutcome)
			}
			if httpOutcome.Verdict != httpRecord.Decision.Verdict {
				t.Fatalf("http refusal header %q does not name the verdict %q",
					httpOutcome.Verdict, httpRecord.Decision.Verdict)
			}
			if httpProxy.origin.connections() != 0 ||
				socksProxy.origin.connections() != 0 {
				t.Fatal("a refused case reached the origin anyway")
			}
		})
	}
}

// TestCarriageEquivalenceCoversBothVerdicts proves the equivalence table is not
// vacuously green by being all-allow or all-refuse.
func TestCarriageEquivalenceCoversBothVerdicts(t *testing.T) {
	verdicts := make(map[Verdict]int)
	for _, tc := range equivalenceCases() {
		evaluator := mustEvaluator(t, tc.rules)
		target := mustTarget(t, tc.host, tc.port)
		decision := evaluator.Evaluate(target)
		verdicts[decision.Verdict]++
	}
	for _, want := range []Verdict{
		VerdictAllowed, VerdictDeniedByRule, VerdictNotAuthorized,
	} {
		if verdicts[want] == 0 {
			t.Fatalf("the equivalence table never produces %q", want)
		}
	}
}

func TestHTTPRefusalIsLegible(t *testing.T) {
	proxy := startProxy(t, listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com"},
	}, nil), equivalenceResolutions())
	outcome := httpConnect(t, proxy.addr, "badexample.com:443")
	if !outcome.Refused {
		t.Fatalf("expected a refusal, got %+v", outcome)
	}
	if outcome.Verdict != VerdictNotAuthorized {
		t.Fatalf("refusal header = %q, want %q",
			outcome.Verdict, VerdictNotAuthorized)
	}
	for _, want := range []string{
		"badexample.com:443", "does not authorize", "Remedy",
	} {
		if !strings.Contains(outcome.Response, want) {
			t.Fatalf("refusal body %q does not contain %q",
				outcome.Response, want)
		}
	}
}

func TestPrivateDestinationRefusalNamesItsVerdict(t *testing.T) {
	proxy := startProxy(t, listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "rebind.example", Ports: []int{443}},
	}, nil), equivalenceResolutions())
	outcome := httpConnect(t, proxy.addr, "rebind.example:443")
	if !outcome.Refused || outcome.Verdict != VerdictPrivateDestination {
		t.Fatalf("expected a private-destination refusal, got %+v", outcome)
	}
	if proxy.origin.connections() != 0 {
		t.Fatal("a blocked rebinding target reached the origin")
	}
	if dialed := proxy.dialedAddrs(); len(dialed) != 0 {
		t.Fatalf("a blocked rebinding target was dialed: %v", dialed)
	}
}

func TestSOCKS5UnsupportedCommands(t *testing.T) {
	proxy := startProxy(t, listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com", Ports: []int{443}},
	}, nil), equivalenceResolutions())
	for _, tc := range []struct {
		name    string
		command byte
	}{
		{"udp associate", socks5CmdUDPAssociate},
		{"bind", socks5CmdBind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, _, conn := socks5Request(
				t, proxy.addr, tc.command, "example.com", 443)
			defer func() { _ = conn.Close() }()
			if reply != socks5ReplyCommandNotSupported {
				t.Fatalf("reply = 0x%02x, want 0x%02x (command not supported)",
					reply, socks5ReplyCommandNotSupported)
			}
		})
	}
	if proxy.origin.connections() != 0 {
		t.Fatal("an unsupported command reached the origin")
	}
}

func TestSOCKS5RequiresNoAuthMethod(t *testing.T) {
	proxy := startProxy(t, listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com", Ports: []int{443}},
	}, nil), equivalenceResolutions())
	conn, err := net.DialTimeout("tcp", proxy.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Offer username/password only.
	if _, err := conn.Write([]byte{socks5Version, 0x01, 0x02}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read greeting reply: %v", err)
	}
	if reply[1] != socks5AuthNoAcceptable {
		t.Fatalf("greeting reply = 0x%02x, want 0x%02x",
			reply[1], socks5AuthNoAcceptable)
	}
}

func TestHTTPAbsoluteFormIsEvaluatedOnTheRequestLineHost(t *testing.T) {
	// Absolute-form forwarding needs an origin that speaks HTTP, unlike the
	// byte-banner origin the CONNECT cases use.
	var observedHost atomic.Pointer[string]
	origin := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			observedHost.Store(&host)
			// Connection is excluded: Go writes its own on the forwarded
			// request. Every other hop-by-hop header must be gone.
			for _, header := range hopByHopHeaders {
				if header == "Connection" {
					continue
				}
				if r.Header.Get(header) != "" {
					w.WriteHeader(http.StatusTeapot)
					return
				}
			}
			if r.URL.Path != "/health" || r.RequestURI != "/health" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, "ok")
		}))
	t.Cleanup(origin.Close)
	originAddr := strings.TrimPrefix(origin.URL, "http://")

	proxy := startProxyToAddr(t, listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com", Ports: []int{80, 8080}},
	}, nil), equivalenceResolutions(), originAddr)

	// The Host header names a different authority than the request line. The
	// request line is what policy evaluates, so it must also be what the
	// origin is told — otherwise an authorized connection could still select
	// an unevaluated vhost.
	// The probe carries a hop-by-hop header the origin must never see: it
	// describes the hop to this proxy, and forwarding Proxy-Authorization
	// would hand the origin the client's proxy credentials. The origin
	// answers 418 if any hop-by-hop header arrives.
	allowed := httpAbsoluteForm(t, proxy.addr,
		"http://example.com/health", "evil.example.com",
		"Proxy-Authorization: Basic c2VjcmV0")
	if !strings.Contains(allowed, "200") {
		t.Fatalf("allowed absolute-form response = %q", allowed)
	}
	record := proxy.lastDecision(t)
	if record.Target.Kind != TargetKindName ||
		record.Target.Name != "example.com" || record.Target.Port != 80 {
		t.Fatalf("absolute-form target = %+v, want example.com:80", record.Target)
	}
	if got := observedHost.Load(); got == nil || *got != "example.com" {
		t.Fatalf("origin observed Host %v, want the request-line authority "+
			"example.com", got)
	}

	// A non-default port must survive into the forwarded Host field, or a
	// vhost or an absolute-URL reconstruction on the origin sees the wrong
	// authority. The default port stays elided, as a Host field normally is.
	allowed = httpAbsoluteForm(t, proxy.addr,
		"http://example.com:8080/health", "evil.example.com:8080", "")
	if !strings.Contains(allowed, "200") {
		t.Fatalf("allowed absolute-form response on port 8080 = %q", allowed)
	}
	if got := observedHost.Load(); got == nil || *got != "example.com:8080" {
		t.Fatalf("origin observed Host %v, want example.com:8080", got)
	}
	if got := proxy.lastDecision(t); got.Target.Port != 8080 {
		t.Fatalf("target port = %d, want 8080", got.Target.Port)
	}

	refused := httpAbsoluteForm(t, proxy.addr,
		"http://badexample.com/health", "badexample.com", "")
	if !strings.Contains(refused, "403") {
		t.Fatalf("refused absolute-form response = %q", refused)
	}
	if got := proxy.lastDecision(t); got.Decision.Verdict != VerdictNotAuthorized {
		t.Fatalf("verdict = %q, want %q",
			got.Decision.Verdict, VerdictNotAuthorized)
	}
}

// httpAbsoluteForm sends one absolute-form request and returns the status line.
// The test origin answers a minimal HTTP response so the forward path is
// exercised end to end.
func httpAbsoluteForm(
	t *testing.T,
	proxyAddr, target, hostHeader, extraHeader string,
) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if extraHeader != "" {
		extraHeader += "\r\n"
	}
	if _, err := fmt.Fprintf(conn,
		"GET %s HTTP/1.1\r\nHost: %s\r\n%sConnection: close\r\n\r\n",
		target, hostHeader, extraHeader,
	); err != nil {
		t.Fatalf("write request: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read status line: %v", err)
	}
	return line
}

// TestDialerIgnoresAmbientProxyEnvironment proves upstream chaining is off. The
// ambient variables name an address that answers nothing, so a dialer that
// consulted them could not reach the origin at all.
func TestDialerIgnoresAmbientProxyEnvironment(t *testing.T) {
	blackHole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen black hole: %v", err)
	}
	blackHoleAddr := blackHole.Addr().String()
	if err := blackHole.Close(); err != nil {
		t.Fatalf("close black hole: %v", err)
	}
	for _, name := range []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		t.Setenv(name, "http://"+blackHoleAddr)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	origin := newTestOrigin(t)
	// A real Dialer with no DialAddr stub: it must reach the origin directly.
	server, err := New(Config{
		Rules: listRules([]sandboxpolicy.NetworkAllowEntry{
			{Loopback: true, Ports: []int{origin.addr.Port}},
		}, nil),
		Dialer: &Dialer{
			Timeout: 5 * time.Second,
			Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
				if host != LoopbackTargetName {
					return nil, fmt.Errorf("no test resolution for %q", host)
				}
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	authority := net.JoinHostPort("localhost", strconv.Itoa(origin.addr.Port))
	outcome := httpConnect(t, listener.Addr().String(), authority)
	if !outcome.Allowed || !outcome.Carried {
		t.Fatalf("the proxy did not reach the origin directly: %+v", outcome)
	}
	if origin.connections() == 0 {
		t.Fatal("the origin observed no connection")
	}
}

func TestCloseTearsDownCarriedConnections(t *testing.T) {
	origin := newTestOrigin(t)
	server, err := New(Config{
		Rules: listRules([]sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		}, nil),
		Dialer: &Dialer{
			Timeout: 5 * time.Second,
			Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			},
			DialAddr: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", origin.addr.String())
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	go func() { _ = server.Serve(listener) }()

	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn,
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
	); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %s, want 200", resp.Status)
	}
	banner := make([]byte, len(originBanner))
	if _, err := io.ReadFull(reader, banner); err != nil {
		t.Fatalf("read origin banner: %v", err)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("the carried connection outlived the proxy")
	}
}

// TestPipelinedPayloadSurvivesTheHandshake guards a real hazard of parsing a
// handshake with a buffered reader: a client commonly sends its first payload
// in the same segment as the request — a TLS ClientHello after CONNECT is the
// normal case — so those bytes land in the parser's buffer rather than the
// socket. Reading the bare connection from there on would drop them silently,
// which presents as a hung TLS handshake rather than an obvious failure.
func TestPipelinedPayloadSurvivesTheHandshake(t *testing.T) {
	const payload = "pipelined-client-payload"

	t.Run("http connect", func(t *testing.T) {
		proxy := startProxy(t, listRules([]sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		}, nil), equivalenceResolutions())
		conn, err := net.DialTimeout("tcp", proxy.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		// One write: request and payload arrive together.
		if _, err := io.WriteString(conn,
			"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"+
				payload,
		); err != nil {
			t.Fatalf("write CONNECT: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn),
			&http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("read CONNECT response: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT status = %s, want 200", resp.Status)
		}
		proxy.origin.waitForReceived(t, payload)
	})

	t.Run("socks5 connect", func(t *testing.T) {
		proxy := startProxy(t, listRules([]sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		}, nil), equivalenceResolutions())
		conn, err := net.DialTimeout("tcp", proxy.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte{
			socks5Version, 0x01, socks5AuthNone,
		}); err != nil {
			t.Fatalf("write greeting: %v", err)
		}
		if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
			t.Fatalf("read greeting reply: %v", err)
		}
		host := "example.com"
		request := []byte{socks5Version, socks5CmdConnect, 0x00,
			socks5ATYPDomain, byte(len(host))}
		request = append(request, host...)
		request = binary.BigEndian.AppendUint16(request, 443)
		// One write: request and payload arrive together.
		request = append(request, payload...)
		if _, err := conn.Write(request); err != nil {
			t.Fatalf("write socks request: %v", err)
		}
		reply := make([]byte, 4)
		if _, err := io.ReadFull(conn, reply); err != nil {
			t.Fatalf("read socks reply: %v", err)
		}
		if reply[1] != socks5ReplySucceeded {
			t.Fatalf("socks reply = 0x%02x, want 0x00", reply[1])
		}
		bound := 4 + 2
		if reply[3] == socks5ATYPIPv6 {
			bound = 16 + 2
		}
		if _, err := io.ReadFull(conn, make([]byte, bound)); err != nil {
			t.Fatalf("read socks bound address: %v", err)
		}
		proxy.origin.waitForReceived(t, payload)
	})
}

// TestUnresolvableTargetIsNotRenderedAsARefusal keeps a resolution failure from
// telling a client its profile forbids a destination the profile allows.
func TestUnresolvableTargetIsNotRenderedAsARefusal(t *testing.T) {
	rules := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "unresolvable.example", Ports: []int{443}},
	}, nil)
	// The resolution table deliberately has no entry for this name.
	proxy := startProxy(t, rules, equivalenceResolutions())
	outcome := httpConnect(t, proxy.addr, "unresolvable.example:443")
	if outcome.Refused {
		t.Fatalf("a resolution failure was rendered as a policy refusal: %+v",
			outcome)
	}
	if !strings.Contains(outcome.Response, "502") {
		t.Fatalf("status = %q, want 502", outcome.Response)
	}
	record := proxy.lastDecision(t)
	if !record.Decision.Allowed() {
		t.Fatalf("recorded verdict = %q, want the policy verdict (allowed)",
			record.Decision.Verdict)
	}

	socksOutcome := socks5Connect(t, proxy.addr, "unresolvable.example", 443)
	if socksOutcome.Refused {
		t.Fatalf("socks rendered a resolution failure as a refusal: %+v",
			socksOutcome)
	}
	if socksOutcome.Response != "0x04" {
		t.Fatalf("socks reply = %s, want 0x04 (host unreachable)",
			socksOutcome.Response)
	}
}

// TestResolvedAddressHonorsDenyRows guards the second evaluation stage against
// the bypass a first-stage-only deny would leave: an allowed name whose answer
// points at a denied address. Both the loopback and CIDR deny shapes are
// covered, and the open baseline is included because a deny must win there too.
func TestResolvedAddressHonorsDenyRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
		addr  string
		port  int
		want  Verdict
	}{
		{
			name: "a loopback deny refuses an allowed name resolving to loopback",
			rules: listRules([]sandboxpolicy.NetworkAllowEntry{
				{Domain: "example.com"}, {Loopback: true},
			}, []sandboxpolicy.NetworkAllowEntry{
				{Loopback: true, Ports: []int{22}},
			}),
			addr: "127.0.0.1", port: 22, want: VerdictDeniedByRule,
		},
		{
			name: "the same policy still admits an undenied loopback port",
			rules: listRules([]sandboxpolicy.NetworkAllowEntry{
				{Domain: "example.com"}, {Loopback: true},
			}, []sandboxpolicy.NetworkAllowEntry{
				{Loopback: true, Ports: []int{22}},
			}),
			addr: "127.0.0.1", port: 8080, want: VerdictAllowed,
		},
		{
			name: "a cidr deny refuses an allowed name resolving into it",
			rules: listRules([]sandboxpolicy.NetworkAllowEntry{
				{Domain: "example.com"}, {CIDR: "10.0.0.0/8"},
			}, []sandboxpolicy.NetworkAllowEntry{
				{CIDR: "10.1.0.0/16"},
			}),
			addr: "10.1.2.3", port: 443, want: VerdictDeniedByRule,
		},
		{
			name: "the same policy still admits the undenied part of the range",
			rules: listRules([]sandboxpolicy.NetworkAllowEntry{
				{Domain: "example.com"}, {CIDR: "10.0.0.0/8"},
			}, []sandboxpolicy.NetworkAllowEntry{
				{CIDR: "10.1.0.0/16"},
			}),
			addr: "10.2.3.4", port: 443, want: VerdictAllowed,
		},
		{
			name:  "a deny wins under an open baseline too",
			rules: openRules([]sandboxpolicy.NetworkAllowEntry{{CIDR: "10.1.0.0/16"}}),
			addr:  "10.1.2.3", port: 443, want: VerdictDeniedByRule,
		},
		{
			name:  "an open baseline still reaches undenied private space",
			rules: openRules([]sandboxpolicy.NetworkAllowEntry{{CIDR: "10.1.0.0/16"}}),
			addr:  "10.2.3.4", port: 443, want: VerdictAllowed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := mustEvaluator(t, tc.rules)
			decision := evaluator.EvaluateResolvedAddress(
				mustTarget(t, "example.com", tc.port),
				netip.MustParseAddr(tc.addr))
			if decision.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", decision.Verdict, tc.want)
			}
		})
	}
}

// TestUnspecifiedAddressIsHostLoopback covers the other spellings that reach
// the host itself. connect() to the unspecified address of either family lands
// on local loopback, and Linux routes 0.0.0.0/8 to the local host, so these
// must be governed by the loopback selector under every baseline — including
// an open one, where an authored loopback row cannot even be expressed.
func TestUnspecifiedAddressIsHostLoopback(t *testing.T) {
	openDeny := openRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}})
	loopbackList := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Loopback: true, Ports: []int{8080}},
	}, nil)

	for _, host := range []string{"0.0.0.0", "::", "0.1.2.3"} {
		t.Run("open baseline refuses "+host, func(t *testing.T) {
			evaluator := mustEvaluator(t, openDeny)
			target := mustTarget(t, host, 8080)
			if got := evaluator.Evaluate(target); got.Allowed() {
				t.Fatalf("requested %s was allowed under an open baseline", host)
			}
			got := evaluator.EvaluateResolvedAddress(
				mustTarget(t, "example.com", 8080), netip.MustParseAddr(host))
			if got.Allowed() {
				t.Fatalf("a name resolving to %s was allowed under an open baseline",
					host)
			}
		})
		t.Run("a loopback row carves out "+host, func(t *testing.T) {
			evaluator := mustEvaluator(t, loopbackList)
			got := evaluator.EvaluateResolvedAddress(
				mustTarget(t, "example.com", 8080), netip.MustParseAddr(host))
			if !got.Allowed() {
				t.Fatalf("verdict for %s = %q, want allowed by the loopback row",
					host, got.Verdict)
			}
		})
	}
}

// TestCloseStopsAccepting proves Close does what its name says: the listening
// socket is gone and Serve has returned, rather than staying bound until one
// more connection happens to arrive.
func TestCloseStopsAccepting(t *testing.T) {
	handled := make(chan struct{}, 1)
	server, err := New(Config{
		Rules: listRules([]sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		}, nil),
		OnDecision: func(Carriage, Target, Decision) {
			select {
			case handled <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	addr := listener.Addr().String()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	// Drive one complete request first. Waiting for its decision proves the
	// proxy was accepting, and proves the accept loop has looped back and is
	// blocked in Accept — which is the state Close has to break out of. A bare
	// dial would not: its connection could still be sitting in the accept
	// queue when Close runs, and Serve would then return for the wrong reason.
	if outcome := httpConnect(t, addr, "denied.example:443"); !outcome.Refused {
		t.Fatalf("the proxy was not serving before Close: %+v", outcome)
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never handled the probe request")
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after Close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Close; it is still blocked in Accept")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the listener is still bound after Close")
	}
}
