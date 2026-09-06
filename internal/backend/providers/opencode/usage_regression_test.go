package opencode

import "testing"

func TestUsageRegressionWholeDollarCost(t *testing.T) {
	_, cost, _, _, err := collectOpenCodeUsage([]byte(`{"messages":[{"info":{"role":"assistant","cost":10,"tokens":{"input":1}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cost.Amount != "10" {
		t.Fatalf("native USD 10 became %q", cost.Amount)
	}
}
func TestUsageRegressionMissingUsageCoverage(t *testing.T) {
	counters, cost, _, partial, err := collectOpenCodeUsage([]byte(`{"messages":[{"info":{"role":"assistant"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("missing usage: counters=%v cost=%v partial=%v; Collect maps !partial to complete counters", counters, cost, partial)
	if !partial {
		t.Fatal("missing assistant token fields are represented as complete zero counters")
	}
}

func TestUsageCoverageDistinguishesMissingFromKnownZero(t *testing.T) {
	for _, tc := range []struct {
		raw            string
		counters, cost string
		count          int
		amount         string
	}{
		{`{"messages":[{"info":{"role":"assistant"}}]}`, "unknown", "unknown", 0, ""},
		{`{"messages":[{"info":{"role":"assistant","cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}]}`, "complete", "complete", 5, "0"},
		{`{"messages":[{"info":{"role":"assistant","cost":10,"tokens":{"input":2}}},{"info":{"role":"assistant","tokens":{"input":3}}}]}`, "partial", "partial", 1, "10"},
	} {
		counters, cost, _, coverage, err := parseOpenCodeUsage([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if string(coverage.Counters) != tc.counters || string(coverage.Cost) != tc.cost || len(counters) != tc.count {
			t.Fatalf("coverage %+v counters %v", coverage, counters)
		}
		if tc.amount != "" && (cost == nil || cost.Amount != tc.amount) {
			t.Fatalf("cost %+v", cost)
		}
	}
}
