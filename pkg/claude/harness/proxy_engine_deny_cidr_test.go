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

// TestLoopbackIdentityCIDRRowsAreUnauthorable is TCL-899's evidence.
//
// The defect: Evaluator.match takes its loopback branch for every target in the
// host-loopback identity space — which includes the unspecified address and all
// of 0.0.0.0/8 — and that branch consults loopback rows alone. A cidr row
// overlapping that space was therefore authorable but INERT: with
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
// Falsifiability: revert loopbackIdentityPrefixes to the pre-fix 127.0.0.0/8 +
// ::1 list and (1) fails — the rows compile. That differing pre-fix value is
// what makes this evidence rather than agreement with whatever the code does.
// Precisely: it differs for the five spellings TCL-899 added; the three
// pre-existing ones below are regression cover, and they are honestly not
// falsified by that revert because the old predicate already refused them.
//
// Baseline scope, stated rather than implied: normalizeNetworkAllowEntry is
// mode-independent, so open and list run the same code path through assertion
// 1's deny polarity. The list baseline is not decoration anyway — it is the
// only one where the allow polarity is authorable at all, and the only one
// where assertion 3 shows a loopback deny beating an authored loopback ALLOW,
// which is the exact shape the escape lived in.
func TestLoopbackIdentityCIDRRowsAreUnauthorable(t *testing.T) {
	// Every spelling of the space match() claims. 127.0.0.0/8 and ::1/128 were
	// already refused before TCL-899 and are kept so a later narrowing of the
	// predicate cannot pass this test.
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
					assert.Containsf(t, err.Error(), `use {"loopback": true} instead`,
						"the refusal must name the remedy, not just refuse: %s cidr %q", polarity, cidr)
					assert.Containsf(t, err.Error(), "host-loopback identity space",
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

// The compiler and the proxy evaluator must not drift back into two
// definitions of the host-loopback identity space. That divergence IS the
// defect: the evaluator's branch is complete only for exactly the space the
// compiler keeps cidr rows out of. Wider compiler than evaluator refuses rows
// that would have worked; narrower compiler than evaluator readmits the inert
// shape TCL-899 closed.
func TestLoopbackIdentityAgreesBetweenCompilerAndEvaluator(t *testing.T) {
	for _, spelling := range []string{
		"0.0.0.0", "0.255.255.255", "127.0.0.1", "127.255.255.254", "::", "::1",
	} {
		addr := netip.MustParseAddr(spelling)
		require.Truef(t, sandboxpolicy.AddrIsLoopbackIdentity(addr),
			"%s is host loopback to the evaluator", spelling)
		single := netip.PrefixFrom(addr, addr.BitLen())
		assert.Truef(t, sandboxpolicy.PrefixIntersectsLoopbackIdentity(single),
			"%s is decided by loopback rows, so a cidr row naming it must not compile",
			spelling)
	}
	// The converse: an address the evaluator treats as routable must stay
	// authorable as a cidr row, or the gate has swallowed ordinary space.
	for _, spelling := range []string{"1.0.0.0", "93.184.216.34", "2001:db8::1", "fd00::2"} {
		addr := netip.MustParseAddr(spelling)
		require.Falsef(t, sandboxpolicy.AddrIsLoopbackIdentity(addr),
			"%s is a routable destination to the evaluator", spelling)
		single := netip.PrefixFrom(addr, addr.BitLen())
		assert.Falsef(t, sandboxpolicy.PrefixIntersectsLoopbackIdentity(single),
			"%s must remain authorable as a cidr row", spelling)
	}
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
