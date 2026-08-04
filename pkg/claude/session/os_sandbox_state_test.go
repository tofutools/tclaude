package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The launch boundary resolves the OS-sandbox verdict into a SessionState
// (TCL-729); everything downstream — the row, the dashboard badge — depends on
// toRow carrying it. fromRow matters just as much for a different reason: every
// state-tracking hook does a load→mutate→save, so a field toRow writes but
// fromRow drops would be silently erased by the first hook tick after launch,
// and the badge would vanish moments after appearing.
func TestOSSandboxVerdictSurvivesRowConversion(t *testing.T) {
	state := &SessionState{
		ID:                  "s1",
		ConvID:              "c1",
		Harness:             "claude",
		HarnessBuiltinMode:  "inherit",
		OSSandboxState:      "on",
		OSSandboxSource:     "~/.claude/settings.json",
		OSSandboxUnverified: true,
	}

	row := toRow(state)
	assert.Equal(t, "on", row.OSSandboxState, "toRow carries the verdict to the DB row")
	assert.Equal(t, "~/.claude/settings.json", row.OSSandboxSource)
	assert.True(t, row.OSSandboxUnverified, "the doubt travels with the verdict")

	back := fromRow(row)
	assert.Equal(t, state.OSSandboxState, back.OSSandboxState, "fromRow reads the verdict back")
	assert.Equal(t, state.OSSandboxSource, back.OSSandboxSource)
	assert.Equal(t, state.OSSandboxUnverified, back.OSSandboxUnverified)
}

// A harness that records no verdict round-trips as "nothing recorded" rather
// than as a positive claim — the value the UPSERT's preserve-on-empty rule and
// the badge's fallback both key on.
func TestAbsentOSSandboxVerdictRoundTripsEmpty(t *testing.T) {
	row := toRow(&SessionState{ID: "s1", ConvID: "c1", Harness: "codex", HarnessBuiltinMode: "workspace-write"})
	assert.Empty(t, row.OSSandboxState)
	assert.Empty(t, row.OSSandboxSource)
	assert.False(t, row.OSSandboxUnverified)

	back := fromRow(&db.SessionRow{ID: "s1", ConvID: "c1", Harness: "codex", HarnessBuiltinMode: "workspace-write"})
	assert.Empty(t, back.OSSandboxState)
	assert.Empty(t, back.OSSandboxSource)
	assert.False(t, back.OSSandboxUnverified)
}
