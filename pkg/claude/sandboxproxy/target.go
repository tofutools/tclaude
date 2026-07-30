// Package sandboxproxy implements the filtering proxy that enforces a
// discriminating network policy for the proxy engine posture.
//
// The proxy serves two carriage protocols on one listener — HTTP (CONNECT
// tunnels plus absolute-form plain HTTP) and SOCKS5 (no-auth, CONNECT command
// only) — and both parse into the same Target tuple before reaching a single
// shared Evaluator. The carriage protocol is a transport detail that never
// reaches the policy layer: there is no protocol-conditional branch anywhere in
// evaluation, so deny-first ordering, label-bound name matching, literal-only
// CIDR, and the private-destination blocker are identical across carriages by
// construction rather than by parallel maintenance.
//
// This package is pure: it opens no sandbox, injects no environment, and reads
// no ambient configuration. A caller supplies a listener and a materialized
// policy.
package sandboxproxy

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TargetKind separates the two things a client can ask for. It is the only
// distinction policy evaluation draws, and it is deliberately not the same
// question as which carriage delivered the request.
type TargetKind string

const (
	// TargetKindName is a DNS name the client stated: an HTTP CONNECT or
	// absolute-form host, or a SOCKS5 ATYP=DOMAINNAME address.
	TargetKindName TargetKind = "name"
	// TargetKindLiteral is an IP literal the client stated: an HTTP CONNECT to
	// a literal, or a SOCKS5 ATYP=IPV4/IPV6 address.
	TargetKindLiteral TargetKind = "literal"
)

// Target is the one tuple both carriages produce and the evaluator consumes.
type Target struct {
	Kind TargetKind
	// Name is set for TargetKindName, normalized to the same spelling
	// authored entries use.
	Name string
	// Addr is set for TargetKindLiteral, always unmapped so an IPv4-mapped
	// IPv6 literal cannot present a second identity for the same address.
	Addr netip.Addr
	Port int
}

// ParseTarget builds a Target from the host and port a carriage read. host is
// whatever the client stated; classification into literal or name happens here
// so neither carriage can decide it differently.
func ParseTarget(host string, port int) (Target, error) {
	if port < 1 || port > 65535 {
		return Target{}, fmt.Errorf("target port %d is invalid (want 1..65535)", port)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if !addr.IsValid() {
			return Target{}, fmt.Errorf("target address %q is invalid", host)
		}
		// A zoned literal names a local interface, which is never a routable
		// destination for this proxy.
		if addr.Zone() != "" {
			return Target{}, fmt.Errorf("target address %q carries a zone", host)
		}
		return Target{Kind: TargetKindLiteral, Addr: addr, Port: port}, nil
	}
	name, err := sandboxpolicy.NormalizeNetworkTargetName(host)
	if err != nil {
		return Target{}, fmt.Errorf("target host %q: %w", host, err)
	}
	return Target{Kind: TargetKindName, Name: name, Port: port}, nil
}

// LoopbackTargetName is the sole name spelling this proxy treats as host
// loopback. Other aliases a host file might carry are ordinary names: they
// resolve host-side and meet the private-destination blocker like any other.
const LoopbackTargetName = "localhost"

// IsLoopback reports whether the client asked for host loopback. Under this
// posture the loopback selector is the only authority over host loopback —
// there is no synthetic address for a CIDR rule or a DNS answer to smuggle in.
func (t Target) IsLoopback() bool {
	switch t.Kind {
	case TargetKindLiteral:
		return t.Addr.IsLoopback()
	case TargetKindName:
		return t.Name == LoopbackTargetName
	default:
		return false
	}
}

// Host renders the destination identity as the client stated it.
func (t Target) Host() string {
	if t.Kind == TargetKindLiteral {
		return t.Addr.String()
	}
	return t.Name
}

// String renders host and port for refusal bodies and audit records.
func (t Target) String() string {
	host := t.Host()
	if t.Kind == TargetKindLiteral && t.Addr.Is6() {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(t.Port)
}
