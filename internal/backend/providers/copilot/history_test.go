package copilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestHistoryDiscoversReadsAndRevisionFencesLocalSession(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "states", "state-one")
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root, NativeHome: stateRoot})
	require.NoError(t, err)
	sessionID := "00000000-0000-4000-8000-000000000001"
	dir := filepath.Join(stateRoot, "session-state", sessionID)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte("id: "+sessionID+"\ncwd: "+root+"\nname: history fixture\ncreated_at: 2026-09-06T10:00:00Z\nupdated_at: 2026-09-06T11:00:00Z\n"), 0o600))
	events := "{\"id\":\"user-1\",\"timestamp\":\"2026-09-06T10:01:00Z\",\"type\":\"user.message\",\"data\":{\"content\":\"hello\",\"attachments\":[{\"name\":\"image\"}]}}\n" +
		"{\"id\":\"assistant-1\",\"timestamp\":\"2026-09-06T10:02:00Z\",\"type\":\"assistant.message\",\"data\":{\"content\":\"world\"}}\n"
	eventsPath := filepath.Join(dir, "events.jsonl")
	require.NoError(t, os.WriteFile(eventsPath, []byte(events), 0o600))
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{Scope: ports.HistoryDiscoveryScope{WorkspaceHint: root}})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	item := discovered.Histories[0]
	require.Equal(t, model.HistoryCoveragePartial, discovered.Coverage.Content, "local provider storage cannot claim complete cloud history")
	require.Equal(t, model.HistoryPointHead, item.Points[0].Kind)
	selection := ports.HistorySourceSelection{ConversationID: "conversation", Provider: Name, Native: item.Native, SourceToken: item.SourceToken, SourceRevision: item.Coverage.SourceRevision, SourceFingerprint: item.SourceFingerprint, Point: &item.Points[0], Evidence: item.Evidence}
	read, err := provider.History().Read(context.Background(), selection)
	require.NoError(t, err)
	require.Len(t, read.Turns, 2)
	require.Equal(t, ports.HistoryPartText, read.Turns[0].Parts[0].Kind)
	require.Equal(t, ports.HistoryPartUnsupported, read.Turns[0].Parts[1].Kind)
	require.True(t, read.Turns[0].Parts[1].Omitted)
	require.Equal(t, model.HistoryCoveragePartial, read.Coverage.Content)
	require.NoError(t, os.WriteFile(eventsPath, []byte(events+"{}\n"), 0o600))
	_, err = provider.History().Read(context.Background(), selection)
	require.ErrorContains(t, err, "source revision changed")
}
