package sandboxpolicy

import "net/netip"

// loopbackIdentityPrefixes is the ONE definition of the address space that
// names the host running the sandbox rather than a routable destination.
//
// It is deliberately wider than netip.Addr.IsLoopback. Linux routes 0.0.0.0/8
// to the local host, and connect() to the unspecified address of either family
// lands on local loopback, so those are further spellings of host loopback. The
// IPv4-mapped forms are carried so a v6-spelled range covering the same space
// cannot present a second identity for it.
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
	// "This network". Reaches the local host, and carries the unspecified
	// IPv4 address.
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
