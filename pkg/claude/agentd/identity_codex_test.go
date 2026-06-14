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

// TestConvIDForPID_CodexAncestorResolvesViaPaneShPID is the JOH-206
// regression. A Codex agent's socket peer walks up to a "codex" ancestor
// (not claude/node) and — because Codex writes no per-pid session file — its
// conv-id comes from agentd's sessions table. The decisive detail this test
// pins: the spawn row is keyed by the tmux pane_pid, which is the
// `sh -c "… codex"` wrapper, and codex runs as that sh's DIRECT CHILD
// (verified live: codex pid 205165 was a child of sh pane pid 205164). So
// FindSessionByPID at the codex pid misses; the conv-id must resolve via the
// codex ancestor's PARENT pid — the pane sh, which is the row key.
func TestConvIDForPID_CodexAncestorResolvesViaPaneShPID(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 4101 // the `tclaude agent` CLI process over the socket
		codexPID = 4050 // the codex runtime — recognised as the harness ancestor
		paneSh   = 4040 // the `sh -c "… codex"` pane wrapper = the tmux pane_pid
	)
	const convID = "11111111-2222-3333-4444-555555555555"

	// The spawn row `tclaude session new` actually writes: keyed by the
	// pane_pid (ParsePIDFromTmux) — the sh wrapper, NOT the codex pid.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codexsess",
		PID:     paneSh,
		ConvID:  convID,
		Harness: "codex",
		Status:  "working",
	}))

	fakeProcTree{
		name: map[int]string{peerPID: "tclaude", codexPID: "codex", paneSh: "sh"},
		parent: map[int]int{
			peerPID:  codexPID,
			codexPID: paneSh, // codex is the pane sh's direct child
			// paneSh's parent is unmapped → walk ends at init.
		},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor, "codex ancestor must be recognised")
	assert.Equal(t, convID, gotConv, "conv-id resolves via the codex ancestor's parent (pane sh) pid")

	// And the real authorization chokepoint accepts it as an agent — the
	// surface whoami / inbox gate on.
	p := &peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}
	assert.Equal(t, classAgent, classify(p))
}

// TestConvIDForPID_CodexAncestorResolvesViaOwnPID covers the variant where
// the sessions row is keyed by the harness pid itself — the pane shell
// exec'd into the harness (so pane_pid == the codex pid), or a hook
// corrected the row to FindClaudePID(). The direct
// FindSessionByPID(harness pid) lookup must still resolve it, so the fix
// works regardless of whether an sh wrapper survives between pane and codex.
func TestConvIDForPID_CodexAncestorResolvesViaOwnPID(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 7101
		codexPID = 7050
	)
	const convID = "22222222-3333-4444-5555-666666666666"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codexsess2",
		PID:     codexPID,
		ConvID:  convID,
		Harness: "codex",
		Status:  "working",
	}))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", codexPID: "codex"},
		parent: map[int]int{peerPID: codexPID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv, "conv-id resolves via the harness pid directly")
}

// TestConvIDForPID_HarnessNameWithoutRecordedRowIsAgentUnknown is the
// un-forgeability guard, and the reason this fix resolves identity from the
// sessions table (host pids the daemon recorded at spawn) rather than from
// the harness process's own environment. A caller that merely *names* a
// process "codex" but whose pid AND parent pid map to no recorded row gets
// hasAncestor=true but NO conv-id → classAgentUnknown (403). It can never
// inherit another agent's identity by choosing a process name or planting an
// env var, because neither feeds the lookup — proven live: `cp tclaude
// /tmp/codex && TCLAUDE_SESSION_ID=<victim> /tmp/codex agent whoami` resolved
// to the victim under the env approach this replaces.
func TestConvIDForPID_HarnessNameWithoutRecordedRowIsAgentUnknown(t *testing.T) {
	setupTestDB(t)

	const (
		forgedPID = 8101 // an attacker-controlled process renamed "codex"
		shellPID  = 8050 // its real parent — no recorded session row
	)

	// A decoy row for a DIFFERENT session, keyed by a pid the attacker
	// cannot make its ancestor. It must never be returned.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "victim",
		PID:     9999,
		ConvID:  "victim-conv",
		Harness: "codex",
		Status:  "working",
	}))

	fakeProcTree{
		name:   map[int]string{forgedPID: "codex", shellPID: "sh"},
		parent: map[int]int{forgedPID: shellPID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(forgedPID)
	assert.True(t, hasAncestor, "a codex-named process counts as a harness ancestor")
	assert.Empty(t, gotConv, "but resolves to no conv-id without a recorded row")
	assert.Equal(t, classAgentUnknown,
		classify(&peer{PID: forgedPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}),
		"fail closed: a forged harness name never inherits another agent's identity")
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
