package harness

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

// TestProxyEngineDenyCIDRRatingMatchesTheEvaluator is TCL-890's evidence, and
// the shape it takes is the one that caught the deny-NAME over-claim: ask the
// real evaluator, in both baselines, for the escape the cell would have to be
// vulnerable to before a lower rating is honest.
//
// The defect was an under-claim carried by a wrong sentence: the deny cell
// reused the ALLOW cidr detail, whose middle sentence says a name resolving
// into the range is not admitted by the rule. For the deny polarity that is
// backwards. Dialer.Connect asks EvaluateResolvedAddress per resolved
// candidate, which re-applies cidr DENY rows to the literal under both
// baselines, so a name resolving into a denied range is refused.
func TestProxyEngineDenyCIDRRatingMatchesTheEvaluator(t *testing.T) {
	const denied = "denied.example.com"
	const deniedAddr = "93.184.216.34"
	denyCIDR := []sandboxpolicy.NetworkAllowEntry{
		{CIDR: "93.184.216.0/24", Ports: []int{443}},
	}

	for _, baseline := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{
			// Default-allow: everything but the denies is authorized, so the
			// deny row is the only thing standing between the client and the
			// destination.
			name: "open baseline",
			rules: sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeOpen,
				Deny:   denyCIDR,
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
		{
			// Allowlist that authorizes the NAME. This is the shape the escape
			// would live in if there were one: the target is admitted before
			// resolution, so only the resolution-stage deny can stop it.
			name: "list baseline authorizing the name",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: denied, Ports: []int{443}},
				},
				Deny:   denyCIDR,
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
	} {
		t.Run(baseline.name, func(t *testing.T) {
			evaluator, err := sandboxproxy.NewEvaluator(baseline.rules)
			require.NoError(t, err)

			byLiteral, err := sandboxproxy.ParseTarget(deniedAddr, 443)
			require.NoError(t, err)
			literalDecision := evaluator.Evaluate(byLiteral)
			assert.Equal(t, sandboxproxy.VerdictDeniedByRule,
				literalDecision.Verdict,
				"a literal in the denied range must be refused BY THE RULE, not incidentally")

			// The interesting half: the client asks by NAME. Under the list
			// baseline the name is authorized, so the pre-resolution decision
			// allows it and only the resolution stage can refuse.
			byName, err := sandboxproxy.ParseTarget(denied, 443)
			require.NoError(t, err)
			if baseline.rules.Mode == sandboxpolicy.AccessModeList {
				require.True(t, evaluator.Evaluate(byName).Allowed(),
					"the name must be authorized pre-resolution, or this baseline tests nothing")
			}

			dialer := &sandboxproxy.Dialer{
				Resolve: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr(deniedAddr)}, nil
				},
				DialAddr: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("a denied address must never be dialed")
					return nil, nil
				},
			}
			conn, decision, err := dialer.Connect(
				context.Background(), evaluator, byName)
			assert.Nil(t, conn)
			require.Error(t, err)
			assert.Equal(t, sandboxproxy.VerdictDeniedByRule, decision.Verdict,
				"the name resolved into the denied range and must be refused by that rule")

			// Only now the rating, against what was just observed.
			for _, activated := range activatedProxyEngineHarnesses(t) {
				predicted, err := PredictAccessEnforcement(
					activated, sandboxpolicy.ImplementationTclaudeLayer,
					sandboxpolicy.ResolvedAxes{Network: baseline.rules}, "", "linux",
				)
				require.NoErrorf(t, err, "harness %s", activated.Name)
				capability, ok := networkSelectorCapability(
					predicted.NetworkDenySelectors,
					string(sandboxpolicy.NetworkSelectorCIDR))
				require.Truef(t, ok, "harness %s", activated.Name)
				assert.Equalf(t, EnforcePartial, capability.Level,
					"%s: the evaluator refuses the literal and the name, but the escapes below keep this cell short of Full",
					activated.Name)
				// Contains, not Equal: OpenCode appends its own launch-gate
				// caveat to every deny selector detail, and that is its row's
				// business rather than this cell's polarity.
				assert.Containsf(t, capability.Detail,
					ProxyEngineDenyCIDRSelectorDetail,
					"%s's deny cidr cell must not borrow the allow-shaped detail",
					activated.Name)
				assert.NotContainsf(t, capability.Detail,
					"is not admitted by this rule",
					"%s: the allow detail's central sentence is false for the deny polarity",
					activated.Name)

				// The allow side is out of TCL-890's scope and must stay put:
				// a cidr ALLOW row really does not admit a name, because the
				// name is authorized before any address exists.
				allow, ok := networkSelectorCapability(
					predicted.NetworkSelectors,
					string(sandboxpolicy.NetworkSelectorCIDR))
				if ok {
					assert.Equalf(t, EnforcePartial, allow.Level,
						"%s: the allow cidr rating is correct as written and not what this changes",
						activated.Name)
					assert.Containsf(t, allow.Detail,
						ProxyEngineCIDRSelectorDetail, "harness %s", activated.Name)
				}
			}
		})
	}
}

