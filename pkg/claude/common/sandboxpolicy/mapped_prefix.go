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
func UnmapPrefix(prefix netip.Prefix) netip.Prefix {
	if !prefix.IsValid() || !prefix.Addr().Is4In6() {
		return prefix
	}
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
