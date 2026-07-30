package sandboxproxy

import (
	"net/netip"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func mustEvaluator(t *testing.T, rules sandboxpolicy.NetworkRules) *Evaluator {
	t.Helper()
	evaluator, err := NewEvaluator(rules)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return evaluator
}

func mustTarget(t *testing.T, host string, port int) Target {
	t.Helper()
	target, err := ParseTarget(host, port)
	if err != nil {
		t.Fatalf("ParseTarget(%q, %d): %v", host, port, err)
	}
	return target
}

// listRules is the policy shape most cases use: a deny baseline materialized
// into a list, which is what a discriminating profile resolves to.
func listRules(
	allow []sandboxpolicy.NetworkAllowEntry,
	deny []sandboxpolicy.NetworkAllowEntry,
) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: allow,
		Deny:  deny,
	}
}

func openRules(
	deny []sandboxpolicy.NetworkAllowEntry,
) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeOpen,
		Deny: deny,
	}
}

func TestEvaluateRequestedTarget(t *testing.T) {
	domainList := listRules(
		[]sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", IncludeSubdomains: true, Ports: []int{443}},
		}, nil)
	denyOverlap := listRules(
		[]sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", IncludeSubdomains: true},
		},
		[]sandboxpolicy.NetworkAllowEntry{{Host: "evil.example.com"}},
	)
	cidrList := listRules(
		[]sandboxpolicy.NetworkAllowEntry{
			{CIDR: "10.20.0.0/16", Ports: []int{5432}},
		}, nil)
	loopbackList := listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{8080}}},
		nil)
	hostOnly := listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Host: "localhost"}}, nil)

	for _, tc := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
		host  string
		port  int
		want  Verdict
	}{
		{"subdomain matches an include_subdomains rule", domainList,
			"api.example.com", 443, VerdictAllowed},
		{"the apex matches its own rule", domainList,
			"example.com", 443, VerdictAllowed},
		{"a sibling name is label-bound out", domainList,
			"badexample.com", 443, VerdictNotAuthorized},
		{"a deeper label still matches", domainList,
			"a.b.example.com", 443, VerdictAllowed},
		{"port narrowing refuses another port", domainList,
			"example.com", 80, VerdictNotAuthorized},
		{"a trailing dot is the same name", domainList,
			"example.com.", 443, VerdictAllowed},
		{"case folds to the authored spelling", domainList,
			"API.Example.COM", 443, VerdictAllowed},

		{"a deny row beats an overlapping allow", denyOverlap,
			"evil.example.com", 443, VerdictDeniedByRule},
		{"the overlap does not refuse its siblings", denyOverlap,
			"other.example.com", 443, VerdictAllowed},

		{"a literal inside an authored cidr is allowed", cidrList,
			"10.20.0.5", 5432, VerdictAllowed},
		{"a literal outside the authored ports is refused", cidrList,
			"10.20.0.5", 5433, VerdictNotAuthorized},
		{"a literal outside the authored range is refused", cidrList,
			"10.21.0.5", 5432, VerdictNotAuthorized},
		{"a name is never resolved and cidr-matched", cidrList,
			"db.example.com", 5432, VerdictNotAuthorized},

		{"an authored loopback rule admits the ipv4 literal", loopbackList,
			"127.0.0.1", 8080, VerdictAllowed},
		{"an authored loopback rule admits the ipv6 literal", loopbackList,
			"::1", 8080, VerdictAllowed},
		{"an authored loopback rule admits the name", loopbackList,
			"localhost", 8080, VerdictAllowed},
		{"an unauthored loopback port is refused", loopbackList,
			"127.0.0.1", 9090, VerdictNotAuthorized},
		{"a host rule is not host-loopback authority", hostOnly,
			"localhost", 80, VerdictNotAuthorized},
		{"a host rule is not loopback-literal authority", hostOnly,
			"127.0.0.1", 80, VerdictNotAuthorized},

		{"an open baseline allows what it does not deny",
			openRules([]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}}),
			"example.com", 443, VerdictAllowed},
		{"an open baseline still honors its denies",
			openRules([]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}}),
			"tracker.example", 443, VerdictDeniedByRule},
		{"an open baseline does not reach host loopback",
			openRules([]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}}),
			"127.0.0.1", 8080, VerdictNotAuthorized},
		{"an open baseline does not reach the loopback name",
			openRules([]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}}),
			"localhost", 8080, VerdictNotAuthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := mustEvaluator(t, tc.rules)
			decision := evaluator.Evaluate(mustTarget(t, tc.host, tc.port))
			if decision.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", decision.Verdict, tc.want)
			}
			if decision.Allowed() {
				return
			}
			if decision.Detail == "" {
				t.Fatal("a refusal must carry a legible reason")
			}
		})
	}
}

