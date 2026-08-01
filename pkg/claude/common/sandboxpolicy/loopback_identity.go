package sandboxpolicy

import "net/netip"

// loopbackIdentityPrefixes is the ONE definition of the address space that
// names the host running the sandbox, or is carried alongside space that does,
// rather than a routable destination. The "carried alongside" is not a hedge:
// see the 0.0.0.0/8 row below.
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
//     address, and neither host's routing state maps any of it on-host. It is
//     RFC 6890 "this host on this network", whose registry entry marks it
//     valid as a SOURCE address and neither a destination nor forwardable.
//
// The /8 row stays regardless and is NOT narrowed here. Be careful about what
// the over-inclusion buys, because the two sides are not symmetric: at
// AUTHORING it only ever tightens, refusing cidr rows over space that turns
// out to be unreachable, so nothing is permitted that should be denied. At
// EVALUATION it hands those addresses to the loopback rows alone, so an
// allow-loopback posture admits a dial to something like 0.0.0.1 that leaves
// the host instead of staying on it — on Linux at least, where the kernel
// selects an off-host route and the connect times out rather than failing
// locally. macOS returned EHOSTUNREACH, which is consistent with nothing
// being transmitted; no capture was taken on either, so what is established
// is route selection, not a frame. How far such a packet travels is a
// property of the network and not of this package: a conforming router drops
// a destination the RFC 6890 registry marks non-forwardable. Narrowing the
// row is a behaviour change that needs its own scrutiny rather than a docs
// pass, and the seam for it would be the evaluator — which rows this space is
// handed to — not this list.
//
// The IPv4-mapped forms are carried so a v6-spelled range covering the same
// space cannot present a second identity for it.
//
// Two consumers read this list and they must never disagree:
//
//   - the compiler, which refuses a cidr row intersecting this space so the
//     operator is directed to the loopback selector that actually governs it
//     (PrefixIntersectsLoopbackIdentity);
//   - the proxy evaluator, whose loopback branch decides every target in this
//     space from loopback rows alone (AddrIsLoopbackIdentity).
//
// The second is only correct because the first holds: if a cidr row could
// overlap this space it would be authorable but INERT, since the evaluator
// would never reach it (TCL-899). One list is what keeps the branch complete
// rather than accidentally sufficient.
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
	// Since TCL-901 the compiler unmaps a cidr row before asking
	// PrefixIntersectsLoopbackIdentity, so a mapped prefix of at least /96
	// reaches this list already in IPv4 form and is caught by the entries
	// above. These two are kept deliberately, for the same reason the
	// IsLoopback check below is kept: they still carry the prefixes shorter
	// than /96, which unmap cannot rewrite and which reach the identity space
	// only as IPv6. They are a subset of what the refusal must cover, so they
	// can only ever agree with it.
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

// PrefixIntersectsLoopbackIdentity reports whether a cidr row would overlap the
// host-loopback identity space. Such a row is refused at authoring time with
// the loopback selector named as the remedy, because the evaluator decides that
// space from loopback rows alone and would never consult it.
func PrefixIntersectsLoopbackIdentity(prefix netip.Prefix) bool {
	for _, loopback := range loopbackIdentityPrefixes {
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
