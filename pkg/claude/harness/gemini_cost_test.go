package harness

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiCallCostUSDPricesCachedToolAndThinkingTokens(t *testing.T) {
	// 100k prompt of which 40k cached, 10k tool prompt, 2k output + 3k thinking
	// on 3.1 Pro's short tier.
	usd, ok := geminiCallCostUSD("gemini-3.1-pro-preview", geminiTokens{
		Input: 100_000, Cached: 40_000, Tool: 10_000, Output: 2_000, Thoughts: 3_000,
	})
	require.True(t, ok)
	want := (70_000*2.00 + 40_000*0.20 + 5_000*12.00) / 1e6
	assert.InDelta(t, want, usd, 1e-12)

	// Over 200k of prompt the whole request moves to the long tier.
	usd, ok = geminiCallCostUSD("gemini-3.1-pro-preview", geminiTokens{Input: 250_000, Output: 1_000})
	require.True(t, ok)
	assert.InDelta(t, (250_000*4.00+1_000*18.00)/1e6, usd, 1e-12)

	// A flat-rate model has no long tier.
	usd, ok = geminiCallCostUSD("gemini-2.5-flash", geminiTokens{Input: 300_000, Output: 1_000})
	require.True(t, ok)
	assert.InDelta(t, (300_000*0.30+1_000*2.50)/1e6, usd, 1e-12)

	_, ok = geminiCallCostUSD("gemini-3-pro-preview", geminiTokens{Input: 1})
	assert.False(t, ok, "a model the pricing page does not list stays unestimated")
}

func TestGeminiCostHistoryIsCumulativePerLocalDay(t *testing.T) {
	day1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	day2 := day1.Add(24 * time.Hour)
	call := func(at time.Time, model string) geminiBilledCall {
		return geminiBilledCall{model: model, tokens: geminiTokens{Input: 1_000_000}, timestamp: at}
	}
	total, history := geminiCostHistory([]geminiBilledCall{
		call(day1, "gemini-2.5-flash"),
		call(day1.Add(time.Hour), "gemini-unknown"),
		call(time.Time{}, "gemini-2.5-flash"), // no timestamp: the previous call's day
		call(day2, "gemini-2.5-pro"),
	}, day2.Add(time.Hour))
	assert.InDelta(t, 0.30+0.30+2.50, total, 1e-9, "1M tokens of prompt is 2.5 Pro's long tier")
	require.Len(t, history, 2)
	assert.Equal(t, day1.Format("2006-01-02"), history[0].Day)
	assert.InDelta(t, 0.60, history[0].CostUSD, 1e-9)
	assert.Equal(t, day2.Format("2006-01-02"), history[1].Day)
	assert.InDelta(t, 3.10, history[1].CostUSD, 1e-9)
	assert.Equal(t, "gemini-2.5-pro", history[1].Model)
}

func TestGeminiUsageFollowerCostSurvivesRewindAndCheckpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	path := writeGeminiUsageFixture(t, home, geminiUsageTestConv,
		`{"id":"u1","type":"user","content":"hi"}`,
		`{"id":"g1","type":"gemini","content":"a","model":"gemini-2.5-flash","timestamp":"2026-10-02T08:01:00Z"}`,
		// The usage lands on a re-append of the same id: one call, not two.
		`{"id":"g1","type":"gemini","content":"a","model":"gemini-2.5-flash","timestamp":"2026-10-02T08:01:00Z","tokens":{"input":1000000,"output":0}}`,
		`{"id":"u2","type":"user","content":"more"}`,
		`{"id":"g2","type":"gemini","content":"b","model":"gemini-2.5-flash","timestamp":"2026-10-02T08:02:00Z","tokens":{"input":1000000,"output":0}}`,
		`{"$rewindTo":"u2"}`,
		`{"$set":{"messages":[{"id":"s1","type":"user","content":"summary"}]}}`,
	)
	var follower GeminiUsageFollower
	usage, found, err := follower.Read(path)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 0, usage.Calls, "the checkpoint left no call in the conversation")
	assert.InDelta(t, 0.60, usage.CostUSD, 1e-9, "both calls were made, so both are priced")
	history := follower.CostHistory(time.Now())
	require.Len(t, history, 1)
	assert.InDelta(t, 0.60, history[0].CostUSD, 1e-9)
}

func TestGeminiUsageFollowerCostSurvivesAFileRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	path := writeGeminiUsageFixture(t, home, geminiUsageTestConv,
		`{"id":"g1","type":"gemini","model":"gemini-2.5-flash","tokens":{"input":1000000,"output":0}}`,
		`{"id":"g2","type":"gemini","model":"gemini-2.5-flash","tokens":{"input":1000000,"output":0}}`,
	)
	var follower GeminiUsageFollower
	usage, _, err := follower.Read(path)
	require.NoError(t, err)
	assert.InDelta(t, 0.60, usage.CostUSD, 1e-9)

	// An atomic rewrite that keeps only g2: g1 was still made.
	rewritten := path + ".tmp"
	require.NoError(t, os.WriteFile(rewritten, []byte(geminiUsageTestMeta(geminiUsageTestConv, "2026-10-02T09:00:00Z")+"\n"+
		`{"id":"g2","type":"gemini","model":"gemini-2.5-flash","tokens":{"input":1000000,"output":0}}`+"\n"), 0o644))
	require.NoError(t, os.Rename(rewritten, path))
	usage, _, err = follower.Read(path)
	require.NoError(t, err)
	assert.Equal(t, 1, usage.Calls)
	assert.InDelta(t, 0.60, usage.CostUSD, 1e-9, "a rewrite never lowers the cost of calls already made")
}
