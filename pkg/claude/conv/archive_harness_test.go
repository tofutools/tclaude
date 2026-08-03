package conv

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// `tclaude conv archive <copilot-id>` with no prior conv_index row.
//
// Archiving is a tclaude concept: Copilot has no equivalent, so the flag lives
// in `conv_index.archived_at` and the command resolves its argument through
// that table. But a harness that owns its own conversation store only gets a
// conv_index row as a SIDE EFFECT of a listing, and nothing makes a human run
// `conv ls` first. Before the harness fallback the command failed with "no
// conversation matches" for a conversation it would happily list a second
// later — an ordering dependency, not a real absence.

const copilotArchiveConvID = "7c6b5a49-3827-4165-9403-1a2b3c4d5e6f"

// copilotArchiveHome writes one real-shaped Copilot session-state tree and
// points the harness at it.
func copilotArchiveHome(t *testing.T, cwd string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "copilot-home")
	dir := filepath.Join(home, "session-state", copilotArchiveConvID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	workspace := "id: " + copilotArchiveConvID + "\n" +
		"cwd: " + cwd + "\n" +
		"name: widgets refactor\n" +
		"user_named: false\n" +
		"created_at: 2026-08-03T19:08:12.442Z\n" +
		"updated_at: 2026-08-03T19:08:13.219Z\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(workspace), 0o600))
	events := `{"type":"session.start","data":{"sessionId":"` + copilotArchiveConvID +
		`","selectedModel":"gpt-5.4"}}` + "\n" +
		`{"type":"user.message","data":{"content":"refactor the widgets"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(events), 0o600))
	t.Setenv(harness.CopilotHomeEnvVar, home)
}

func TestArchiveCopilotConvWithoutAPriorIndexRow(t *testing.T) {
	setupTestDB(t)
	copilotArchiveHome(t, t.TempDir())

	row, err := db.GetConvIndex(copilotArchiveConvID)
	require.NoError(t, err)
	require.Nil(t, row, "the premise: nothing has cached this conversation yet")

	var stdout, stderr bytes.Buffer
	code := runArchiveOrUnarchive(copilotArchiveConvID, true, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "Archived")
	assert.Contains(t, stdout.String(), "widgets refactor",
		"the harness store's title should reach the confirmation line")

	row, err = db.GetConvIndex(copilotArchiveConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.True(t, row.IsArchived())
	assert.Equal(t, harness.CopilotName, row.Harness)

	// And the reverse verb works on the row the fallback created.
	stdout.Reset()
	stderr.Reset()
	require.Equal(t, 0, runArchiveOrUnarchive(copilotArchiveConvID, false, &stdout, &stderr),
		"stderr: %s", stderr.String())
	row, err = db.GetConvIndex(copilotArchiveConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.False(t, row.IsArchived())
}

// TestArchiveCopilotConvByPrefix covers the form a human actually types.
func TestArchiveCopilotConvByPrefix(t *testing.T) {
	setupTestDB(t)
	copilotArchiveHome(t, t.TempDir())

	var stdout, stderr bytes.Buffer
	code := runArchiveOrUnarchive(copilotArchiveConvID[:8], true, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr: %s", stderr.String())

	row, err := db.GetConvIndex(copilotArchiveConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.True(t, row.IsArchived())
}

// TestArchiveUnknownConvStillFails keeps the fallback from turning a genuine
// miss into a silently created row.
func TestArchiveUnknownConvStillFails(t *testing.T) {
	setupTestDB(t)
	copilotArchiveHome(t, t.TempDir())

	var stdout, stderr bytes.Buffer
	code := runArchiveOrUnarchive("deadbeef-0000-4000-8000-000000000000", true, &stdout, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "no conversation matches")
	assert.Empty(t, stdout.String())
}
