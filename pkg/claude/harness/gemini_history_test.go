package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestGeminiHistoryPreservesNativeCheckpointAndRewind(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv(GeminiHomeEnvVar, home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	cwd := testutil.CanonicalTempDir(t)
	prose := historySource + " /source private original text"
	raw := `{"sessionId":"` + historySource + `","projectHash":"old","kind":"main","startTime":"2026-10-01T10:00:00Z"}` + "\n" +
		`{"id":"u1","type":"user","content":"` + prose + `"}` + "\n" +
		`{"id":"g1","type":"gemini","content":"discard this"}` + "\n" +
		`{"$rewindTo":"g1"}` + "\n" +
		`{"$set":{"messages":[{"id":"u1","type":"user","content":"` + prose + `"},{"id":"g2","type":"gemini","content":"kept reply","toolCalls":[{"id":"call-1","result":{"output":"untouched"}}]}],"summary":"task summary"}}` + "\n" +
		`{"id":"g2","type":"gemini","content":"revised reply"}` + "\n" +
		`{"sessionId":"` + historySource + `","projectHash":"old","kind":"main"}` + "\n"
	h := geminiHistory{}
	require.NoError(t, h.Validate([]byte(raw), historySource))
	id, cleanup, err := h.Import([]byte(raw), historySource, cwd)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.NotEqual(t, historySource, id)
	r, err := h.Open(id, cwd)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.NoError(t, h.Validate(got, id))
	require.Contains(t, string(got), prose)
	require.NotContains(t, string(got), `"id":"u1"`)
	require.Contains(t, string(got), `"projectHash":"`+geminiProjectHash(cwd)+`"`)
	path, found, err := LocateGeminiSessionFile(id)
	require.NoError(t, err)
	require.True(t, found)
	f, err := os.Open(path)
	require.NoError(t, err)
	session, ok := foldGeminiSessionFile(f, path)
	_ = f.Close()
	require.True(t, ok)
	require.Len(t, session.messages, 2)
	require.Contains(t, string(session.messages[0].Content), prose)
	require.Contains(t, string(session.messages[1].Content), "revised reply")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	cleanup()
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}
func TestGeminiHistoryLegacyFieldOrder(t *testing.T) {
	raw := `{"messages":[{"id":"u1","type":"user","content":"original"}],"sessionId":"` + historySource + `","projectHash":"old","kind":"main","summary":"keep summary"}`
	var out bytes.Buffer
	require.NoError(t, streamLegacyGemini(strings.NewReader(raw), &out))
	require.NoError(t, (geminiHistory{}).Validate(out.Bytes(), historySource))
	require.Contains(t, out.String(), "keep summary")
	var metadata map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.Split(out.String(), "\n")[0]), &metadata))
	require.NotContains(t, metadata, "messages")
}
func TestGeminiHistoryRejectsWrongIdentityAndSubagents(t *testing.T) {
	h := geminiHistory{}
	for _, raw := range []string{`{"sessionId":"` + historyTarget + `","projectHash":"h"}`, `{"sessionId":"` + historySource + `","projectHash":"h","kind":"subagent"}`, `{"sessionId":"` + historySource + `","projectHash":"h"}` + "\n" + `{"$set":{"sessionId":"` + historyTarget + `"}}`} {
		require.Error(t, h.Validate([]byte(raw), historySource))
	}
	raw := `{"sessionId":"` + historySource + `","projectHash":"h"}` + "\n" + `{"id":"u1","type":"user","content":"` + strings.Repeat("x", 2000) + `"}` + "\n"
	require.ErrorContains(t, h.ValidateReader(historyLimitedReader{strings.NewReader(raw), 1024}, historySource), "federation.agent_history_record_max_bytes")
}

func TestGeminiHistoryMetadataSkipsLargeLegacyMessages(t *testing.T) {
	// Greater than the ordinary conversation reader's 64 MiB legacy ceiling.
	// Feed it without constructing the history in memory.
	r := io.MultiReader(strings.NewReader(`{"messages":[{"id":"u1","content":"`), io.LimitReader(geminiHistoryFillReader{}, 65<<20), strings.NewReader(`"}],"sessionId":"`+historySource+`","projectHash":"native"}`))
	meta, err := geminiHistoryMetadata(bufio.NewReader(r))
	require.NoError(t, err)
	require.Equal(t, historySource, meta["sessionId"])
	require.Equal(t, "native", meta["projectHash"])
}

type geminiHistoryFillReader struct{}

func (geminiHistoryFillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
