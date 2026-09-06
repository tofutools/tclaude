package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestHistoryDiscoversTurnPointsReadsAndRevisionFences(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "states", "state-one")
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root, NativeHome: stateRoot})
	require.NoError(t, err)
	transcript := filepath.Join(stateRoot, "sessions", "2026", "09", "06", "rollout-test.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o700))
	lines := "{\"timestamp\":\"2026-09-06T10:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"00000000-0000-4000-8000-000000000001\",\"timestamp\":\"2026-09-06T10:00:00Z\",\"cwd\":\"" + root + "\"}}\n" +
		"{\"timestamp\":\"2026-09-06T10:01:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-1\"}}\n" +
		"{\"timestamp\":\"2026-09-06T10:01:01Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hello\"},{\"type\":\"input_image\"}]}}\n" +
		"{\"timestamp\":\"2026-09-06T10:02:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"hello\"}}\n" +
		"{\"timestamp\":\"2026-09-06T10:03:00Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"world\"}]}}\n"
	require.NoError(t, os.WriteFile(transcript, []byte(lines), 0o600))
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{Scope: ports.HistoryDiscoveryScope{WorkspaceHint: root}})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	item := discovered.Histories[0]
	require.Equal(t, "hello", item.Title)
	require.Len(t, item.Points, 1)
	require.Equal(t, model.HistoryPointTurn, item.Points[0].Kind)
	selection := ports.HistorySourceSelection{ConversationID: "conversation", Provider: Name, Native: item.Native, SourceToken: item.SourceToken, SourceRevision: item.Coverage.SourceRevision, SourceFingerprint: item.SourceFingerprint, Point: &item.Points[0], Evidence: item.Evidence}
	read, err := provider.History().Read(context.Background(), selection)
	require.NoError(t, err)
	require.Len(t, read.Turns, 2)
	require.Equal(t, ports.HistoryPartUnsupported, read.Turns[0].Parts[1].Kind)
	require.True(t, read.Turns[0].Parts[1].Omitted)
	require.Equal(t, model.HistoryCoveragePartial, read.Coverage.Content)
	require.NoError(t, os.WriteFile(transcript, []byte(lines+"{}\n"), 0o600))
	_, err = provider.History().Read(context.Background(), selection)
	require.ErrorContains(t, err, "source revision changed")
}
