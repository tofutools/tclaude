package sandboxpolicy

import "net/netip"

// loopbackIdentityPrefixes is the ONE definition of the address space that
// names the host running the sandbox rather than a routable destination —
// with one deliberate exception, at the 0.0.0.0/8 row below, which carries
// routable space alongside the host spelling it exists for.
//
// It is deliberately wider than netip.Addr.IsLoopback, and wider than
// reachability alone justifies. This comment used to say that Linux routes
// 0.0.0.0/8 to the local host. It does not — on either platform we ship — and
// the two halves of that sentence come apart (TCL-910, probe output quoted in
// the PR; measured on ONE Azure Linux runner, kernel 6.17, and ONE macOS 26.4
// host):
//
//   - The UNSPECIFIED address of either family does land on the local host,
//     but never out of a route-table entry, and the platforms differ in where
//     the substitution happens. Linux answers it in the output-route CODE:
//     the local table carries no 0/8 row, yet "ip route get 0.0.0.0" reports
//     "local 0.0.0.0 dev lo src 127.0.0.1", because a zero destination is
//     rewritten to loopback during the lookup rather than matched. macOS does
//     not — "route -n get -inet 0.0.0.0" returns the DEFAULT route via the
//     LAN gateway — and connect() still lands on 127.0.0.1, because BSD
//     substitutes the FIRST CONFIGURED INTERFACE ADDRESS for INADDR_ANY in
//     the connect path, and lo0 is first in any normal configuration. For ::
//     both platforms substitute in the connect path: a dial of [::] reaches a
//     ::1-ONLY listener on hosts whose routing state does not resolve :: at
//     all. Three mechanisms for one outcome, which is why this cannot be
//     stated once for "either family" and left there.
//
//   - The REST of 0.0.0.0/8 reaches the local host on neither platform, and
//     is not special-cased on the OUTPUT path: it is ordinary destination
//     space resolved through the route table like any other. On the routed
//     Linux host "ip route get 0.0.0.1" returns "via <gateway> dev eth0" —
//     OFF the host — and the connect times out; on macOS it likewise resolves
//     to the default gateway, and the connect fails EHOSTUNREACH. On a host
//     with no default route it fails ENETUNREACH, which is what the reviewer
//     who found this saw: that errno was a property of their route table, not
//     of the address space. No interface on either host carries a 0/8
//     address, and neither host's routing state maps any of the rest on-host.
//     It is
//     RFC 6890 "this host on this network", whose registry entry marks it
//     valid as a SOURCE address and neither a destination nor forwardable.
//
// The /8 row stays regardless and is NOT narrowed here. Since TCL-916 this
// broad membership is not itself proxy row authority: non-unspecified 0/8
// falls through to reserved-destination handling, and exact CIDR rows there
// are authorable in either polarity. The evaluator and compiler use the
// separate list below so that change does not recreate TCL-899's trap: an
// address that is both unauthorable and undeniable.
//
// The IPv4-mapped forms are carried so a v6-spelled range covering the same
// space cannot present a second identity for it.
//
// Proxy row authority is a different question. The evaluator and compiler use
// loopbackRowAuthorityPrefixes below; keeping it separate is what lets the
// broad identity membership stand without handing routable 0/8 destinations
// to loopback rows (TCL-916).
var loopbackIdentityPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	// "This network" (RFC 6890): source-only space. It carries the
	// unspecified IPv4 address, which does reach the local host; the rest of
	// the /8 does not, and is covered deliberately — see above.
	netip.MustParsePrefix("0.0.0.0/8"),
	// The unspecified IPv6 address.
	netip.MustParsePrefix("::/128"),
	// IPv4-mapped IPv6 spellings of the two IPv4 ranges above. Addresses are
	// unmapped before they reach AddrIsLoopbackIdentity, so these exist for
	// the prefix check: a mapped range must not be an authorable second name
	// for space the loopback selector governs.
	//
	// PrefixIntersectsLoopbackIdentity also answers the broad membership
	// question for mapped prefixes shorter than /96, which UnmapPrefix cannot
	// rewrite. Proxy authoring refusal uses the separate row-authority list.
	netip.MustParsePrefix("::ffff:127.0.0.0/104"),
	netip.MustParsePrefix("::ffff:0.0.0.0/104"),
}

