package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func stubQuery(t *testing.T, path, response string) {
	t.Helper()
	prev := agent.DaemonRequestImpl
	agent.DaemonRequestImpl = func(method, got string, in, out any, opts agent.DaemonOpts) error {
		assert.Equal(t, "GET", method)
		assert.Equal(t, path, got)
		return json.Unmarshal([]byte(response), out)
	}
	t.Cleanup(func() { agent.DaemonRequestImpl = prev })
}

func TestUsageQueriesDaemonAndPreservesJSON(t *testing.T) {
	response := `{"windows":[{"provider":"openai","window_name":"seven_day","status":"current","pct":42,"age_seconds":60,"resets_at":"2026-10-10T00:00:00Z","forecasts":{"span":{"status":"before_reset","rate_pct_per_hour":2,"hits_limit_at":"2026-10-05T00:00:00Z"}}}],"coverage_warnings":[],"future_field":"preserved"}`
	stubQuery(t, "/v1/usage/summary", response)
	var out bytes.Buffer
	require.NoError(t, runUsage(&Params{JSON: true}, &out))
	assert.JSONEq(t, response, out.String())
	out.Reset()
	require.NoError(t, runUsage(&Params{}, &out))
	for _, s := range []string{"openai", "42.0%", "1m0s", "before_reset", "2.00 pp/h"} {
		assert.Contains(t, out.String(), s)
	}
}

func TestUsageDoesNotPresentExpiredReadingAsCurrent(t *testing.T) {
	stubQuery(t, "/v1/usage/summary", `{"windows":[{"provider":"anthropic","window_name":"five_hour","status":"reset","pct":80,"forecasts":{"span":{"status":"before_reset"}}}]}`)
	var out bytes.Buffer
	require.NoError(t, runUsage(&Params{}, &out))
	assert.Contains(t, out.String(), "80.0%")
	assert.Contains(t, out.String(), "last observation reset")
	assert.NotContains(t, out.String(), "before_reset")
}

func TestCostsQueriesDaemonWithRangeAndSelf(t *testing.T) {
	stubQuery(t, "/v1/costs?from=2026-10-01&self=true&to=2026-10-03", `{"scope":"self","from":"2026-10-01","to":"2026-10-03","timezone":"UTC","real_total_usd":2,"what_if_total_usd":3,"today_real_usd":1,"what_if_enabled":true,"agents":[{"provider":"anthropic","real_cost_usd":2,"what_if_cost_usd":3}]}`)
	var out bytes.Buffer
	require.NoError(t, runCosts(&CostsParams{From: "2026-10-01", To: "2026-10-03", Self: true}, &out))
	for _, s := range []string{"Recorded API: $2.0000", "WHAT-IF subscription estimate: $3.0000", "anthropic", "raw USD"} {
		assert.Contains(t, out.String(), s)
	}
}

func TestQueriesPropagateDaemonErrorsAndValidateDates(t *testing.T) {
	prev := agent.DaemonRequestImpl
	agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error { return fmt.Errorf("permission denied") }
	t.Cleanup(func() { agent.DaemonRequestImpl = prev })
	var out bytes.Buffer
	assert.ErrorContains(t, runUsage(&Params{}, &out), "permission denied")
	assert.ErrorContains(t, runCosts(&CostsParams{}, &out), "permission denied")
	assert.ErrorContains(t, runCosts(&CostsParams{From: "invalid"}, &out), "bad from date")
	assert.ErrorContains(t, runCosts(&CostsParams{From: "2026-10-03", To: "2026-10-01"}, &out), "from must be")
	assert.Empty(t, out.String())
}
