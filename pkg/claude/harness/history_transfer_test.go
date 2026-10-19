package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const historySource = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
const historyTarget = "019fe740-43a4-7023-b8ae-1ee64459f2a2"

func TestHistoryTransferRemapsMetadataOnly(t *testing.T) {
	for _, name := range []string{DefaultName, CodexName} {
		t.Run(name, func(t *testing.T) {
			h, _ := Get(name)
			require.True(t, h.SupportsHistoryTransfer())
			native := h.History.(jsonlHistory)
			prose := historySource + " /original api_key=keepverbatim123456"
			var raw string
			if name == CodexName {
				raw = `{"type":"session_meta","payload":{"id":"` + historySource + `","cwd":"/original"}}` + "\n" + `{"type":"turn_context","payload":{"cwd":"/original"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","content":[{"text":"` + prose + `"}]}}` + "\n"
			} else {
				raw = `{"sessionId":"` + historySource + `","cwd":"/original","type":"user","message":{"content":"` + prose + `"}}` + "\n"
			}
			require.NoError(t, native.Validate([]byte(raw), historySource))
			got, err := native.rewriteHistory([]byte(raw), historySource, historyTarget, "/receiving")
			require.NoError(t, err)
			assert.Contains(t, string(got), prose)
			assert.Contains(t, string(got), historyTarget)
			assert.Contains(t, string(got), `"cwd":"/receiving"`)
			var obj map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(strings.Split(string(got), "\n")[0]), &obj))
			require.Error(t, native.Validate([]byte(strings.ReplaceAll(raw, historySource, historyTarget)), historySource))
			require.Error(t, native.Validate([]byte("not-json\n"), historySource))
		})
	}
}

func TestHistoryTransferLargeSingleRecord(t *testing.T) {
	h, _ := Get(DefaultName)
	history := h.History.(StreamingHistoryTransfer)
	raw := `{"sessionId":"` + historySource + `","cwd":"/original","type":"user","message":{"content":"` + strings.Repeat("x", 11<<20) + `"}}` + "\n"
	require.NoError(t, history.ValidateReader(strings.NewReader(raw), historySource))
	require.ErrorContains(t, history.ValidateReader(historyLimitedReader{strings.NewReader(raw), 1 << 20}, historySource), "federation.agent_history_record_max_bytes")
}

type historyLimitedReader struct {
	*strings.Reader
	limit int
}

func (r historyLimitedReader) HistoryRecordLimit() int { return r.limit }

// A clone whose source runs with a private CODEX_HOME must find its imported
// rollout under that root, not under the daemon's own Codex home.
func TestCodexHistoryImportHonoursExplicitStateRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "daemon-codex"))
	db.ResetForTest()
	root := filepath.Join(home, "agent-codex")
	h, _ := Get(CodexName)
	scoped := h.History.(interface {
		WithCodexHome(string) HistoryTransfer
	}).WithCodexHome(root)
	raw := `{"type":"session_meta","payload":{"id":"` + historySource + `","cwd":"/original"}}` + "\n"
	id, cleanup, err := scoped.Import([]byte(raw), historySource, "/receiving")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	matches, err := filepath.Glob(filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
	require.NoError(t, err)
	assert.Len(t, matches, 1, "rollout lands under the explicit root")
	_, err = os.Stat(filepath.Join(home, "daemon-codex", "sessions"))
	assert.True(t, os.IsNotExist(err), "nothing lands under the daemon's own CODEX_HOME")
	until, err := db.ConvCostInheritedUntil(id)
	require.NoError(t, err)
	assert.False(t, until.IsZero(), "the copied token usage is marked as the source's spend")
	cleanup()
	until, err = db.ConvCostInheritedUntil(id)
	require.NoError(t, err)
	assert.True(t, until.IsZero(), "a rolled-back import leaves no boundary behind")
}

// Claude Code restores a transcript's cost-state on resume. Re-keyed onto the
// copy it would put the source's spend on the copy's meter, so the import
// drops it; the rest of the transcript is untouched.
func TestClaudeHistoryImportDropsCostState(t *testing.T) {
	h, _ := Get(DefaultName)
	native := h.History.(jsonlHistory)
	turn := `{"sessionId":"` + historySource + `","cwd":"/original","type":"user","message":{"content":"mentions cost-state in prose"}}`
	costState := `{"type":"cost-state","sessionId":"` + historySource + `","totalCostUSD":12.5}`
	raw := turn + "\n" + costState + "\n" + turn + "\n"
	require.NoError(t, native.Validate([]byte(raw), historySource))
	got, err := native.rewriteHistory([]byte(raw), historySource, historyTarget, "/receiving")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	require.Len(t, lines, 2)
	for _, line := range lines {
		assert.Contains(t, line, "mentions cost-state in prose")
		assert.NotContains(t, line, "totalCostUSD")
	}
}