func TestEvaluateDenyRowRefusesTheLoopbackName(t *testing.T) {
	// A deny row may never be narrowed by the rule that only the loopback
	// selector grants host loopback: refusing more is always within intent.
	evaluator := mustEvaluator(t, listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
		[]sandboxpolicy.NetworkAllowEntry{{Host: "localhost"}},
	))
	// Both spellings of host loopback are one identity, so the deny row must
	// refuse the literal exactly as it refuses the name.
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		decision := evaluator.Evaluate(mustTarget(t, host, 8080))
		if decision.Verdict != VerdictDeniedByRule {
			t.Fatalf("%s verdict = %q, want %q",
				host, decision.Verdict, VerdictDeniedByRule)
		}
	}
}

func TestEvaluateDenyLoopbackRow(t *testing.T) {
	evaluator := mustEvaluator(t, listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
		[]sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{22}}},
	))
	if got := evaluator.Evaluate(mustTarget(t, "127.0.0.1", 22)); got.Verdict !=
		VerdictDeniedByRule {
		t.Fatalf("verdict = %q, want %q", got.Verdict, VerdictDeniedByRule)
	}
	if got := evaluator.Evaluate(mustTarget(t, "127.0.0.1", 8080)); !got.Allowed() {
		t.Fatalf("verdict = %q, want allowed", got.Verdict)
	}
}

func TestEvaluateResolvedAddressBlocksReservedSpace(t *testing.T) {
	nameList := listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "example.com"}}, nil)
	withCIDR := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com"},
		{CIDR: "10.0.0.0/8", Ports: []int{443}},
	}, nil)
	withLoopback := listRules([]sandboxpolicy.NetworkAllowEntry{
		{Domain: "example.com"},
		{Loopback: true, Ports: []int{443}},
	}, nil)
	openDeny := openRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "tracker.example"}})

	for _, tc := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
		addr  string
		port  int
		want  Verdict
	}{
		{"a public address is reached", nameList,
			"93.184.216.34", 443, VerdictAllowed},
		{"rfc1918 space is refused", nameList,
			"10.0.0.5", 443, VerdictPrivateDestination},
		{"loopback is refused", nameList,
			"127.0.0.1", 443, VerdictPrivateDestination},
		{"link-local metadata is refused", nameList,
			"169.254.169.254", 443, VerdictPrivateDestination},
		{"cgnat space is refused", nameList,
			"100.64.0.1", 443, VerdictPrivateDestination},
		{"the ipv6 ula range is refused", nameList,
			"fd00::1", 443, VerdictPrivateDestination},
		{"ipv6 link-local is refused", nameList,
			"fe80::1", 443, VerdictPrivateDestination},
		{"the unspecified address is refused", nameList,
			"0.0.0.0", 443, VerdictPrivateDestination},
		{"this-network space is refused", nameList,
			"0.1.2.3", 443, VerdictPrivateDestination},
		{"multicast is refused", nameList,
			"224.0.0.1", 443, VerdictPrivateDestination},
		{"an ipv4-mapped private address cannot present a second identity",
			nameList, "::ffff:10.0.0.5", 443, VerdictPrivateDestination},

		{"an explicit cidr carves out its own range", withCIDR,
			"10.0.0.5", 443, VerdictAllowed},
		{"the carve-out does not exceed its authored ports", withCIDR,
			"10.0.0.5", 8443, VerdictPrivateDestination},
		{"a loopback row carves out loopback", withLoopback,
			"127.0.0.1", 443, VerdictAllowed},
		{"the loopback carve-out does not exceed its ports", withLoopback,
			"127.0.0.1", 8443, VerdictPrivateDestination},

		{"an open baseline reaches private space", openDeny,
			"10.0.0.5", 443, VerdictAllowed},
		{"an open baseline reaches link-local space", openDeny,
			"169.254.169.254", 443, VerdictAllowed},
		{"an open baseline still does not reach host loopback", openDeny,
			"127.0.0.1", 443, VerdictPrivateDestination},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := mustEvaluator(t, tc.rules)
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("ParseAddr(%q): %v", tc.addr, err)
			}
			target := mustTarget(t, "example.com", tc.port)
			decision := evaluator.EvaluateResolvedAddress(target, addr)
			if decision.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", decision.Verdict, tc.want)
			}
			if !decision.Allowed() && decision.Detail == "" {
				t.Fatal("a refusal must carry a legible reason")
			}
		})
	}
}

