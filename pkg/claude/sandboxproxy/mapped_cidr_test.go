package sandboxproxy

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TestMappedDenyCIDRDeniesInBothBaselines is the TCL-901 evidence against the
// real evaluator. A deny row authored in IPv4-mapped form used to compile
// cleanly and then match nothing: every case below answered VerdictAllowed
// before the fix, under both baselines and for both spellings of the target.
//
// The list baseline carries an allow row covering the same space, so a passing
// result cannot come from the destination merely being unauthorized — it has to
// come from the deny row itself, which is why the assertion is the
// discriminating VerdictDeniedByRule rather than a bare Allowed() == false.
func TestMappedDenyCIDRDeniesInBothBaselines(t *testing.T) {
	baselines := []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{"open", sandboxpolicy.NetworkRules{
			Mode:   sandboxpolicy.AccessModeOpen,
			Deny:   []sandboxpolicy.NetworkAllowEntry{{CIDR: "::ffff:10.0.0.0/104"}},
			Engine: sandboxpolicy.NetworkEngineProxy,
		}},
		{"list", sandboxpolicy.NetworkRules{
			Mode:   sandboxpolicy.AccessModeList,
			Allow:  []sandboxpolicy.NetworkAllowEntry{{CIDR: "10.0.0.0/8"}},
			Deny:   []sandboxpolicy.NetworkAllowEntry{{CIDR: "::ffff:10.0.0.0/104"}},
			Engine: sandboxpolicy.NetworkEngineProxy,
		}},
	}
	for _, baseline := range baselines {
		t.Run(baseline.name, func(t *testing.T) {
			evaluator := mustEvaluator(t, baseline.rules)
			for _, host := range []string{"10.0.0.1", "::ffff:10.0.0.1"} {
				t.Run(host, func(t *testing.T) {
					decision := evaluator.Evaluate(mustTarget(t, host, 443))
					if decision.Verdict != VerdictDeniedByRule {
						t.Fatalf("Evaluate(%s) verdict = %s, want %s",
							host, decision.Verdict, VerdictDeniedByRule)
					}
					if decision.Rule == nil || decision.Rule.Value != "10.0.0.0/8" {
						t.Fatalf("Evaluate(%s) rule = %+v, want the normalized cidr row",
							host, decision.Rule)
					}
				})
			}
		})
	}
}

// TestMappedDenyCIDRFromPersistedSpecDenies starts from a persisted-shaped
// spec rather than an in-memory authored entry, because the case for
// normalizing rather than refusing rests on already-persisted rows being
// repaired without operator action. Compilation is the chokepoint every launch
// crosses, so a profile written before this change denies as authored.
func TestMappedDenyCIDRFromPersistedSpecDenies(t *testing.T) {
	const persisted = `{
		"mode": "open",
		"deny": [{"cidr": "::ffff:10.0.0.0/104"}],
		"engine": "proxy"
	}`
	var rules sandboxpolicy.NetworkRules
	if err := json.Unmarshal([]byte(persisted), &rules); err != nil {
		t.Fatalf("unmarshal persisted network rules: %v", err)
	}
	if rules.Deny[0].CIDR != "::ffff:10.0.0.0/104" {
		t.Fatalf("persisted spec did not decode as authored: %+v", rules.Deny)
	}
	evaluator := mustEvaluator(t, rules)
	decision := evaluator.Evaluate(mustTarget(t, "10.0.0.1", 443))
	if decision.Verdict != VerdictDeniedByRule {
		t.Fatalf("verdict = %s, want %s", decision.Verdict, VerdictDeniedByRule)
	}
}

// TestMappedDenyCIDRDeniesResolvedAddress closes the rebinding route for the
// same row: a name that resolves into the mapped-authored range must be denied
// on the resolved address too, not just on the requested target.
func TestMappedDenyCIDRDeniesResolvedAddress(t *testing.T) {
	evaluator := mustEvaluator(t, listRules(
		[]sandboxpolicy.NetworkAllowEntry{{Domain: "example.com"}},
		[]sandboxpolicy.NetworkAllowEntry{{CIDR: "::ffff:10.0.0.0/104"}},
	))
	target := mustTarget(t, "example.com", 443)
	decision := evaluator.EvaluateResolvedAddress(
		target, netip.MustParseAddr("10.0.0.1"))
	if decision.Verdict != VerdictDeniedByRule {
		t.Fatalf("verdict = %s, want %s", decision.Verdict, VerdictDeniedByRule)
	}
	if decision.Rule == nil || decision.Rule.Value != "10.0.0.0/8" {
		t.Fatalf("rule = %+v, want the normalized cidr row", decision.Rule)
	}
}
