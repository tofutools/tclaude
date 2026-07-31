package sandboxpolicy

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestUnmapPrefixRewritesMappedIPv4Prefixes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"routable mapped range", "::ffff:10.0.0.0/104", "10.0.0.0/8"},
		{"mapped host", "::ffff:8.8.8.8/128", "8.8.8.8/32"},
		{"mapped subnet", "::ffff:192.168.1.0/120", "192.168.1.0/24"},
		{"whole mapped block", "::ffff:0:0/96", "0.0.0.0/0"},
		// Below /96 the prefix reaches outside the mapped block, so no IPv4
		// prefix names the same addresses and the row must stay as authored.
		{"below the mapped block", "::ffff:0:0/95", "::fffe:0:0/95"},
		{"ordinary v6 untouched", "2001:db8::/32", "2001:db8::/32"},
		{"ordinary v4 untouched", "10.0.0.0/8", "10.0.0.0/8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := UnmapPrefix(netip.MustParsePrefix(c.in).Masked())
			if got.String() != c.want {
				t.Fatalf("UnmapPrefix(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// TestMappedCIDRRowCompilesToIPv4Rule is the core TCL-901 evidence on the
// compiler side: the authored mapped row reaches the shared IR as the IPv4
// prefix both engines can match. Pre-fix this value was "::ffff:10.0.0.0/104".
func TestMappedCIDRRowCompilesToIPv4Rule(t *testing.T) {
	compiled, err := CompileFilteredNetworkRules(NetworkRules{
		Mode:   AccessModeOpen,
		Deny:   []NetworkAllowEntry{{CIDR: "::ffff:10.0.0.0/104"}},
		Engine: NetworkEngineProxy,
	})
	if err != nil {
		t.Fatalf("CompileFilteredNetworkRules: %v", err)
	}
	if len(compiled.DenyRules) != 1 {
		t.Fatalf("deny rules = %d, want 1", len(compiled.DenyRules))
	}
	if got := compiled.DenyRules[0].Value; got != "10.0.0.0/8" {
		t.Fatalf("compiled deny cidr = %q, want %q", got, "10.0.0.0/8")
	}
}

// TestMappedCIDRRowRendersIPv4NFTRule is the cross-engine half of the same
// contract. Pre-fix the renderer keyed its family on Addr().Is4(), which is
// false for a 4-in-6 prefix, and emitted "ip6 daddr ::ffff:10.0.0.0/104" —
// rules no packet toward routable IPv4 can ever match. One authored row must
// not mean different things per engine.
func TestMappedCIDRRowRendersIPv4NFTRule(t *testing.T) {
	compiled, err := CompileFilteredNetworkRules(NetworkRules{
		Mode:   AccessModeOpen,
		Deny:   []NetworkAllowEntry{{CIDR: "::ffff:10.0.0.0/104"}},
		Engine: NetworkEnginePacket,
	})
	if err != nil {
		t.Fatalf("CompileFilteredNetworkRules: %v", err)
	}
	nft, err := RenderFilteredNetworkNFT(compiled)
	if err != nil {
		t.Fatalf("RenderFilteredNetworkNFT: %v", err)
	}
	for _, want := range []string{
		"ip daddr 10.0.0.0/8 meta l4proto tcp drop",
		"ip daddr 10.0.0.0/8 meta l4proto udp drop",
	} {
		if !strings.Contains(nft, want) {
			t.Fatalf("nft policy missing %q:\n%s", want, nft)
		}
	}
	if strings.Contains(nft, "::ffff:") {
		t.Fatalf("nft policy still carries a mapped destination:\n%s", nft)
	}
}

// TestMappedLoopbackCIDRRowStillRefused pins TCL-899 under the new code path.
// Unmapping runs before the loopback refusal, so these arrive as IPv4 and are
// caught by the IPv4 entries of the one identity list rather than its mapped
// entries. The refusal must survive that change of route, with its authored
// remedy intact (the wording corrected in #1801/#1811).
func TestMappedLoopbackCIDRRowStillRefused(t *testing.T) {
	for _, cidr := range []string{
		"::ffff:127.0.0.1/128",
		"::ffff:127.0.0.0/104",
		"::ffff:0.0.0.0/104",
		"::ffff:0:0/96",
	} {
		t.Run(cidr, func(t *testing.T) {
			_, err := CompileFilteredNetworkRules(NetworkRules{
				Mode:   AccessModeList,
				Allow:  []NetworkAllowEntry{{CIDR: cidr}},
				Engine: NetworkEngineProxy,
			})
			if err == nil {
				t.Fatalf("cidr %q was accepted; want the loopback refusal", cidr)
			}
			if !strings.Contains(err.Error(), `use {"loopback": true} instead`) {
				t.Fatalf("cidr %q refused with %v, want the loopback remedy", cidr, err)
			}
		})
	}
}

// TestMappedBlockPrefixBelowNinetySixStillRefused is the boundary that keeps
// the mapped entries in loopbackIdentityPrefixes load-bearing. A prefix shorter
// than /96 reaches outside the mapped block, so UnmapPrefix cannot rewrite it
// and no IPv4 entry of the identity list can catch it: ::fffe:0:0/95 contains
// neither :: nor ::1. Only the mapped entries refuse it. Without them the row
// would be authorable and INERT — the exact TCL-901 shape this ticket closes.
func TestMappedBlockPrefixBelowNinetySixStillRefused(t *testing.T) {
	const cidr = "::ffff:0:0/95"
	if got := UnmapPrefix(netip.MustParsePrefix(cidr).Masked()).Addr().Is4(); got {
		t.Fatalf("UnmapPrefix rewrote %s; the boundary this test guards has moved", cidr)
	}
	_, err := CompileFilteredNetworkRules(NetworkRules{
		Mode:   AccessModeList,
		Allow:  []NetworkAllowEntry{{CIDR: cidr}},
		Engine: NetworkEngineProxy,
	})
	if err == nil {
		t.Fatalf("cidr %q was accepted; want the loopback refusal", cidr)
	}
	if !strings.Contains(err.Error(), `use {"loopback": true} instead`) {
		t.Fatalf("cidr %q refused with %v, want the loopback remedy", cidr, err)
	}
}

// TestUnmapPrefixIsAddressSetPreserving pins the property the whole change
// rests on: the rewritten prefix names exactly the addresses the authored one
// named, so "the same row" really is the same row. Asserted on the mapped
// image of each address rather than by string comparison, because that is the
// question the evaluator actually asks.
func TestUnmapPrefixIsAddressSetPreserving(t *testing.T) {
	prefixes := []string{
		"::ffff:10.0.0.0/104", "::ffff:172.16.0.0/108",
		"::ffff:192.168.1.0/120", "::ffff:8.8.8.8/128",
	}
	probes := []string{
		"10.0.0.1", "10.255.255.255", "11.0.0.1", "172.16.0.1", "172.32.0.1",
		"192.168.1.7", "192.168.2.7", "8.8.8.8", "8.8.4.4", "0.0.0.0",
	}
	for _, spelling := range prefixes {
		authored := netip.MustParsePrefix(spelling)
		unmapped := UnmapPrefix(authored)
		for _, probe := range probes {
			v4 := netip.MustParseAddr(probe)
			mapped := netip.AddrFrom16(v4.As16())
			if got, want := unmapped.Contains(v4), authored.Contains(mapped); got != want {
				t.Fatalf("%s: Contains(%s) = %v after unmap, %v as authored",
					spelling, probe, got, want)
			}
		}
	}
}

// TestNoCIDRSpellingBecomesNewlyRefused guards the compat-break direction that
// the other refusal tests do not: a spelling that USED to be accepted must not
// start being refused. Resolve re-normalizes every tier at launch, so a newly
// refused form would break profiles already on disk — the same migration cost
// that argued against refusing rather than normalizing in the first place.
func TestNoCIDRSpellingBecomesNewlyRefused(t *testing.T) {
	accepted := []string{
		// Mapped rows over routable space: the rows this ticket repairs.
		"::ffff:10.0.0.0/104", "::ffff:192.168.1.0/120", "::ffff:8.8.8.8/128",
		// Ordinary IPv4 and IPv6 rows, which unmap must not disturb.
		"10.0.0.0/8", "192.168.0.0/16", "203.0.113.0/24",
		"2001:db8::/32", "fd00::/8", "2606:4700::/32",
	}
	for _, cidr := range accepted {
		t.Run(cidr, func(t *testing.T) {
			if _, err := CompileFilteredNetworkRules(NetworkRules{
				Mode:   AccessModeList,
				Allow:  []NetworkAllowEntry{{CIDR: cidr}},
				Engine: NetworkEngineProxy,
			}); err != nil {
				t.Fatalf("cidr %q is now refused (%v); it was accepted before", cidr, err)
			}
		})
	}
}

// TestPersistedProfileWithMappedCIDRNormalizesOnLoad proves the benefit that
// justified normalizing over refusing: a profile ALREADY on disk carrying a
// mapped row is repaired when it is loaded, without the operator editing it.
// The input is decoded JSON, not an in-memory authored entry, so the test
// exercises the persisted shape rather than the authoring call.
func TestPersistedProfileWithMappedCIDRNormalizesOnLoad(t *testing.T) {
	const persisted = `{
		"name": "tcl901",
		"network": {
			"baseline": "allow",
			"deny": [{"cidr": "::ffff:10.0.0.0/104"}]
		}
	}`
	var profile Profile
	if err := json.Unmarshal([]byte(persisted), &profile); err != nil {
		t.Fatalf("unmarshal persisted profile: %v", err)
	}
	if profile.Network == nil || len(profile.Network.Deny) != 1 ||
		profile.Network.Deny[0].CIDR != "::ffff:10.0.0.0/104" {
		t.Fatalf("persisted profile did not decode as authored: %+v", profile.Network)
	}
	normalized, _, err := NormalizeForPersistence(profile)
	if err != nil {
		t.Fatalf("NormalizeForPersistence: %v", err)
	}
	if got := normalized.Network.Deny[0].CIDR; got != "10.0.0.0/8" {
		t.Fatalf("persisted deny cidr = %q, want %q", got, "10.0.0.0/8")
	}
}

// TestMappedCIDRAllowIntersectsAcrossTiers covers a SECOND behavior change that
// rides along with normalization, on the allow side rather than the deny side.
// The tier intersection compares parsed prefixes and requires equal BitLen
// (access_rules.go networkSelectorCovers / intersectNetworkSelector), so before
// this change a mapped allow row and its IPv4 twin did not intersect and the
// destination was dropped from the effective policy entirely. Normalizing
// upstream makes them the same row, so the grant survives composition. That is
// a widening, and it is asserted here explicitly rather than left implicit.
func TestMappedCIDRAllowIntersectsAcrossTiers(t *testing.T) {
	global := Profile{
		Name: "global",
		Network: &NetworkRules{
			Mode:  AccessModeList,
			Allow: []NetworkAllowEntry{{CIDR: "10.0.0.0/8"}},
		},
	}
	explicit := Profile{
		Name: "explicit",
		Network: &NetworkRules{
			Mode:  AccessModeList,
			Allow: []NetworkAllowEntry{{CIDR: "::ffff:10.0.0.0/104"}},
		},
	}
	resolved, err := Resolve(Scopes{Global: &global, Explicit: &explicit})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Network == nil {
		t.Fatalf("resolved profile carries no network rules")
	}
	var got []string
	for _, entry := range resolved.Network.Allow {
		got = append(got, entry.CIDR)
	}
	if len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("effective allow cidrs = %v, want [10.0.0.0/8]", got)
	}
}
