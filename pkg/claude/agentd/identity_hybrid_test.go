package agentd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestConvIDForPID_CodexWrapperPIDResolvesViaEnv pins the C2 failure mode
// from JOH-206: spawn records tmux's shell wrapper PID, while the socket
// peer's process tree walks to the real codex process. The env/session-id
// path must resolve identity even when FindSessionByPID(codexPID) would miss.
func TestConvIDForPID_CodexWrapperPIDResolvesViaEnv(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID  = 4101 // the `tclaude agent` CLI process over the socket
		shPID    = 4090 // tmux pane shell wrapper recorded by ParsePIDFromTmux
		codexPID = 4050 // the codex runtime observed in the ancestor walk
	)
	const convID = "11111111-2222-3333-4444-555555555555"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codexsess",
		PID:     shPID,
		ConvID:  convID,
		Harness: "codex",
		Status:  "working",
	}))

	installIdentityProcTree(t,
		map[int]string{
			peerPID:  "tclaude",
			shPID:    "sh",
			codexPID: "codex",
		},
		map[int]int{
			peerPID: shPID,
			shPID:   codexPID,
		},
		map[int][]byte{
			codexPID: []byte("PATH=/bin\x00TCLAUDE_SESSION_ID=codexsess\x00"),
		},
	)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor, "codex ancestor must be recognised")
	assert.Equal(t, convID, gotConv, "conv-id resolved from TCLAUDE_SESSION_ID despite wrapper PID row")
	assert.Equal(t, classAgent, classify(&peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}))
}

func TestProcEnvValue(t *testing.T) {
	installIdentityProcTree(t, nil, nil, map[int][]byte{
		7: []byte("A=1\x00TCLAUDE_SESSION_ID=sess-7\x00EMPTY=\x00"),
	})

	assert.Equal(t, "sess-7", procEnvValue(7, "TCLAUDE_SESSION_ID"))
	assert.Empty(t, procEnvValue(7, "MISSING"))
	assert.Empty(t, procEnvValue(7, "EMPTY"))
}

func installIdentityProcTree(t *testing.T, names map[int]string, parents map[int]int, env map[int][]byte) {
	t.Helper()
	prevName, prevParent, prevEnv := procName, procParent, procEnv
	procName = func(pid int) string { return names[pid] }
	procParent = func(pid int) int {
		if p, ok := parents[pid]; ok {
			return p
		}
		return 1
	}
	procEnv = func(pid int) ([]byte, error) {
		if data, ok := env[pid]; ok {
			return data, nil
		}
		return nil, fmt.Errorf("no env for pid %d", pid)
	}
	t.Cleanup(func() {
		procName = prevName
		procParent = prevParent
		procEnv = prevEnv
	})
}
