package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const geminiUsageTestConv = "8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11"

func geminiUsageTestMeta(convID, lastUpdated string) string {
	return `{"sessionId":"` + convID + `","projectHash":"h","startTime":"2026-10-02T08:00:00Z","lastUpdated":"` +
		lastUpdated + `","kind":"main"}`
}

func writeGeminiUsageFixture(t *testing.T, home, convID string, lines ...string) string {
	t.Helper()
	chats := filepath.Join(home, ".gemini", "tmp", "proj", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	path := filepath.Join(chats, "session-2026-10-02T08-00-"+convID[:8]+".jsonl")
	body := geminiUsageTestMeta(convID, "2026-10-02T08:00:00Z") + "\n" + strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func appendGeminiUsageFixture(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(text)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestGeminiUsageFollowerFoldsTheSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	path := writeGeminiUsageFixture(t, home, geminiUsageTestConv,
		`{"id":"u1","type":"user","content":"hi"}`,
		// The usage lands after the message: the same id is re-appended.
		`{"id":"g1","type":"gemini","content":"a","model":"gemini-3-flash","toolCalls":[{"id":"t","result":[{"big":"x"}]}]}`,
		`{"id":"g1","type":"gemini","content":"a","model":"gemini-3-flash","tokens":{"input":1000,"output":10,"cached":0,"thoughts":5,"total":1015}}`,
		`{"id":"u2","type":"user","content":"more"}`,
		`{"id":"g2","type":"gemini","content":"b","model":"gemma-4-31b-it","tokens":{"input":64000,"output":20,"cached":0,"thoughts":0,"total":64020}}`,
	)

	located, found, err := LocateGeminiSessionFile(geminiUsageTestConv)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, path, located)

	var follower GeminiUsageFollower
	usage, found, err := follower.Read(path)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 2, usage.Calls, "a re-recorded message is one call, not two")
	assert.Equal(t, int64(35), usage.OutputTokens)
	assert.Equal(t, int64(64000), usage.ContextTokens)
	assert.Equal(t, "gemma-4-31b-it", usage.Model)
	assert.Equal(t, int64(256_000), usage.ContextWindow)
	assert.InDelta(t, 25.0, usage.ContextPct(), 0.001)

	// A record still being written is left for the next read.
	appendGeminiUsageFixture(t, path, `{"$rewindTo":"u2"}`)
	usage, _, err = follower.Read(path)
	require.NoError(t, err)
	assert.Equal(t, 2, usage.Calls, "a partial line must not be folded")
	appendGeminiUsageFixture(t, path, "\n")
	usage, _, err = follower.Read(path)
	require.NoError(t, err)
	assert.Equal(t, 1, usage.Calls, "a rewind drops the rewound turn's usage")
	assert.Equal(t, int64(1000), usage.ContextTokens)
	assert.Equal(t, int64(1_048_576), usage.ContextWindow)

	// A checkpoint replaces the history wholesale.
	appendGeminiUsageFixture(t, path, `{"$set":{"messages":[{"id":"s1","type":"user","content":"summary"},`+
		`{"id":"s2","type":"gemini","content":"ok","model":"gemini-3-flash","tokens":{"input":500,"output":1,"cached":0,"total":501}}]}}`+"\n")
	usage, _, err = follower.Read(path)
	require.NoError(t, err)
	assert.Equal(t, 1, usage.Calls)
	assert.Equal(t, int64(500), usage.ContextTokens)
	assert.Equal(t, int64(1), usage.OutputTokens)

	// An atomic rewrite (a new file under the same name) is re-folded from the
	// start rather than read from a stale offset.
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(geminiUsageTestMeta(geminiUsageTestConv, "2026-10-02T09:00:00Z")+"\n"+
		`{"id":"g9","type":"gemini","content":"z","model":"gemini-3-flash","tokens":{"input":7,"output":3,"cached":0,"total":10}}`+"\n"), 0o644))
	require.NoError(t, os.Rename(tmp, path))
	usage, _, err = follower.Read(path)
	require.NoError(t, err)
	assert.Equal(t, 1, usage.Calls)
	assert.Equal(t, int64(7), usage.ContextTokens)
	assert.Equal(t, int64(3), usage.OutputTokens)

	require.NoError(t, os.Remove(path))
	_, found, err = follower.Read(path)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, follower.Path())
}

// A resumed legacy session leaves its `.json` in place and writes only to the
// migrated `.jsonl`, which carries the same lastUpdated until the next message.
func TestLocateGeminiSessionFilePrefersTheMigratedJSONL(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	jsonl := writeGeminiUsageFixture(t, home, geminiUsageTestConv, `{"id":"u1","type":"user","content":"hi"}`)
	legacy := strings.TrimSuffix(jsonl, "l")
	require.NoError(t, os.WriteFile(legacy, []byte(`{"sessionId":"`+geminiUsageTestConv+
		`","lastUpdated":"2026-10-02T08:00:00Z","messages":[{"id":"g0","type":"gemini","content":"old",`+
		`"model":"gemini-3-flash","tokens":{"input":42,"output":1,"cached":0,"total":43}}]}`), 0o644))

	located, found, err := LocateGeminiSessionFile(geminiUsageTestConv)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, jsonl, located)
	assert.Equal(t, jsonl, GeminiMigratedSessionFile(legacy))
	assert.Empty(t, GeminiMigratedSessionFile(jsonl))

	var follower GeminiUsageFollower
	usage, found, err := follower.Read(legacy)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(42), usage.ContextTokens, "a legacy record's inline messages are folded")
}

func TestLocateGeminiSessionFileIgnoresAPrefixCollision(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	writeGeminiUsageFixture(t, home, "8d3c0d5e-0000-4000-8000-000000000000",
		`{"id":"u1","type":"user","content":"hi"}`)
	_, found, err := LocateGeminiSessionFile(geminiUsageTestConv)
	require.NoError(t, err)
	assert.False(t, found, "an 8-character prefix match is not the conversation")

	_, found, err = LocateGeminiSessionFile("short")
	require.NoError(t, err)
	assert.False(t, found)
}
