package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// costsWhatIfTotal reads the operator's /v1/costs WHAT-IF total for the
// current month, the bucket Codex's priced token usage lands in.
func costsWhatIfTotal(t *testing.T, f *testharness.Flow) float64 {
	t.Helper()
	rec := accountQuery(t, f, "", "/v1/costs")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		WhatIfTotalUSD float64 `json:"what_if_total_usd"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.WhatIfTotalUSD
}

// Regression: a copy-clone carries the source's rollout, token_count events
// and all. Pricing the clone's rollout from the top counted the source's
// spend a second time under the clone's new conversation. Only the clone's
// own usage may add to the books.
func TestCloneCodex_CopiedHistoryCostIsNotCountedTwice(t *testing.T) {
	const conv = "019ec004-4250-79b1-9ade-ebaea4170190"
	const label = "spwn-codexclonecost"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	agentd.ResetCodexContextRefreshForTest()
	t.Cleanup(agentd.ResetCodexContextRefreshForTest)

	// Given: a live Codex agent that has already spent on priced usage.
	f := newFlow(t)
	require.NoError(t, config.Save(&config.Config{Cost: &config.CostConfig{ShowOnSubscription: true}}))
	f.HaveGroup("crew")
	cx := f.HaveAliveCodexSession(conv, label, "tmux-codexclonecost", f.TestCwd("codexclonecost"))
	cx.Model = "gpt-5.6-terra"
	f.HaveMember("crew", conv)
	usage := testharness.CodexTokenUsage{InputTokens: 200000, OutputTokens: 10000, TotalTokens: 210000}
	require.NoError(t, cx.WriteUserInput("do the codex thing"))
	require.NoError(t, cx.WriteTokenCount(usage, usage))
	fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	sourceSpend := costsWhatIfTotal(t, f)
	require.Positive(t, sourceSpend, "the source's priced usage is on the books")

	// When: it is cloned with its history, and the clone is read.
	clone := f.CloneWith(conv, map[string]any{})
	require.Equalf(t, http.StatusOK, clone.Code, "body=%s", clone.Raw)
	require.True(t, clone.CopyConv, "the clone copies the conversation")
	agentd.ResetCodexContextRefreshForTest()
	fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	// Then: the copied history adds nothing.
	assert.InDelta(t, sourceSpend, costsWhatIfTotal(t, f), 1e-9,
		"the clone's copied token usage stays the source's spend")

	// And: the clone's own usage is counted.
	clonedSim := f.World.Codexes.GetByConvID(clone.NewConv)
	require.NotNil(t, clonedSim, "the clone resumed its copied rollout")
	clonedSim.Model = "gpt-5.6-terra"
	require.NoError(t, clonedSim.WriteUserInput("the clone's own work"))
	require.NoError(t, clonedSim.WriteTokenCount(usage, usage))
	agentd.ResetCodexContextRefreshForTest()
	fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.InDelta(t, 2*sourceSpend, costsWhatIfTotal(t, f), 1e-9,
		"the clone's own identical turn costs the same as the source's")
}
