package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestClaudeHistoryDiscoverReadAndRevisionGuard(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "-workspace")
	require.NoError(t, os.Mkdir(project, 0o700))
	id := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	path := filepath.Join(project, id+".jsonl")
	writeClaudeHistory(t, path,
		`{"type":"user","uuid":"message-one","sessionId":"`+id+`","cwd":"/workspace","timestamp":"2026-09-06T10:00:00Z","message":{"role":"user","content":"find the regression"}}`,
		`{"type":"assistant","uuid":"message-two","sessionId":"`+id+`","cwd":"/workspace","timestamp":"2026-09-06T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"found it"},{"type":"tool_use"}]}}`,
		`{"type":"custom-title","sessionId":"`+id+`","customTitle":"Regression hunt"}`,
	)

	reader := historyReader{}
	discovered, err := reader.Discover(context.Background(), ports.HistoryDiscoveryRequest{Scope: ports.HistoryDiscoveryScope{Source: root}})
	require.NoError(t, err)
	require.Equal(t, model.HistoryCoverageComplete, discovered.Coverage.Metadata)
	require.Len(t, discovered.Histories, 1)
	source := discovered.Histories[0]
	require.Equal(t, "Regression hunt", source.Title)
	require.Equal(t, "/workspace", source.WorkspaceHint)
	require.Equal(t, model.HistoryContent, source.Availability)
	require.Equal(t, ports.HistoryPrecisionNone, reader.Capabilities().ForkPrecision)

	selection := ports.HistorySourceSelection{
		Provider: Name, Native: source.Native, SourceToken: source.SourceToken,
		SourceRevision: source.Coverage.SourceRevision, SourceFingerprint: source.SourceFingerprint,
		Point: &source.Points[0], Evidence: source.Evidence,
	}
	read, err := reader.Read(context.Background(), selection)
	require.NoError(t, err)
	require.Len(t, read.Turns, 2)
	require.Equal(t, "find the regression", read.Turns[0].Parts[0].Text)
	require.Equal(t, ports.HistoryPartUnsupported, read.Turns[1].Parts[1].Kind)
	require.True(t, read.Turns[1].Parts[1].Omitted)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString("{}\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = reader.Read(context.Background(), selection)
	require.ErrorContains(t, err, "source revision changed")
}

func TestClaudeExactForkIsUnsupportedForMutableNativeHead(t *testing.T) {
	root := t.TempDir()
	id := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	path := filepath.Join(root, id+".jsonl")
	writeClaudeHistory(t, path,
		`{"type":"user","uuid":"message-one","sessionId":"`+id+`","cwd":"/workspace","timestamp":"2026-09-06T10:00:00Z","message":{"role":"user","content":"source"}}`,
	)
	discovered, err := (historyReader{}).Discover(context.Background(), ports.HistoryDiscoveryRequest{Scope: ports.HistoryDiscoveryScope{Source: root}})
	require.NoError(t, err)
	source := discovered.Histories[0]
	selection := &ports.HistorySourceSelection{Provider: Name, Native: source.Native,
		SourceRevision: source.Coverage.SourceRevision, SourceFingerprint: source.SourceFingerprint,
		Point: &source.Points[0], Evidence: source.Evidence}
	request := ports.PreparationRequest{Intent: ports.StartFork, History: selection,
		Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_fork", Harness: Name}}
	nativeID, err := nativeIDFor(request)
	require.Empty(t, nativeID)
	require.ErrorIs(t, err, ports.ErrHistoryUnsupported)
	require.Equal(t, ports.HistoryPrecisionNone, (historyReader{}).Capabilities().ForkPrecision)
}

func TestClaudeHistoryRejectsMessagePrecisionForFork(t *testing.T) {
	selection := &ports.HistorySourceSelection{Provider: Name,
		Native: model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: "43e874eb-4827-4b22-b1b8-376a5e5e553f"},
		Point:  &ports.ProviderHistoryPoint{Kind: model.HistoryPointMessage},
	}
	_, err := nativeIDFor(ports.PreparationRequest{Intent: ports.StartFork, History: selection})
	require.ErrorIs(t, err, ports.ErrHistoryUnsupported)
}

func writeClaudeHistory(t *testing.T, path string, records ...string) {
	t.Helper()
	content := ""
	for _, record := range records {
		content += record + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