func TestParseTargetClassifiesKind(t *testing.T) {
	name := mustTarget(t, "Example.COM.", 443)
	if name.Kind != TargetKindName || name.Name != "example.com" {
		t.Fatalf("parsed %+v, want a normalized name target", name)
	}
	literal := mustTarget(t, "::ffff:10.0.0.5", 443)
	if literal.Kind != TargetKindLiteral ||
		literal.Addr != netip.MustParseAddr("10.0.0.5") {
		t.Fatalf("parsed %+v, want an unmapped literal target", literal)
	}
	for _, bad := range []struct {
		host string
		port int
	}{
		{"example.com", 0},
		{"example.com", 65536},
		{"", 443},
		{"exa mple.com", 443},
		{"fe80::1%eth0", 443},
		{"http://example.com", 443},
	} {
		if _, err := ParseTarget(bad.host, bad.port); err == nil {
			t.Fatalf("ParseTarget(%q, %d) accepted an invalid target",
				bad.host, bad.port)
		}
	}
}

func TestNewEvaluatorRejectsUnmaterializedRules(t *testing.T) {
	_, err := NewEvaluator(sandboxpolicy.NetworkRules{
		Baseline: sandboxpolicy.NetworkBaselineDeny,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com"},
		},
	})
	if err == nil {
		t.Fatal("expected unmaterialized launch intent to be refused")
	}
}

// TestLoopbackIdentityIsOneDomainEverywhere pins the property whose absence
// produced a deny bypass: every spelling of the host is governed by the
// loopback selector, on both polarities and at both evaluation stages. A strict
// predicate in one place and a broad one in another is exactly how a deny row
// authored against the loopback selector went unmatched.
func TestLoopbackIdentityIsOneDomainEverywhere(t *testing.T) {
	// 0.0.0.0 and :: reach the host, and Linux routes 0.0.0.0/8 there, so all
	// of these must behave exactly as 127.0.0.1 does.
	spellings := []string{"127.0.0.1", "::1", "0.0.0.0", "::", "0.5.6.7"}

	denyPolicy := listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "example.com"}, {Loopback: true}},
		[]sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{22}}},
	)
	allowPolicy := listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{8080}}}, nil)

	for _, spelling := range spellings {
		addr := netip.MustParseAddr(spelling)

		t.Run("a loopback deny refuses a name resolving to "+spelling, func(t *testing.T) {
			got := mustEvaluator(t, denyPolicy).EvaluateResolvedAddress(
				mustTarget(t, "example.com", 22), addr)
			if got.Verdict != VerdictDeniedByRule {
				t.Fatalf("verdict = %q, want %q", got.Verdict, VerdictDeniedByRule)
			}
		})

		t.Run("a loopback deny refuses a literal "+spelling, func(t *testing.T) {
			got := mustEvaluator(t, denyPolicy).Evaluate(mustTarget(t, spelling, 22))
			if got.Verdict != VerdictDeniedByRule {
				t.Fatalf("verdict = %q, want %q", got.Verdict, VerdictDeniedByRule)
			}
		})

		t.Run("a loopback allow admits a literal "+spelling, func(t *testing.T) {
			got := mustEvaluator(t, allowPolicy).Evaluate(
				mustTarget(t, spelling, 8080))
			if !got.Allowed() {
				t.Fatalf("verdict = %q, want allowed by the loopback row",
					got.Verdict)
			}
		})

		t.Run("no loopback row refuses a literal "+spelling, func(t *testing.T) {
			evaluator := mustEvaluator(t, listRules(
				[]sandboxpolicy.NetworkAllowEntry{{Domain: "example.com"}}, nil))
			if got := evaluator.Evaluate(mustTarget(t, spelling, 8080)); got.Allowed() {
				t.Fatalf("%s was allowed with no authored loopback row", spelling)
			}
		})
	}
}
