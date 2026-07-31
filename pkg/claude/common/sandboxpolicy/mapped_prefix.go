package sandboxpolicy

import "net/netip"

// mappedIPv4Block is the IPv4-mapped IPv6 block (RFC 4291). It is named here
// for its prefix length alone. Membership is decided by netip.Addr.Is4In6, and
// the rewrite itself by netip.Addr.Unmap — the same call ParseTarget and
// EvaluateResolvedAddress already use to reduce a target to one address form.
// UnmapPrefix therefore does not restate what an IPv4-mapped address is; it
// lifts the one existing definition from an address to a prefix.
var mappedIPv4Block = netip.MustParsePrefix("::ffff:0:0/96")

// UnmapPrefix rewrites an IPv4-mapped IPv6 prefix to the IPv4 prefix naming
// exactly the same addresses (::ffff:10.0.0.0/104 -> 10.0.0.0/8), and returns
// every other prefix unchanged.
//
// This exists because a cidr row is stored in whatever form the operator
// authored, while every target is unmapped before matching, and
// netip.Prefix.Contains is false across differing BitLen. A mapped row was
// therefore authorable but INERT: it matched nothing in the proxy evaluator,
// and the nft renderer keyed its family on Addr().Is4() and emitted an "ip6
// daddr ::ffff:.../104" rule that no packet toward routable IPv4 can match
// either. Both engines silently did nothing (TCL-901). Normalizing at compile
// time gives the row the meaning its author wrote, identically on both engines.
//
// The rewrite is total rather than partial, which is what makes normalizing a
// complete answer instead of a special case. A prefix shorter than /96 that
// intersects the mapped block necessarily contains the whole of it, hence
// contains ::ffff:0.0.0.0/104, and PrefixIntersectsLoopbackIdentity already
// refuses it. So every mapped prefix that survives authoring is at least /96
// and lies wholly inside the block, where the bit arithmetic below is exact.
// There is no residue of partially-normalizable rows needing a second policy.
// The returned prefix is ALWAYS masked — on the pass-through paths as much as
// on the rewriting one. UnmapPrefix(2001:db8::1/32) returns 2001:db8::/32, not
// the argument. Masking here rather than relying on the caller is what lets the
// paragraph above be stated unconditionally: without it,
// UnmapPrefix(::ffff:10.0.0.5/104) would return 10.0.0.5/8, whose String()
// renders a non-canonical prefix even though Contains still agrees.
//
// So this canonicalizes host bits, and callers must not rely on getting their
// own value back unchanged. It never changes which addresses the prefix names:
// masking only clears bits below the prefix length, so Contains answers
// identically for every address, on every path. The sole production caller
// (normalizeNetworkAllowEntry) masks before calling and is unaffected either
// way; the guarantee is stated for the second caller.
func UnmapPrefix(prefix netip.Prefix) netip.Prefix {
	if !prefix.IsValid() {
		return prefix
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4In6() {
		return prefix
	}
	// Unreachable for a masked prefix — masking below /96 clears part of the
	// ffff field, so Is4In6 is already false above — and kept deliberately, in
	// the same posture as the redundant checks in loopback_identity.go. It is
	// what stops the subtraction below from going negative if Is4In6 ever
	// stops meaning exactly "inside ::ffff:0:0/96".
	if prefix.Bits() < mappedIPv4Block.Bits() {
		return prefix
	}
	unmapped := netip.PrefixFrom(
		prefix.Addr().Unmap(),
		prefix.Bits()-mappedIPv4Block.Bits(),
	)
	if !unmapped.IsValid() {
		return prefix
	}
	return unmapped
}
