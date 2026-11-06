package harness

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// openableHistory is a history transfer that also opens its stored file.
type openableHistory interface {
	HistoryTransfer
	Open(id, cwd string) (io.ReadCloser, error)
}

// Every file-backed history exports what Open reads, validated. These pin
// what Export hands back for a valid history, for one whose stored file no
// longer validates (copilot, gemini and pi hand back the bytes with the
// error; the Claude/Codex transcript hands back none), and for an unknown id.
func TestHistoryExportContract(t *testing.T) {
	const unknown = "eeee5555-0000-4000-8000-000000000005"
	for _, c := range []struct {
		name          string
		setup         func(t *testing.T) (h openableHistory, raw []byte, source string, path func(id, cwd string) string)
		bytesOnFailed bool
	}{
		{"copilot", func(t *testing.T) (openableHistory, []byte, string, func(string, string) string) {
			home := testutil.CanonicalTempDir(t)
			t.Setenv("HOME", home)
			t.Setenv("COPILOT_HOME", filepath.Join(home, "copilot"))
			raw := []byte(`{"id":"event-start","type":"session.start","data":{"sessionId":"` + historySource + `","context":{"cwd":"/old"}}}
{"id":"event-user","parentId":"event-start","type":"user.message","data":{"content":"hello"}}
`)
			return copilotHistory{}, raw, historySource, func(id, _ string) string {
				return filepath.Join(copilotHome(), copilotSessionStateDirName, id, copilotEventsFileName)
			}
		}, true},
		{"gemini", func(t *testing.T) (openableHistory, []byte, string, func(string, string) string) {
			home := testutil.CanonicalTempDir(t)
			t.Setenv("HOME", home)
			t.Setenv(GeminiHomeEnvVar, home)
			db.ResetForTest()
			t.Cleanup(db.ResetForTest)
			raw := []byte(`{"sessionId":"` + historySource + `","projectHash":"old","kind":"main","startTime":"2026-10-01T10:00:00Z"}` + "\n" +
				`{"id":"u1","type":"user","content":"hello"}` + "\n")
			return geminiHistory{}, raw, historySource, func(id, _ string) string {
				p, found, err := LocateGeminiSessionFile(id)
				require.NoError(t, err)
				require.True(t, found)
				return p
			}
		}, true},
		{"pi", func(t *testing.T) (openableHistory, []byte, string, func(string, string) string) {
			piTestAgentDir(t)
			const source = "dddd4444-0000-4000-8000-000000000004"
			raw := jsonl(t,
				obj{"type": "session", "version": 3, "id": source, "timestamp": "2026-10-01T10:00:00.000Z", "cwd": "/sender/proj"},
				piMsg("u1", nil, obj{"role": "user", "content": "hello"}),
			)
			return piHistory{}, raw, source, piLocateSessionFile
		}, true},
		{"claude", func(t *testing.T) (openableHistory, []byte, string, func(string, string) string) {
			home := testutil.CanonicalTempDir(t)
			t.Setenv("HOME", home)
			db.ResetForTest()
			t.Cleanup(db.ResetForTest)
			h, _ := Get(DefaultName)
			raw := []byte(`{"sessionId":"` + historySource + `","cwd":"/original","type":"user","message":{"content":"hello"}}` + "\n")
			return h.History.(openableHistory), raw, historySource, func(id, _ string) string {
				row, err := db.GetConvIndex(id)
				require.NoError(t, err)
				require.NotNil(t, row)
				return row.FullPath
			}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, raw, source, path := c.setup(t)
			cwd := testutil.CanonicalTempDir(t)
			id, cleanup, err := h.Import(raw, source, cwd)
			require.NoError(t, err)
			t.Cleanup(cleanup)

			r, err := h.Open(id, cwd)
			require.NoError(t, err)
			stored, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			got, err := h.Export(id, cwd)
			require.NoError(t, err)
			assert.Equal(t, stored, got, "Export is what Open reads")

			// Still found where it is stored, but no longer valid.
			corrupt := append(append([]byte(nil), stored...), "not-json\n"...)
			require.NoError(t, os.WriteFile(path(id, cwd), corrupt, 0o600))
			got, err = h.Export(id, cwd)
			assert.Error(t, err, "a stored file that no longer validates is refused")
			if c.bytesOnFailed {
				assert.Equal(t, corrupt, got, "the bytes come back with the error")
			} else {
				assert.Nil(t, got, "no bytes come back with the error")
			}

			got, err = h.Export(unknown, cwd)
			assert.Error(t, err)
			assert.Nil(t, got)
		})
	}
}
