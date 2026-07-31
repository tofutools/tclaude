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
				assert.Equalf(t, EnforceFull, capability.Level,
					"the evaluator refused both the literal and the name resolving into the range, so %s's deny cidr cell is Full",
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