// AddrIsLoopbackIdentity reports whether an address names the host itself by
// any of its spellings, or falls in the space carried alongside them — the
// rest of 0.0.0.0/8, which does NOT name the host and is covered deliberately
// (see loopbackIdentityPrefixes). Every spelling that does name the host is
// governed by the loopback selector, under every baseline — otherwise an open
// posture would reach host services through 0.0.0.0 that no authored row ever
// granted.
//
// The question it answers is a CONNECT-scope one — whether traffic sent to an
// address arrives at the host running the sandbox, for the spellings that do
// (see above for the space carried alongside them, which does not) — and it is
// not a bind-scope test. The
// two diverge exactly where it would hurt: 0.0.0.0 is in this space because
// connect() to it lands on local loopback, while bind() to it is the widest
// scope there is rather than a loopback-scoped one. A caller asking "may
// something listen here" needs its own predicate, not this one. The concrete
// instance is validateSeatbeltProxyEndpoint (TCL-906), and the reasoning lives
// at that call site rather than being restated here.
func AddrIsLoopbackIdentity(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	// Redundant with the list today — 127.0.0.0/8 and ::1/128 are exactly
	// IsLoopback, and both unspecified addresses fall inside 0.0.0.0/8 and
	// ::/128 — and deliberately kept. It is a subset, so it can only ever
	// agree; if a future Go release widens either predicate, this tracks it
	// and the compiler's refusal is what would then need to catch up.
	if addr.IsLoopback() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range loopbackIdentityPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// PrefixIntersectsLoopbackIdentity reports whether a prefix overlaps the broad
// loopback-identity membership. It does not answer whether a CIDR row would be
// inert: use PrefixIntersectsLoopbackRowAuthority for that policy question.
func PrefixIntersectsLoopbackIdentity(prefix netip.Prefix) bool {
	for _, loopback := range loopbackIdentityPrefixes {
		if prefixesIntersect(prefix, loopback) {
			return true
		}
	}
	return false
}

// loopbackRowAuthorityPrefixes is the address space the proxy decides from
// loopback rows alone. Unlike loopbackIdentityPrefixes it includes only the
// spellings that actually reach the host: 127/8, ::1, and the exact
// unspecified addresses. The mapped entries cover prefixes that UnmapPrefix
// cannot rewrite because they begin below the mapped block's /96 boundary.
//
// Both consumers of this list are load-bearing. AddrHasLoopbackRowAuthority
// dispatches evaluator rows; PrefixIntersectsLoopbackRowAuthority keeps CIDR
// rows out of exactly the space where that dispatch would make them inert
// (TCL-899). Adding an address to either question without the other recreates
// an unauthorable or ineffective policy row.
var loopbackRowAuthorityPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("0.0.0.0/32"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::ffff:127.0.0.0/104"),
	netip.MustParsePrefix("::ffff:0.0.0.0/128"),
}

// AddrHasLoopbackRowAuthority reports whether the proxy must decide an address
// from loopback rows rather than CIDR rows or the default verdict.
func AddrHasLoopbackRowAuthority(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	for _, prefix := range loopbackRowAuthorityPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// PrefixIntersectsLoopbackRowAuthority reports whether a CIDR row overlaps
// address space the proxy decides from loopback rows alone. Such a row would be
// inert, so authoring must direct the operator to a loopback row instead.
func PrefixIntersectsLoopbackRowAuthority(prefix netip.Prefix) bool {
	for _, loopback := range loopbackRowAuthorityPrefixes {
		if prefixesIntersect(prefix, loopback) {
			return true
		}
	}
	return false
}

func prefixesIntersect(a, b netip.Prefix) bool {
	if a.Addr().BitLen() != b.Addr().BitLen() {
		return false
	}
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
}