// TCL-890 scope item 2: is Partial still the right LEVEL, given the
// resolution-stage re-application above? Yes — asserted against the real
// evaluator rather than asserted about it, because a rating kept for a reason
// nobody re-checked is how the last one went wrong.
//
// TCL-890 recorded two reasons. TCL-899 closed the first, so ONE remains, and
// the level is now carried entirely by it: revisit Full if this test ever
// stops reproducing it. It is not the escape the old allow-shaped string
// described, which is why the wording changed even though the level did not.
func TestProxyEngineDenyCIDREscapesThatKeepItPartial(t *testing.T) {
	// The host-loopback identity escape this test used to assert is CLOSED
	// (TCL-899). It is not merely absent here: TestLoopbackIdentityCIDRRowsAre
	// Unauthorable below is its replacement, and it asserts the closure the
	// same way this test asserts the remaining escape — against the real
	// compiler and the real evaluator, in both baselines.

	// An address is not a destination. The same host restated in another
	//    address family is a different address, and an IPv4 rule does not
	//    match it. Under a default-allow baseline that form is reachable.
	openBaseline := sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeOpen,
		Deny:   []sandboxpolicy.NetworkAllowEntry{{CIDR: "93.184.216.0/24"}},
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
	openEvaluator, err := sandboxproxy.NewEvaluator(openBaseline)
	require.NoError(t, err)
	v4, err := sandboxproxy.ParseTarget("93.184.216.34", 443)
	require.NoError(t, err)
	require.Equal(t, sandboxproxy.VerdictDeniedByRule,
		openEvaluator.Evaluate(v4).Verdict,
		"the v4 literal must be denied, or the embedding below proves nothing")
	for _, embedded := range []string{
		"64:ff9b::5db8:d822", // NAT64
		"2002:5db8:d822::",   // 6to4
	} {
		target, parseErr := sandboxproxy.ParseTarget(embedded, 443)
		require.NoError(t, parseErr)
		assert.Truef(t, openEvaluator.Evaluate(target).Allowed(),
			"%s is the denied destination in another address family and is not matched by the v4 rule",
			embedded)
	}

	// The disclosure names the escape that remains, so an operator reading the
	// cell learns what to author rather than discovering it.
	assert.Contains(t, ProxyEngineDenyCIDRSelectorDetail, "NAT64 or 6to4")
	// And it must NOT still name the one that is gone. A disclosure that keeps
	// claiming a closed escape teaches an operator to author a workaround for
	// a hole that no longer exists, which is its own kind of false statement.
	assert.NotContains(t, ProxyEngineDenyCIDRSelectorDetail,
		"decided by loopback rules alone",
		"the loopback-identity escape is closed (TCL-899); the cell must stop disclosing it")
}

