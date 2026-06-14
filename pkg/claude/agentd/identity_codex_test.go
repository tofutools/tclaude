package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// fakeProcTree stands in for the /proc readers convIDForPID walks. It
// models an ancestor chain as name + parent maps and installs itself over
// the procName / procParent package seams for the duration of a test.
type fakeProcTree struct {
	name   map[int]string
	parent map[int]int
}

func (f fakeProcTree) install(t *testing.T) {
	t.Helper()
	prevName, prevParent := procName, procParent
	procName = func(pid int) string { return f.name[pid] }
	procParent = func(pid int) int {
		if p, ok := f.parent[pid]; ok {
			return p
		}
		return 1 // reaching init ends the walk
	}
	t.Cleanup(func() { procName, procParent = prevName, prevParent })
}

// TestConvIDForPID_CodexAncestorResolvesViaDB is the JOH-206 regression:
// a Codex agent's socket peer walks up to a "codex" ancestor (not
// claude/node), and — because Codex writes no per-pid session file — its
// conv-id is resolved from agentd's sessions table, keyed by the same
// codex host pid the hook callback recorded. Before the fix the walk only
// matched claude/node, so the codex ancestor was missed and the caller got
// no identity (whoami → (unnamed), inbox → 403).
func TestConvIDForPID_CodexAncestorResolvesViaDB(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 4101 // the `tclaude agent` CLI process over the socket
		shPID    = 4090 // codex's shell wrapper
		codexPID = 4050 // the codex runtime — what the walk must recognise
	)
	const convID = "11111111-2222-3333-4444-555555555555"

	// The sessions row the hook callback would have written: keyed by the
	// codex host pid (FindClaudePID), conv-id populated after the seed turn.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codexsess",
		PID:     codexPID,
		ConvID:  convID,
		Harness: "codex",
		Status:  "working",
	}))

	fakeProcTree{
		name: map[int]string{
			peerPID:  "tclaude",
			shPID:    "sh",
			codexPID: "codex",
		},
		parent: map[int]int{
			peerPID: shPID,
			shPID:   codexPID,
			// codexPID's parent is unmapped → walk ends at init.
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor, "codex ancestor must be recognised")
	assert.Equal(t, convID, gotConv, "conv-id resolved from the sessions table")

	// And the real authorization chokepoint accepts it as an agent — the
	// surface whoami / inbox gate on.
	p := &peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}
	assert.Equal(t, classAgent, classify(p))
}

// TestConvIDForPID_ClaudeAncestorViaSessionFile guards that the Claude
// Code path is untouched by the harness-agnostic match: a "claude"
// ancestor still resolves its conv-id from ~/.claude/sessions/<pid>.json
// without ever consulting the DB.
func TestConvIDForPID_ClaudeAncestorViaSessionFile(t *testing.T) {
	setupTestDB(t) // sets HOME to a temp dir

	const (
		peerPID   = 5201
		claudePID = 5100
	)
	const convID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	sessDir := filepath.Join(home, ".claude", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "5100.json"),
		[]byte(`{"sessionId":"`+convID+`"}`), 0o644))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", claudePID: "claude"},
		parent: map[int]int{peerPID: claudePID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv, "conv-id resolved from the per-pid session file")
}

// TestConvIDForPID_NoHarnessAncestorStaysHuman guards the warning in the
// handoff: recognising codex must NOT reclassify a real human. A peer with
// a plain (sshd/bash) ancestor chain reports no harness ancestor, so
// classify routes it through the operator-token branch — human with a
// valid token, unconfirmed without — exactly as before.
func TestConvIDForPID_NoHarnessAncestorStaysHuman(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID = 6301
		bashPID = 6200
		sshPID  = 6100
	)
	fakeProcTree{
		name: map[int]string{peerPID: "tclaude", bashPID: "bash", sshPID: "sshd"},
		parent: map[int]int{
			peerPID: bashPID,
			bashPID: sshPID,
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.False(t, hasAncestor, "no harness ancestor in a plain shell chain")
	assert.Empty(t, gotConv)

	assert.Equal(t, classHuman,
		classify(&peer{PID: peerPID, HasClaudeAncestor: false, HumanTokenValid: true}),
		"a token-bearing human is still the human")
	assert.Equal(t, classUnconfirmed,
		classify(&peer{PID: peerPID, HasClaudeAncestor: false}),
		"no ancestor and no token is still unconfirmed, not an agent")
}
