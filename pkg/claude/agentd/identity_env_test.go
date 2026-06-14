package agentd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestConvIDForPID_CodexAncestorResolvesFromTclaudeSessionID(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "sess-codex",
		ConvID:  "conv-codex",
		Status:  "running",
		Harness: "codex",
	}))

	restore := stubIdentityProcessTree(t,
		map[int]string{100: "tclaude", 200: "sh", 300: "codex"},
		map[int]int{100: 200, 200: 300, 300: 1},
		map[int][]byte{300: []byte("PATH=/bin\x00TCLAUDE_SESSION_ID=sess-codex\x00")},
	)
	t.Cleanup(restore)

	convID, hasAncestor := convIDForPID(100)
	assert.True(t, hasAncestor, "codex must count as a harness ancestor")
	assert.Equal(t, "conv-codex", convID)
	assert.Equal(t, classAgent, classify(&peer{PID: 100, ConvID: convID, HasClaudeAncestor: hasAncestor}))
}

func TestConvIDForPID_CodexAncestorWithoutResolvableEnvIsAgentUnknown(t *testing.T) {
	restore := stubIdentityProcessTree(t,
		map[int]string{100: "tclaude", 200: "sh", 300: "codex"},
		map[int]int{100: 200, 200: 300, 300: 1},
		map[int][]byte{300: []byte("PATH=/bin\x00TCLAUDE_SESSION_ID=missing\x00")},
	)
	t.Cleanup(restore)

	convID, hasAncestor := convIDForPID(100)
	assert.True(t, hasAncestor, "codex must count as a harness ancestor even when unresolved")
	assert.Empty(t, convID)
	assert.Equal(t, classAgentUnknown, classify(&peer{PID: 100, ConvID: convID, HasClaudeAncestor: hasAncestor}))
}

func TestProcEnvValue(t *testing.T) {
	restore := stubIdentityProcessTree(t, nil, nil, map[int][]byte{
		7: []byte("A=1\x00TCLAUDE_SESSION_ID=sess-7\x00EMPTY=\x00"),
	})
	t.Cleanup(restore)

	assert.Equal(t, "sess-7", procEnvValue(7, "TCLAUDE_SESSION_ID"))
	assert.Empty(t, procEnvValue(7, "MISSING"))
	assert.Empty(t, procEnvValue(7, "EMPTY"))
}

func stubIdentityProcessTree(t *testing.T, names map[int]string, parents map[int]int, env map[int][]byte) func() {
	t.Helper()
	prevName, prevParent, prevEnv := identityProcessName, identityParentPID, identityProcEnv
	identityProcessName = func(pid int) string { return names[pid] }
	identityParentPID = func(pid int) int { return parents[pid] }
	identityProcEnv = func(pid int) ([]byte, error) {
		if data, ok := env[pid]; ok {
			return data, nil
		}
		return nil, fmt.Errorf("no env for pid %d", pid)
	}
	return func() {
		identityProcessName = prevName
		identityParentPID = prevParent
		identityProcEnv = prevEnv
	}
}