// TestLoopbackRowAuthorityCIDRRowsAreUnauthorable is TCL-899's evidence.
//
// The defect: Evaluator.match takes its loopback branch for every target in the
// loopback-row authority space, and that branch consults loopback rows alone.
// A cidr row overlapping that space was therefore authorable but INERT: with
// allow loopback:8080 + deny cidr 0.0.0.0/8, both the literal and an allowed
// name resolving to 0.0.0.0 were dialed. An operator believed a deny existed
// that never fired, which is the worst failure a policy surface has.
//
// The fix moves the refusal to authoring time, so the shape cannot exist. Three
// things have to hold for that to be an honest answer rather than a blunt one,
// and each is asserted here in BOTH baselines:
//
//  1. the previously-inert shape is refused, with the capability-phrased
//     message naming the loopback selector as the remedy;
//  2. the gate DISCRIMINATES — an ordinary cidr deny still compiles and still
//     denies, so this is not "cidr rows got harder to author";
//  3. the remedy the message names actually works: a loopback deny refuses the
//     very target the inert row failed to stop, with VerdictDeniedByRule rather
//     than a bare Allowed() == false.
//
// Falsifiability: removing an exact unspecified entry from
// loopbackRowAuthorityPrefixes makes its corresponding row compile; bypassing
// the compiler gate makes every row compile. The authorable-and-effective test
// below separately fails if the compiler is switched back to the broad identity
// list.
//
// Baseline scope, stated rather than implied: normalizeNetworkAllowEntry is
// mode-independent, so open and list run the same code path through assertion
// 1's deny polarity. The list baseline is not decoration anyway — it is the
// only one where the allow polarity is authorable at all, and the only one
// where assertion 3 shows a loopback deny beating an authored loopback ALLOW,
// which is the exact shape the escape lived in.
func TestLoopbackRowAuthorityCIDRRowsAreUnauthorable(t *testing.T) {
	// Every prefix overlaps at least one address match() decides from loopback
	// rows. The broad 0/8 spellings stay refused because they contain the exact
	// unspecified address; individual non-unspecified addresses are tested as
	// authorable below.
	inert := []string{
		"0.0.0.0/8",            // the reported shape
		"0.0.0.0/32",           // the unspecified address alone
		"0.0.0.0/7",            // a range merely OVERLAPPING the space
		"::/128",               // unspecified, v6
		"::ffff:0.0.0.0/104",   // the v4 "this network" space, v6-spelled
		"127.0.0.0/8",          // pre-existing coverage, must not regress
		"::1/128",              //
		"::ffff:127.0.0.1/128", //
	}

	for _, baseline := range []struct {
		name  string
		rules func(deny []sandboxpolicy.NetworkAllowEntry) sandboxpolicy.NetworkRules
	}{
		{
			// Default-allow: the deny row is the only thing standing between
			// the client and the destination.
			name: "open",
			rules: func(deny []sandboxpolicy.NetworkAllowEntry) sandboxpolicy.NetworkRules {
				return sandboxpolicy.NetworkRules{
					Mode: sandboxpolicy.AccessModeOpen, Deny: deny,
					Engine: sandboxpolicy.NetworkEngineProxy,
				}
			},
		},
		{
			// Allowlist that AUTHORIZES host loopback, which is the shape the
			// escape actually lived in: the target is admitted by the loopback
			// allow row, so only the cidr deny could have stopped it.
			name: "list",
			rules: func(deny []sandboxpolicy.NetworkAllowEntry) sandboxpolicy.NetworkRules {
				return sandboxpolicy.NetworkRules{
					Mode: sandboxpolicy.AccessModeList,
					Allow: []sandboxpolicy.NetworkAllowEntry{
						{Loopback: true, Ports: []int{8080}},
					},
					Deny:   deny,
					Engine: sandboxpolicy.NetworkEngineProxy,
				}
			},
		},
	} {
		t.Run(baseline.name, func(t *testing.T) {
			// 1. The inert shape is refused at authoring time, in both
			//    polarities: an ALLOW cidr row over this space was equally
			//    inert, and the same normalizer governs both.
			// An open baseline has no allow list at all — network.allow is only
			// valid with mode "list" — so the allow polarity is exercised where
			// it is authorable.
			polarities := []string{"deny"}
			if baseline.name == "list" {
				polarities = append(polarities, "allow")
			}
			for _, cidr := range inert {
				for _, polarity := range polarities {
					entry := []sandboxpolicy.NetworkAllowEntry{{CIDR: cidr}}
					rules := baseline.rules(entry)
					if polarity == "allow" {
						rules = baseline.rules(nil)
						rules.Allow = append(rules.Allow, entry...)
					}
					_, err := sandboxproxy.NewEvaluator(rules)
					require.Errorf(t, err,
						"%s cidr %q overlaps the host-loopback identity space and must not compile",
						polarity, cidr)
					assert.Containsf(t, err.Error(), `use {"loopback": true} for that portion`,
						"the refusal must name the remedy, not just refuse: %s cidr %q", polarity, cidr)
					assert.Containsf(t, err.Error(), "split it and keep CIDR rows for the remainder",
						"the refusal must preserve mixed-range intent: %s cidr %q", polarity, cidr)
					assert.Containsf(t, err.Error(), "address space governed by loopback rows",
						"the refusal must name what it refused: %s cidr %q", polarity, cidr)
				}
			}

			// 2. The gate discriminates. An ordinary cidr deny outside the
			//    identity space still compiles AND still denies — otherwise
			//    assertion 1 would be satisfied by refusing everything.
			ordinary := baseline.rules([]sandboxpolicy.NetworkAllowEntry{
				{CIDR: "93.184.216.0/24"},
			})
			evaluator, err := sandboxproxy.NewEvaluator(ordinary)
			require.NoError(t, err, "an ordinary cidr deny must still be authorable")
			routable, err := sandboxproxy.ParseTarget("93.184.216.34", 443)
			require.NoError(t, err)
			assert.Equal(t, sandboxproxy.VerdictDeniedByRule,
				evaluator.Evaluate(routable).Verdict,
				"the ordinary cidr deny must still fire")

			// 3. The remedy the refusal names actually blocks the target the
			//    inert row failed to block. Without this, the message would be
			//    directing an operator at a selector nobody re-checked.
			remedied := baseline.rules([]sandboxpolicy.NetworkAllowEntry{
				{Loopback: true},
			})
			remediedEvaluator, err := sandboxproxy.NewEvaluator(remedied)
			require.NoError(t, err)
			for _, spelling := range []string{"0.0.0.0", "127.0.0.1", "::", "::1"} {
				target, parseErr := sandboxproxy.ParseTarget(spelling, 8080)
				require.NoError(t, parseErr)
				decision := remediedEvaluator.Evaluate(target)
				assert.Equalf(t, sandboxproxy.VerdictDeniedByRule, decision.Verdict,
					"%s is host loopback and the loopback deny must refuse it by rule", spelling)
			}
		})
	}
}

