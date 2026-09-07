package copilotfixture_test

import (
	"bytes"
	"encoding/json"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

func TestCopilotBackgroundEventLog(t *testing.T) {
	requireSmoke(t)
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{ID: "call_background_probe", Name: "task", Args: `{"name":"fixture-agent","description":"fixture agent","prompt":"Reply done","agent_type":"explore","mode":"background"}`}},
		{Text: "DONE"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	const id = "9a1c2d3e-4f50-4617-8829-0b1c2d3e4f50"
	result := copilotfixture.Run(t, copilotfixture.RunOptions{Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir, BaseURL: mock.BaseURL(), Model: copilotfixture.MockModel, SessionID: id, Prompt: "start the requested task"})
	require.Equal(t, 0, result.ExitCode, "%s", result.Stderr)
	data, err := os.ReadFile(filepath.Join(dirs.Home, "session-state", id, "events.jsonl"))
	require.NoError(t, err)

	require.Contains(t, copilotfixture.EventTypesIn(data), "subagent.started")
	require.Contains(t, copilotfixture.EventTypesIn(data), "subagent.completed")
	// Replay real on-disk bytes one record at a time: the follower must show
	// background work between start and completion, not just after shutdown.
	home := t.TempDir()
	dir := filepath.Join(home, "session-state", id)
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := filepath.Join(dir, "events.jsonl")
	follower := &harness.CopilotTelemetryFollower{}
	var prefix []byte
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		prefix = append(prefix, line...)
		prefix = append(prefix, '\n')
		require.NoError(t, os.WriteFile(path, prefix, 0600))
		snap, ok, err := follower.RuntimeTelemetry(home, id)
		require.NoError(t, err)
		require.True(t, ok)
		var event struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &event))
		switch event.Type {
		case "subagent.started":
			require.Equal(t, 1, snap.SubagentCount)
		case "subagent.completed":
			require.Zero(t, snap.SubagentCount)
		}
	}
}
