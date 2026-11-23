package agentd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// writeClaudeSubagentMeta writes a Claude Code sub-agent meta sidecar where
// Claude Code puts it for a conversation launched in cwd.
func writeClaudeSubagentMeta(t *testing.T, cwd, convID, agentID, body string) {
	t.Helper()
	dir := filepath.Join(convops.GetClaudeProjectPath(cwd), convID, "subagents")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-"+agentID+".meta.json"), []byte(body), 0o644))
}

// A sub-agent whose meta sidecar says it finished is dropped from what the
// read shows, even though its ledger entry is still within the TTL. That is
// the backstop for a sub-agent whose end no hook reported: without it, the
// row reads "1 subagent running" until the TTL runs out.
func TestTerminalStatus_ClaudeFinishedSubagentSettlesRow(t *testing.T) {
	setupTestDB(t)
	cwd := testutil.CanonicalTempDir(t)
	const convID = "conv-meta-finished"

	ledger := db.SubagentSet{
		"afinished01": {Type: "web-fetch", Seen: time.Now()},
	}.Encode()
	writeClaudeSubagentMeta(t, cwd, convID, "afinished01",
		`{"agentType":"web-fetch","requestShape":"foreground","finished":true}`)

	row := &db.SessionRow{
		ID: "s-meta-finished", ConvID: convID, TmuxSession: "pane-meta-finished", Cwd: cwd,
		Status: session.StatusMainAgentIdle, StatusDetail: "1 subagent running",
		SubagentsJSON: ledger, SubagentCount: 1,
		CreatedAt: time.Now(), LastHook: time.Now(),
	}
	require.NoError(t, db.SaveSession(row))
	stored, err := db.LoadSession(row.ID)
	require.NoError(t, err)

	term, online := terminalStatusForSessions([]*db.SessionRow{stored}, map[string]struct{}{row.TmuxSession: {}})
	require.True(t, online)
	assert.Equal(t, 0, term.SubagentCount, "a finished sub-agent is not counted")
	assert.Equal(t, session.StatusIdle, term.Status, "an emptied ledger settles the row like an expired one")
	assert.Empty(t, term.StatusDetail)
}

func TestClaudeFinishedSubagents(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cwd := testutil.CanonicalTempDir(t)
	const convID = "conv-meta"

	writeClaudeSubagentMeta(t, cwd, convID, "adone", `{"finished":true}`)
	writeClaudeSubagentMeta(t, cwd, convID, "arunning", `{"agentType":"Explore"}`)
	writeClaudeSubagentMeta(t, cwd, convID, "agarbled", `{"finished":`)
	set := db.SubagentSet{
		"adone":    {Seen: time.Now()},
		"arunning": {Seen: time.Now()},
		"agarbled": {Seen: time.Now()},
		"amissing": {Seen: time.Now()},
		"../adone": {Seen: time.Now()},
	}
	row := &db.SessionRow{ConvID: convID, Cwd: cwd}

	assert.Equal(t, map[string]struct{}{"adone": {}}, claudeFinishedSubagents(row, set),
		"only a sidecar that says finished drops an entry; missing, unfinished, garbled and unsafe ids are kept")

	codex := &db.SessionRow{ConvID: convID, Cwd: cwd, Harness: "codex"}
	assert.Nil(t, claudeFinishedSubagents(codex, set), "Claude Code sidecars say nothing about another harness")
	assert.Nil(t, claudeFinishedSubagents(&db.SessionRow{ConvID: convID}, set), "no cwd, no project dir")
}