// The compiler and evaluator must agree on loopback-row authority even though
// broad loopback-identity membership is intentionally wider (TCL-916).
func TestLoopbackRowAuthorityAgreesBetweenCompilerAndEvaluator(t *testing.T) {
	for _, spelling := range []string{
		"0.0.0.0", "127.0.0.1", "127.255.255.254", "::", "::1",
	} {
		addr := netip.MustParseAddr(spelling)
		require.Truef(t, sandboxpolicy.AddrHasLoopbackRowAuthority(addr),
			"%s is governed by loopback rows", spelling)
		single := netip.PrefixFrom(addr, addr.BitLen())
		assert.Truef(t, sandboxpolicy.PrefixIntersectsLoopbackRowAuthority(single),
			"%s is decided by loopback rows, so a cidr row naming it must not compile",
			spelling)
	}
	for _, spelling := range []string{
		"0.0.0.1", "::ffff:0.0.0.1", "1.0.0.0", "93.184.216.34", "2001:db8::1", "fd00::2",
	} {
		addr := netip.MustParseAddr(spelling)
		require.Falsef(t, sandboxpolicy.AddrHasLoopbackRowAuthority(addr),
			"%s must not be governed by loopback rows", spelling)
		single := netip.PrefixFrom(addr, addr.BitLen())
		assert.Falsef(t, sandboxpolicy.PrefixIntersectsLoopbackRowAuthority(single),
			"%s must remain authorable as a cidr row", spelling)
	}

	// Membership itself is not narrowed: TCL-910's broader answer remains true
	// for both native and mapped spellings even though row authority is false.
	for _, spelling := range []string{"0.0.0.1", "0.255.255.255", "::ffff:0.0.0.1"} {
		addr := netip.MustParseAddr(spelling)
		assert.Truef(t, sandboxpolicy.AddrIsLoopbackIdentity(addr),
			"%s must remain in the settled broad identity space", spelling)
		assert.Truef(t, sandboxpolicy.PrefixIntersectsLoopbackIdentity(
			netip.PrefixFrom(addr, addr.BitLen())),
			"%s must remain in the settled broad prefix identity space", spelling)
	}
}

