package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGeminiUsageFixture(t *testing.T, home, convID string, lines ...string) string {
	t.Helper()
	chats := filepath.Join(home, ".gemini", "tmp", "proj", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	path := filepath.Join(chats, "session-2026-10-02T08-00-"+convID[:8]+".jsonl")
	body := `{"sessionId":"` + convID + `","projectHash":"h","startTime":"2026-10-02T08:00:00Z","lastUpdated":"2026-10-02T08:00:00Z","kind":"main"}` + "\n" +
		strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestGeminiUsageFoldsTheSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	const convID = "8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11"
	path := writeGeminiUsageFixture(t, home, convID,
		`{"id":"u1","type":"user","content":"hi"}`,
		// The usage lands after the message: the same id is rewritten.
		`{"id":"g1","type":"gemini","content":"a","model":"gemini-3-flash"}`,
		`{"id":"g1","type":"gemini","content":"a","model":"gemini-3-flash","tokens":{"input":1000,"output":10,"cached":0,"thoughts":5,"total":1015}}`,
		`{"id":"u2","type":"user","content":"more"}`,
		`{"id":"g2","type":"gemini","content":"b","model":"gemma-4-31b-it","tokens":{"input":64000,"output":20,"cached":0,"thoughts":0,"total":64020}}`,
	)

	located, found, err := LocateGeminiSessionFile(convID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, path, located.Path)

	usage, _, ok := GeminiUsageFromFile(path)
	require.True(t, ok)
	assert.Equal(t, 2, usage.Calls, "a re-recorded message is one call, not two")
	assert.Equal(t, int64(35), usage.OutputTokens)
	assert.Equal(t, int64(64000), usage.ContextTokens)
	assert.Equal(t, "gemma-4-31b-it", usage.Model)
	assert.Equal(t, int64(256_000), usage.ContextWindow)
	assert.InDelta(t, 25.0, usage.ContextPct(), 0.001)

	// A rewind drops the rewound turn's usage with it.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"$rewindTo":"u2"}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	usage, _, ok = GeminiUsageFromFile(path)
	require.True(t, ok)
	assert.Equal(t, 1, usage.Calls)
	assert.Equal(t, int64(1000), usage.ContextTokens)
	assert.Equal(t, int64(1_048_576), usage.ContextWindow)
}

func TestLocateGeminiSessionFileIgnoresAPrefixCollision(t *testing.T) {
	home := t.TempDir()
	t.Setenv(GeminiHomeEnvVar, home)
	writeGeminiUsageFixture(t, home, "8d3c0d5e-0000-4000-8000-000000000000",
		`{"id":"u1","type":"user","content":"hi"}`)
	_, found, err := LocateGeminiSessionFile("8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11")
	require.NoError(t, err)
	assert.False(t, found, "an 8-character prefix match is not the conversation")

	_, found, err = LocateGeminiSessionFile("short")
	require.NoError(t, err)
	assert.False(t, found)
}
