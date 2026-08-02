package sandboxproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestSyntheticRouteHostRoundTripAndReservedNamespace(t *testing.T) {
	for _, routeID := range []string{"rte_0123456789abcdef", "route-a", "route/name"} {
		host, err := SyntheticRouteHost(routeID)
		if err != nil {
			t.Fatalf("SyntheticRouteHost(%q): %v", routeID, err)
		}
		if !strings.HasSuffix(host, "."+SyntheticRouteDomain) {
			t.Fatalf("host %q escaped reserved namespace", host)
		}
		got, err := ParseSyntheticRouteHost(host)
		if err != nil || got != routeID {
			t.Fatalf("ParseSyntheticRouteHost(%q) = %q, %v; want %q", host, got, err, routeID)
		}
		target, err := ParseTarget(host, 443)
		if err != nil || target.Kind != TargetKindRoute || target.RouteID != routeID {
			t.Fatalf("ParseTarget(%q) = %#v, %v", host, target, err)
		}
	}
	if _, err := ParseSyntheticRouteHost("route.tclaude.invalid"); err == nil {
		t.Fatal("reserved namespace root was accepted as a route")
	}
	if _, err := ParseTarget("r-not-base32.route.tclaude.invalid", 443); err == nil {
		t.Fatal("malformed route namespace fell through to ordinary DNS")
	}
	ordinary, err := ParseTarget("api.example.com", 443)
	if err != nil || ordinary.Kind != TargetKindName {
		t.Fatalf("ordinary target changed kind: %#v, %v", ordinary, err)
	}
}

func TestEvaluatorNeverAuthorizesSyntheticRouteByDNSRule(t *testing.T) {
	evaluator, err := NewEvaluator(listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: SyntheticRouteDomain, IncludeSubdomains: true},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	host, err := SyntheticRouteHost("route-a")
	if err != nil {
		t.Fatal(err)
	}
	target, err := ParseTarget(host, 443)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluator.Evaluate(target); got.Allowed() {
		t.Fatalf("synthetic route was authorized by a DNS suffix rule: %#v", got)
	}
}

type routeTestResolver struct {
	mu          sync.Mutex
	resolution  RouteResolution
	err         error
	requests    []RouteRequest
	resolveCall func(RouteRequest) (RouteResolution, error)
}

type routeReleaseResolver struct {
	routeTestResolver
	released chan RouteResolution
}

func (r *routeReleaseResolver) ReleaseRoute(_ context.Context, resolution RouteResolution) error {
	r.released <- resolution
	return nil
}

func (r *routeTestResolver) ResolveRoute(_ context.Context, request RouteRequest) (RouteResolution, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	resolveCall := r.resolveCall
	resolution, err := r.resolution, r.err
	r.mu.Unlock()
	if resolveCall != nil {
		return resolveCall(request)
	}
	return resolution, err
}

func (r *routeTestResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func newRouteProxy(t *testing.T, resolver RouteResolver, identity RouteIdentity) (string, *testOrigin) {
	t.Helper()
	origin := newTestOrigin(t)
	dialer := &Dialer{
		Timeout: 5 * time.Second,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return nil, fmt.Errorf("ordinary DNS must not be used for route targets")
		},
		DialAddr: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	}
	server, err := New(Config{
		Rules:         openRules(nil),
		Dialer:        dialer,
		RouteResolver: resolver,
		RouteIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String(), origin
}

func routeIdentity() RouteIdentity {
	return RouteIdentity{
		GroupID:          41,
		GroupGeneration:  7,
		AgentID:          "agt-consumer",
		ConvID:           "conv-consumer",
		LaunchGeneration: "launch-consumer",
	}
}

func routeResolution(t *testing.T, routeID string, addr net.Addr) RouteResolution {
	t.Helper()
	tcpAddr := addr.(*net.TCPAddr)
	return RouteResolution{
		RouteID:                   routeID,
		LeaseID:                   "rlease-" + routeID,
		GroupID:                   41,
		GroupGeneration:           7,
		PublisherAgentID:          "agt-publisher",
		PublisherConvID:           "conv-publisher",
		PublisherLaunchGeneration: "launch-publisher",
		Endpoint:                  netip.MustParseAddrPort(tcpAddr.String()),
		TargetPort:                443,
	}
}

func routeHTTPConnect(t *testing.T, proxyAddr, authority string) error {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP CONNECT status %s body %q", resp.Status, body)
	}
	return nil
}

func TestNamedRoutesShareOneHTTPAndSOCKSListenerConcurrently(t *testing.T) {
	routeIDs := []string{"rte-concurrent-a", "rte-concurrent-b"}
	hosts := make([]string, len(routeIDs))
	for i, routeID := range routeIDs {
		var err error
		hosts[i], err = SyntheticRouteHost(routeID)
		if err != nil {
			t.Fatal(err)
		}
	}
	origins := []*testOrigin{newTestOrigin(t), newTestOrigin(t)}
	resolver := &routeTestResolver{}
	resolver.resolveCall = func(request RouteRequest) (RouteResolution, error) {
		for i, routeID := range routeIDs {
			if request.RouteID == routeID {
				return routeResolution(t, routeID, origins[i].addr), nil
			}
		}
		return RouteResolution{}, fmt.Errorf("unknown route %q", request.RouteID)
	}
	// Keep the setup in one helper-shaped block so both protocol clients use
	// the same listener and the same route authority.
	dialer := &Dialer{
		Timeout: 5 * time.Second,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return nil, fmt.Errorf("route target reached ordinary DNS")
		},
		DialAddr: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	}
	server, err := New(Config{Rules: openRules(nil), Dialer: dialer, RouteResolver: resolver, RouteIdentity: routeIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	proxyAddr := listener.Addr().String()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			routeIndex := i % len(routeIDs)
			host := hosts[routeIndex]
			if i%2 == 0 {
				if err := routeHTTPConnect(t, proxyAddr, net.JoinHostPort(host, "443")); err != nil {
					t.Errorf("HTTP route %d: %v", i, err)
				}
				return
			}
			if outcome := socks5Connect(t, proxyAddr, host, 443); !outcome.Allowed || !outcome.Carried {
				t.Errorf("SOCKS route %d: %+v", i, outcome)
			}
		}(i)
	}
	wg.Wait()
	if got := resolver.count(); got != 6 {
		t.Fatalf("route resolver calls = %d, want 6", got)
	}
	for i, origin := range origins {
		if got := origin.connections(); got != 3 {
			t.Fatalf("route %s upstream connections = %d, want 3", routeIDs[i], got)
		}
	}
}