// TestNonUnspecifiedZeronetCIDRRowsAreAuthorableAndEffective proves both
// directions of TCL-899's invariant after the authority split: CIDR allows and
// denies compile, and each actually governs direct and resolved targets. Both
// address spelling and port are varied so neither rule parameter is assumed.
func TestNonUnspecifiedZeronetCIDRRowsAreAuthorableAndEffective(t *testing.T) {
	const (
		name       = "example.com"
		authored   = 8080
		unauthored = 8081
	)
	addresses := []string{"0.0.0.1", "::ffff:0.0.0.1"}

	allowEvaluator, err := sandboxproxy.NewEvaluator(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: name},
			{CIDR: "0.0.0.1/32", Ports: []int{authored}},
		},
		Engine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err, "a non-unspecified 0/8 CIDR allow must be authorable")

	denyEvaluator, err := sandboxproxy.NewEvaluator(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeOpen,
		Deny: []sandboxpolicy.NetworkAllowEntry{
			{CIDR: "0.0.0.1/32", Ports: []int{authored}},
		},
		Engine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err, "a non-unspecified 0/8 CIDR deny must be authorable")

	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				evaluator     *sandboxproxy.Evaluator
				port          int
				directWant    sandboxproxy.Verdict
				resolvedWant  sandboxproxy.Verdict
				wantRuleMatch bool
			}{
				{"allow authored port", allowEvaluator, authored,
					sandboxproxy.VerdictAllowed, sandboxproxy.VerdictAllowed, true},
				{"allow other port", allowEvaluator, unauthored,
					sandboxproxy.VerdictNotAuthorized, sandboxproxy.VerdictPrivateDestination, false},
				{"deny authored port", denyEvaluator, authored,
					sandboxproxy.VerdictDeniedByRule, sandboxproxy.VerdictDeniedByRule, true},
				{"deny other port", denyEvaluator, unauthored,
					sandboxproxy.VerdictAllowed, sandboxproxy.VerdictAllowed, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					direct, parseErr := sandboxproxy.ParseTarget(address, tc.port)
					require.NoError(t, parseErr)
					directDecision := tc.evaluator.Evaluate(direct)
					assert.Equal(t, tc.directWant, directDecision.Verdict)

					byName, parseErr := sandboxproxy.ParseTarget(name, tc.port)
					require.NoError(t, parseErr)
					if tc.evaluator == allowEvaluator {
						require.True(t, tc.evaluator.Evaluate(byName).Allowed(),
							"the name must reach resolved-address evaluation")
					}
					resolvedDecision := tc.evaluator.EvaluateResolvedAddress(
						byName, netip.MustParseAddr(address))
					assert.Equal(t, tc.resolvedWant, resolvedDecision.Verdict)
					assert.Equal(t, tc.wantRuleMatch, directDecision.Rule != nil)
					assert.Equal(t, tc.wantRuleMatch, resolvedDecision.Rule != nil)
				})
			}
		})
	}
}

