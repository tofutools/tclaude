package conv

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common/termtext"
)

// An agent can set its conversation's title by renaming itself, so a conv
// command that prints titles prints through cliStdout, cleaned on a terminal
// (tcl-7gm2ec). Here archive's confirmation line.
func TestConvArchiveConfirmationIsTerminalSafeOnATerminal(t *testing.T) {
	setupTestDB(t)
	const id = "0459cd73-0000-4000-8000-000000000001"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: id, ProjectDir: t.TempDir(), CustomTitle: "fix\x1b[2J\x1b]8;;https://evil.example\x1b\\it\u202egpj.exe",
		Created: "2026-03-01T10:00:00Z", IndexedAt: time.Now(),
	}))
	var raw, errOut bytes.Buffer
	require.Zero(t, runArchiveOrUnarchive(id, true, termtext.NewWriter(&raw), &errOut), errOut.String())
	out := raw.String()
	for _, bad := range []string{"\x1b", "\u202e"} {
		assert.NotContains(t, out, bad)
	}
	assert.Contains(t, out, "Archived 0459cd73")
}
