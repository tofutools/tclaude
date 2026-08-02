package sandboxproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// DefaultDialTimeout bounds one upstream connection attempt.
const DefaultDialTimeout = 30 * time.Second

// Dialer performs host-side resolution and the guarded upstream connection.
//
// Upstream chaining is off and is not configurable: this dialer never reads
// HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, or any other ambient proxy variable, so
// the requested-target policy stays authoritative and a host-side proxy
// variable cannot re-point sandbox traffic. If chaining is ever wanted it must
// arrive as an explicitly authored upstream, with its own evidence that policy
// remains authoritative.
type Dialer struct {
	// Resolve performs host-side name resolution. nil uses the process
	// default resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// DialAddr connects to one already-resolved ip:port. nil uses a plain
	// net.Dialer. It is never handed a name, so no second resolution can
	// select an address the blocker did not clear.
	DialAddr func(ctx context.Context, network, addr string) (net.Conn, error)
	// Timeout bounds a single connection attempt. Zero uses
	// DefaultDialTimeout.
	Timeout time.Duration
}

func (d *Dialer) resolve(
	ctx context.Context,
	host string,
) ([]netip.Addr, error) {
	if d != nil && d.Resolve != nil {
		return d.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (d *Dialer) timeout() time.Duration {
	if d != nil && d.Timeout > 0 {
		return d.Timeout
	}
	return DefaultDialTimeout
}

func (d *Dialer) dialAddr(
	ctx context.Context,
	addr netip.Addr,
	port int,
) (net.Conn, error) {
	address := net.JoinHostPort(addr.String(), strconv.Itoa(port))
	network := "tcp4"
	if addr.Is6() {
		network = "tcp6"
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()
	if d != nil && d.DialAddr != nil {
		return d.DialAddr(ctx, network, address)
	}
	// A zero-value net.Dialer reads no environment. Proxy discovery lives in
	// net/http, never here.
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

// Connect resolves the target host-side, applies the private-destination
// blocker to every candidate address, and connects to the first that answers.
//
// It is called only after Evaluate has authorized the target. The returned
// Decision is the resolution-stage verdict: VerdictAllowed when a connection
// was established, and a refusal verdict otherwise. A refusal carries no
// connection; a non-nil error without a refusal verdict is an upstream failure
// rather than a policy one.
func (d *Dialer) Connect(
	ctx context.Context,
	evaluator *Evaluator,
	target Target,
) (net.Conn, Decision, error) {
	addrs, err := d.candidates(ctx, target)
	if err != nil {
		return nil, Decision{
			Verdict: VerdictUnresolvable,
			Detail:  refusalDetail(target, VerdictUnresolvable),
		}, err
	}
	var (
		blocked  Decision
		anyClear bool
		lastErr  error
	)
	for _, addr := range addrs {
		decision := evaluator.EvaluateResolvedAddress(target, addr)
		if !decision.Allowed() {
			blocked = decision
			continue
		}
		anyClear = true
		conn, dialErr := d.dialAddr(ctx, addr, target.Port)
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		return conn, Decision{Verdict: VerdictAllowed}, nil
	}
	if !anyClear {
		if blocked.Verdict == "" {
			blocked = Decision{
				Verdict: VerdictUnresolvable,
				Detail:  refusalDetail(target, VerdictUnresolvable),
			}
		}
		return nil, blocked, fmt.Errorf("no authorized address for %s", target)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no address answered for %s", target)
	}
	return nil, Decision{Verdict: VerdictAllowed}, lastErr
}

// ConnectRoute dials an authority-supplied loopback endpoint directly. Route
// endpoints are never passed through name resolution or net/http proxy
// discovery; validation happens before this method is called and the address
// is kept as a literal all the way to DialAddr.
func (d *Dialer) ConnectRoute(
	ctx context.Context,
	endpoint netip.AddrPort,
) (net.Conn, error) {
	if !endpoint.IsValid() || endpoint.Addr().Zone() != "" ||
		!endpoint.Addr().IsLoopback() || endpoint.Addr().IsUnspecified() {
		return nil, fmt.Errorf("route endpoint is not a valid loopback address")
	}
	return d.dialAddr(ctx, endpoint.Addr(), int(endpoint.Port()))
}

// candidates returns the addresses to try, in resolver order. A literal target
// is its own single candidate: it is never re-resolved.
func (d *Dialer) candidates(
	ctx context.Context,
	target Target,
) ([]netip.Addr, error) {
	if target.Kind == TargetKindLiteral {
		return []netip.Addr{target.Addr}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()
	addrs, err := d.resolve(ctx, target.Name)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", target.Name, err)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		unmapped := addr.Unmap()
		if unmapped.IsValid() {
			out = append(out, unmapped)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", target.Name)
	}
	return out, nil
}