func TestNamedRouteRefusesCrossGroupStaleAndWithdrawnAuthority(t *testing.T) {
	routeID := "rte-refuse"
	host, err := SyntheticRouteHost(routeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "cross group", err: fmt.Errorf("route is not in the consumer group")},
		{name: "stale generation", err: fmt.Errorf("route generation is stale")},
		{name: "withdrawn lease", err: fmt.Errorf("route lease is closed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &routeTestResolver{err: tc.err}
			proxyAddr, _ := newRouteProxy(t, resolver, routeIdentity())
			if err := routeHTTPConnect(t, proxyAddr, net.JoinHostPort(host, "443")); err == nil || !strings.Contains(err.Error(), "403") {
				t.Fatalf("HTTP route refusal = %v", err)
			}
			if outcome := socks5Connect(t, proxyAddr, host, 443); !outcome.Refused || outcome.Response != "0x02" {
				t.Fatalf("SOCKS route refusal = %+v", outcome)
			}
		})
	}
}

func TestNamedRouteAuthorityIdentityAndEndpointValidation(t *testing.T) {
	request := RouteRequest{Identity: routeIdentity(), RouteID: "rte-a", Port: 443}
	valid := RouteResolution{
		RouteID:                   "rte-a",
		LeaseID:                   "lease-a",
		GroupID:                   41,
		GroupGeneration:           7,
		PublisherAgentID:          "publisher",
		PublisherConvID:           "publisher-conv",
		PublisherLaunchGeneration: "publisher-launch",
		Endpoint:                  netip.MustParseAddrPort("127.0.0.1:1234"),
		TargetPort:                443,
	}
	for _, tc := range []struct {
		name string
		edit func(*RouteResolution)
	}{
		{name: "route identity", edit: func(r *RouteResolution) { r.RouteID = "rte-other" }},
		{name: "group identity", edit: func(r *RouteResolution) { r.GroupID = 42 }},
		{name: "publisher identity", edit: func(r *RouteResolution) { r.PublisherAgentID = "" }},
		{name: "non-loopback endpoint", edit: func(r *RouteResolution) { r.Endpoint = netip.MustParseAddrPort("192.0.2.1:1234") }},
		{name: "target port", edit: func(r *RouteResolution) { r.TargetPort = 8443 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := valid
			tc.edit(&got)
			if err := validateRouteResolution(request, got); err == nil {
				t.Fatal("invalid route resolution was accepted")
			}
		})
	}
}

func TestNamedRouteLeaseClosesWithUpstreamConnection(t *testing.T) {
	origin := newTestOrigin(t)
	resolver := &routeReleaseResolver{
		routeTestResolver: routeTestResolver{resolution: routeResolution(t, "lease-route", origin.addr)},
		released:          make(chan RouteResolution, 1),
	}
	server, err := New(Config{
		Rules:         openRules(nil),
		Dialer:        &Dialer{Timeout: time.Second},
		RouteResolver: resolver,
		RouteIdentity: routeIdentity(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	host, err := SyntheticRouteHost("lease-route")
	if err != nil {
		t.Fatal(err)
	}
	target, err := ParseTarget(host, 443)
	if err != nil {
		t.Fatal(err)
	}
	upstream, decision, dialErr := server.connect(context.Background(), CarriageHTTP, target)
	if dialErr != nil || !decision.Allowed() {
		t.Fatalf("route connect = decision=%+v err=%v", decision, dialErr)
	}
	if err := upstream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case resolution := <-resolver.released:
		if resolution.LeaseID != "rlease-lease-route" {
			t.Fatalf("released resolution = %#v", resolution)
		}
	case <-time.After(time.Second):
		t.Fatal("route lease was not released when upstream closed")
	}
}
