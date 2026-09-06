//go:build linux || darwin

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestOpenCodeHistoryDiscoverReadAndMessagePoint(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "execution-history")
	require.NoError(t, os.Mkdir(stateRoot, 0o700))
	nativeID := "ses_fixture"
	require.NoError(t, writeHistoryManifest(stateRoot, historyManifest{NativeID: nativeID, CWD: t.TempDir()}))
	exportPath := filepath.Join(root, "export.json")
	writeOpenCodeExport(t, exportPath, nativeID, mustManifest(t, stateRoot).CWD, "second answer")
	executable := filepath.Join(root, "opencode-fake")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n[ \"$1\" = export ] || exit 2\ncat \"$OPENCODE_EXPORT_FIXTURE\"\n"), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: root,
		Environment: []string{"OPENCODE_EXPORT_FIXTURE=" + exportPath}})
	require.NoError(t, err)
	reader := provider.History()
	require.Equal(t, ports.HistoryPrecisionBeforeMessage, reader.Capabilities().ForkPrecision)
	require.True(t, reader.Capabilities().ForkRequiresExclusive)

	discovered, err := reader.Discover(context.Background(), ports.HistoryDiscoveryRequest{})
	require.NoError(t, err)
	require.Equal(t, model.HistoryCoverageComplete, discovered.Coverage.Metadata)
	require.Len(t, discovered.Histories, 1)
	source := discovered.Histories[0]
	require.Equal(t, "Fixture history", source.Title)
	require.Len(t, source.Points, 3)
	require.Equal(t, model.HistoryPointBeforeMessage, source.Points[0].Kind)
	require.Equal(t, model.HistoryPointHead, source.Points[2].Kind)

	selection := ports.HistorySourceSelection{Provider: Name, Native: source.Native,
		SourceRevision: source.Coverage.SourceRevision, SourceFingerprint: source.SourceFingerprint,
		Evidence: source.Evidence, Point: &source.Points[0]}
	read, err := reader.Read(context.Background(), selection)
	require.NoError(t, err)
	require.Empty(t, read.Turns)

	selection.Point = &source.Points[1]
	read, err = reader.Read(context.Background(), selection)
	require.NoError(t, err)
	require.Len(t, read.Turns, 1)
	require.Equal(t, "first question", read.Turns[0].Parts[0].Text)

	selection.Point = &source.Points[2]
	read, err = reader.Read(context.Background(), selection)
	require.NoError(t, err)
	require.Len(t, read.Turns, 2)
	require.Equal(t, "second answer", read.Turns[1].Parts[0].Text)

	writeOpenCodeExport(t, exportPath, nativeID, mustManifest(t, stateRoot).CWD, "changed")
	_, err = reader.Read(context.Background(), selection)
	require.ErrorContains(t, err, "source revision changed")
}

func TestOpenCodeHistoryDiscoveryReportsUnreadableSourceAsUnknown(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "execution-history")
	require.NoError(t, os.Mkdir(stateRoot, 0o700))
	require.NoError(t, writeHistoryManifest(stateRoot, historyManifest{NativeID: "ses_missing", CWD: t.TempDir()}))
	executable := filepath.Join(root, "opencode-fail")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: root})
	require.NoError(t, err)
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	require.Equal(t, model.HistoryUnknown, discovered.Histories[0].Availability)
	require.Equal(t, model.HistoryCoveragePartial, discovered.Coverage.Metadata)
}

func TestOpenCodeHistoryDiscoversOrdinaryConfiguredNativeRoot(t *testing.T) {
	providerRoot := t.TempDir()
	nativeRoot := t.TempDir()
	workspace := t.TempDir()
	exportPath := filepath.Join(providerRoot, "export.json")
	writeOpenCodeExport(t, exportPath, "ses_native", workspace, "ordinary answer")
	listPath := filepath.Join(providerRoot, "sessions.json")
	require.NoError(t, os.WriteFile(listPath, []byte(`[{"id":"ses_native","title":"Native","directory":"`+workspace+`"}]`), 0o600))
	executable := filepath.Join(providerRoot, "opencode-fake")
	script := "#!/bin/sh\nif [ \"$1\" = db ]; then cat \"$OPENCODE_LIST_FIXTURE\"; exit; fi\nif [ \"$1\" = export ]; then cat \"$OPENCODE_EXPORT_FIXTURE\"; exit; fi\nexit 2\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: providerRoot,
		Environment: []string{"OPENCODE_LIST_FIXTURE=" + listPath, "OPENCODE_EXPORT_FIXTURE=" + exportPath}})
	require.NoError(t, err)
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{
		Scope: ports.HistoryDiscoveryScope{Source: nativeRoot},
	})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	require.Equal(t, "ses_native", discovered.Histories[0].Native.Reference)
	read, err := provider.History().Read(context.Background(), ports.HistorySourceSelection{
		Provider: Name, Native: discovered.Histories[0].Native,
		SourceRevision:    discovered.Histories[0].Coverage.SourceRevision,
		SourceFingerprint: discovered.Histories[0].SourceFingerprint, Evidence: discovered.Histories[0].Evidence,
	})
	require.NoError(t, err)
	require.Len(t, read.Turns, 2)
}

func mustManifest(t *testing.T, root string) historyManifest {
	t.Helper()
	manifest, err := readHistoryManifest(root)
	require.NoError(t, err)
	return manifest
}

func writeOpenCodeExport(t *testing.T, path, nativeID, cwd, secondText string) {
	t.Helper()
	value := `{
  "info":{"id":"` + nativeID + `","directory":"` + cwd + `","title":"Fixture history","time":{"created":1788717000000,"updated":1788717100000}},
  "messages":[
    {"info":{"id":"msg_one","role":"user","time":{"created":1788717000000}},"parts":[{"type":"text","text":"first question"}]},
    {"info":{"id":"msg_two","role":"assistant","time":{"created":1788717100000}},"parts":[{"type":"text","text":"` + secondText + `"},{"type":"tool","name":"bash"}]}
  ]
}`
	require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
}