// TestMixedZeronetDenyMigrationPreservesIntent exercises the migration the
// authoring refusal prescribes. A rejected 0/8 deny is replaced by a loopback
// deny for exact 0.0.0.0 plus the canonical CIDR decomposition of the rest;
// every resulting row compiles and fires, while an address outside 0/8 remains
// allowed under the open baseline.
func TestMixedZeronetDenyMigrationPreservesIntent(t *testing.T) {
	_, err := sandboxproxy.NewEvaluator(sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeOpen,
		Deny:   []sandboxpolicy.NetworkAllowEntry{{CIDR: "0.0.0.0/8"}},
		Engine: sandboxpolicy.NetworkEngineProxy,
	})
	require.Error(t, err, "the mixed CIDR must be split before it can be effective")
	assert.ErrorContains(t, err, `use {"loopback": true} for that portion`)
	assert.ErrorContains(t, err, "split it and keep CIDR rows for the remainder")

	addrFromUint32 := func(value uint32) netip.Addr {
		return netip.AddrFrom4([4]byte{
			byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value),
		})
	}
	uint32FromAddr := func(addr netip.Addr) uint32 {
		bytes := addr.As4()
		return uint32(bytes[0])<<24 | uint32(bytes[1])<<16 |
			uint32(bytes[2])<<8 | uint32(bytes[3])
	}

	deny := []sandboxpolicy.NetworkAllowEntry{{Loopback: true}}
	cursor := uint32(1)
	for bits := 32; bits >= 9; bits-- {
		prefix := netip.PrefixFrom(addrFromUint32(cursor), bits)
		require.Equal(t, cursor, uint32FromAddr(prefix.Masked().Addr()),
			"the generated remainder prefix must start at the uncovered cursor")
		deny = append(deny, sandboxpolicy.NetworkAllowEntry{CIDR: prefix.String()})
		cursor += uint32(1) << uint(32-bits)
	}
	require.Equal(t, uint32(1)<<24, cursor,
		"the split rows must cover every non-unspecified address in 0/8")

	evaluator, err := sandboxproxy.NewEvaluator(sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeOpen,
		Deny:   deny,
		Engine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err, "the prescribed split must be authorable")

	zero, err := sandboxproxy.ParseTarget("0.0.0.0", 443)
	require.NoError(t, err)
	assert.Equal(t, sandboxproxy.VerdictDeniedByRule, evaluator.Evaluate(zero).Verdict,
		"the loopback row must preserve the deny at exact 0.0.0.0")

	for _, entry := range deny[1:] {
		prefix := netip.MustParsePrefix(entry.CIDR)
		start := uint32FromAddr(prefix.Addr())
		size := uint32(1) << uint(32-prefix.Bits())
		for _, value := range []uint32{start, start + size - 1} {
			target, parseErr := sandboxproxy.ParseTarget(addrFromUint32(value).String(), 443)
			require.NoError(t, parseErr)
			assert.Equalf(t, sandboxproxy.VerdictDeniedByRule,
				evaluator.Evaluate(target).Verdict,
				"the split deny row %s must fire at %s", entry.CIDR, target.Host())
		}
	}

	outside, err := sandboxproxy.ParseTarget("1.0.0.0", 443)
	require.NoError(t, err)
	assert.Equal(t, sandboxproxy.VerdictAllowed, evaluator.Evaluate(outside).Verdict,
		"the split must not become a blanket deny")
}

// The two polarities must not converge on one string again. A single shared
// detail is how the wrong one got rendered under a second harness.
func TestProxyEngineCIDRDetailsStayDistinctPerPolarity(t *testing.T) {
	assert.NotEqual(t, ProxyEngineCIDRSelectorDetail,
		ProxyEngineDenyCIDRSelectorDetail)
	assert.Contains(t, ProxyEngineCIDRSelectorDetail,
		"is not admitted by this rule",
		"the allow detail keeps the sentence that is true for its own polarity")
	assert.Contains(t, ProxyEngineDenyCIDRSelectorDetail,
		"resolves into this range is refused")
}
