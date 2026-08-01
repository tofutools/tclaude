package sandboxpolicy

import "net/netip"

// loopbackIdentityPrefixes is the ONE definition of the address space that
// names the host running the sandbox rather than a routable destination.
//
// It is deliberately wider than netip.Addr.IsLoopback, and wider than
// reachability alone justifies. This comment used to say that Linux routes
// 0.0.0.0/8 to the local host. It does not — on either platform we ship — and
// the two halves of that sentence come apart (TCL-910, probe output quoted in
// the PR; measured on an Azure Linux runner, kernel 6.17, and macOS 26.4):
//
//   - The UNSPECIFIED address of either family does land on the local host,
//     but not by routing, and the platforms arrive there by different
//     mechanisms. Linux resolves it in the route lookup itself:
//     "ip route get 0.0.0.0" reports "local 0.0.0.0 dev lo src 127.0.0.1".
//     macOS does not — "route -n get -inet 0.0.0.0" there returns the DEFAULT
//     route via the LAN gateway — and connect() still lands on 127.0.0.1, so
//     there the substitution happens in the connect path, below the route
//     table. For :: both platforms substitute in the connect path: a dial of
//     [::] reaches a ::1-ONLY listener on hosts whose route table calls ::
//     unreachable. This is why the claim cannot be stated once for "either
//     family" and left there.
//
//   - The REST of 0.0.0.0/8 reaches the local host on neither platform, and
//     is not special-cased at all: it is ordinary destination space resolved
//     through the route table like any other. On the routed Linux host
//     "ip route get 0.0.0.1" returns "via <gateway> dev eth0" — OFF the host
//     — and the connect times out; on macOS it likewise resolves to the
//     default gateway and fails EHOSTUNREACH. On a host with no default route
//     it fails ENETUNREACH, which is what the reviewer who found this saw:
//     that errno was a property of their route table, not of the address
//     space. Neither kernel's local table carries any part of 0/8. It is
//     RFC 6890 "this host on this network" — valid as a SOURCE address, not
//     as a destination.
//
// The /8 row stays regardless and is NOT narrowed here. Be careful about what
// the over-inclusion buys, because the two sides are not symmetric: at
// AUTHORING it only ever tightens, refusing cidr rows over space that turns
// out to be unreachable, so nothing is permitted that should be denied. At
// EVALUATION it hands those addresses to the loopback rows alone, so an
// allow-loopback posture admits a dial to something like 0.0.0.1 that leaves
// the host instead of staying on it — undeliverable anywhere, since RFC 6890
// space is not forwarded, but still not the same statement as "over-inclusion
// can only refuse more". Narrowing the row is a behaviour change that needs
// its own scrutiny rather than a docs pass.
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
// any of its spellings. Every such spelling is governed by the loopback
// selector, under every baseline — otherwise an open posture would reach host
// services through 0.0.0.0 that no authored row ever granted.
//
// It answers CONNECT-reachability only — whether traffic sent to this address
// arrives at the host running the sandbox — and is not a bind-scope test. The
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
