package copilotfixture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// Prove native resume can rebuild from the reviewed event projection without
// transferring a database, checkpoints, permissions or executable artifacts.
func TestCopilotPortableHistoryNativeResume(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_HISTORY_SMOKE") != "1" {
		t.Skip("installed native CLI")
	}
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "PORTABLE ORIGINAL ANSWER"}, {Text: "PORTABLE RESUMED ANSWER"}})
	source := copilotfixture.NewSandboxDirs(t)
	id := uuid.NewString()
	first := copilotfixture.Run(t, copilotfixture.RunOptions{Root: source.Root, Home: source.Home, Cache: source.Cache, WorkDir: source.WorkDir, BaseURL: mock.BaseURL(), SessionID: id, Prompt: "PORTABLE ORIGINAL QUESTION"})
	require.Equal(t, 0, first.ExitCode, first.Stderr)
	target := copilotfixture.NewSandboxDirs(t)
	t.Setenv("HOME", target.Root)
	t.Setenv("COPILOT_HOME", target.Home)
	raw, err := os.ReadFile(filepath.Join(source.Home, "session-state", id, "events.jsonl"))
	require.NoError(t, err)
	history := harness.MustGet("copilot").History
	reminted, cleanup, err := history.Import(raw, id, target.WorkDir)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	resumed := copilotfixture.Run(t, copilotfixture.RunOptions{Root: target.Root, Home: target.Home, Cache: target.Cache, WorkDir: target.WorkDir, BaseURL: mock.BaseURL(), ResumeID: reminted, Prompt: "PORTABLE NEW QUESTION"})
	require.Equal(t, 0, resumed.ExitCode, resumed.Stderr)
	requests := mock.Requests()
	require.Len(t, requests, 2)
	native, err := json.Marshal(requests[1])
	require.NoError(t, err)
	require.Contains(t, string(native), "PORTABLE ORIGINAL QUESTION")
	require.Contains(t, string(native), "PORTABLE ORIGINAL ANSWER")
	require.Contains(t, string(native), "PORTABLE NEW QUESTION")
}
