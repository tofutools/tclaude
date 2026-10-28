package usage

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

func TestRuleFlagsBuildARule(t *testing.T) {
	r, err := ruleFlags{Name: "monthly", Period: "month", Budget: 500, Step: []string{"50:notify:operator", "100:block-spawn"}}.rule()
	require.NoError(t, err)
	r.Normalize()
	require.NoError(t, r.Validate(usagerules.ScopeGlobal))
	require.Equal(t, usagerules.ActionBlockSpawn, r.Steps[1].Action)

	_, err = ruleFlags{Name: "x"}.rule()
	require.Error(t, err, "a rule needs a step")
	_, err = ruleFlags{Name: "x", Step: []string{"bogus"}}.rule()
	require.Error(t, err)
}

func TestRenderRulesAndSelf(t *testing.T) {
	overview := map[string]any{
		"limits": []any{map[string]any{"id": 1, "scope": "global", "enabled": true, "rule": map[string]any{
			"name": "monthly", "metric": "total_usd", "period": "month", "budget": 500,
			"steps": []any{map[string]any{"at": 50, "action": "notify", "notify": []string{"operator"}}}}}},
		"profiles": []any{map[string]any{"id": 1, "name": "default", "rules": []any{}}},
		"assignments": []any{map[string]any{"scope": "global", "profile_name": "default"}},
		"statuses": []any{
			map[string]any{"rule_key": "limit:1", "scope": "global", "name": "monthly", "metric": "total_usd", "value": 260, "budget": 500, "pct": 52, "reached": "notify"},
			map[string]any{"rule_key": "profile:1:budget", "scope": "agent", "agent_id": "agt_1", "agent_title": "worker", "profile": "default", "name": "budget", "metric": "total_usd", "value": 5, "budget": 20, "pct": 25,
				"next": map[string]any{"at": 50, "action": "notify"}},
		},
	}
	raw, err := json.Marshal(overview)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, renderRules(raw, &out))
	text := out.String()
	require.Contains(t, text, `#1 global "monthly" total_usd month $500.00: 50:notify:operator`)
	require.Contains(t, text, "now $260.00 / $500.00 (52%)")
	require.Contains(t, text, "global → default")
	require.Contains(t, text, "worker")
	require.Contains(t, text, "$5.00 / $20.00 (25%), next notify at 50%")

	self, err := json.Marshal(map[string]any{"layer": "group", "profiles": []any{map[string]any{"name": "default"}},
		"statuses": overview["statuses"].([]any)[1:], "limits": overview["statuses"].([]any)[:1]})
	require.NoError(t, err)
	out.Reset()
	require.NoError(t, renderSelf(self, &out))
	require.Contains(t, out.String(), "Usage profiles: default (from the group layer)")
	require.Contains(t, out.String(), "global monthly: $260.00 / $500.00 (52%)")
}
