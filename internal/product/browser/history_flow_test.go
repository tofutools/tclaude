package browser

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type historyBrowserProvider struct{ ports.Provider }

func (historyBrowserProvider) Name() string { return "codex" }
func (historyBrowserProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
func (historyBrowserProvider) History() ports.HistoryReader { return historyBrowserReader{} }

type historyBrowserSources struct{}

func (historyBrowserSources) HistorySource(h, n string) (ports.HistoryDiscoveryScope, bool) {
	return ports.HistoryDiscoveryScope{Source: "fixture"}, h == "codex" && n == "fixture"
}

type historyBrowserReader struct{}

func (historyBrowserReader) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{MetadataDiscovery: true, ContentRead: true}
}
func historyBrowserCoverage() model.HistoryCoverage {
	return model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoveragePartial, SourceRevision: "fixture-1", RefreshedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
}
func (historyBrowserReader) Discover(context.Context, ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	coverage := historyBrowserCoverage()
	var histories []ports.DiscoveredHistory
	for _, name := range []string{"Architecture notes", "Other conversation"} {
		histories = append(histories, ports.DiscoveredHistory{Native: model.NativeConversationEvidence{Namespace: "codex", Reference: name}, SourceToken: name, SourceFingerprint: name, Title: name, Availability: model.HistoryContent, Coverage: coverage, ModifiedAt: coverage.RefreshedAt, Points: []ports.ProviderHistoryPoint{{Token: "before", Kind: model.HistoryPointBeforeMessage}}, Evidence: model.ProviderEvidence{Provider: "codex", Version: 1}})
	}
	return ports.HistoryDiscoveryResult{Histories: histories, Coverage: coverage}, nil
}
func (historyBrowserReader) Read(_ context.Context, selection ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	turns := []ports.HistoryTurn{{Role: "user", Parts: []ports.HistoryPart{{Kind: ports.HistoryPartText, Text: "earlier architecture question"}}}}
	if selection.Point == nil {
		turns = append(turns, ports.HistoryTurn{Role: "assistant", Parts: []ports.HistoryPart{{Kind: ports.HistoryPartText, Text: "needle answer <script>unsafe</script>"}, {Kind: ports.HistoryPartUnsupported, MediaType: "image/png", Omitted: true}}})
	}
	return ports.HistoryReadResult{Turns: turns, Coverage: historyBrowserCoverage()}, nil
}

func TestBrowserHistoryFiltersReadsArchivesAndPreservesConflicts(t *testing.T) {
	ctx, page, operator := processEditorBrowserWithHistory(t, historyBrowserSources{}, historyBrowserProvider{})
	var refreshed app.HistorySearchResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/history/refresh", map[string]any{"harness": "codex", "source": "fixture"}, &refreshed))
	page.MustElement("[data-tab=history]").MustClick()
	page.MustElementR("#histories [role=status]", "2 conversations")
	page.MustElement("[aria-label='History harness']").MustSelect("claude")
	page.MustElementR("#histories [role=status]", "0 conversations")
	page.MustElement("[aria-label='History harness']").MustSelect("codex")
	page.MustElementR("#histories [role=status]", "2 conversations")
	page.MustElement("[aria-label='Search history']").MustInput("Architecture")
	page.MustElementR("#histories form button", "^Search$").MustClick()
	page.MustElementR("#histories [role=status]", "1 conversations")
	page.MustElementR("#histories button", "^Read conversation$").MustClick()
	page.MustElementR("#histories h2", "Architecture notes")
	page.MustElementR("#histories pre", "needle answer <script>unsafe</script>")
	require.Len(t, page.MustElements("#histories script"), 0)
	page.MustElementR("#histories pre", "unsupported omitted")
	page.MustElement("[aria-label='Find in conversation']").MustInput("needle")
	page.MustElementR("#histories p", "1 matching turns of 2")
	page.MustElement("[aria-label='Conversation role']").MustSelect("user")
	page.MustElementR("#histories p", "0 matching turns of 2")
	page.MustEval(`() => {window.exported='';const original=URL.createObjectURL;URL.createObjectURL=b=>{b.text().then(t=>window.exported=t);return original(b)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#histories button", "^Export conversation text$").MustClick()
	page.MustWait(`() => window.exported.includes('needle answer')`)
	require.Contains(t, page.MustEval(`() => window.exported`).Str(), "image/png")
	page.MustElement("[aria-label='History read point']").MustSelect(page.MustElement("[aria-label='History read point'] option:nth-child(2)").MustText())
	page.MustElementR("#histories button", "^Read selected point$").MustClick()
	page.MustElementR("#histories p", "1 matching turns of 1")
	require.Len(t, page.MustElements(".history-turn"), 1)
	// Refresh the catalog's revision after indexing the native read.
	page.MustElementR("#histories form button", "^Search$").MustClick()
	page.MustElementR("#histories button", "^Edit title$").MustClick()
	page.MustElement("#editor [name=title]").MustSelectAllText().MustInput("Local edit")
	var current app.HistorySearchResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/history/search", map[string]any{"query": "Architecture"}, &current))
	require.Len(t, current.Entries, 1)
	entry := current.Entries[0]
	require.NoError(t, operator.Call(ctx, "POST", "/v2/history/metadata", map[string]any{"request_id": "concurrent", "conversation_id": entry.ConversationID, "expected_revision": entry.Revision, "title": "Architecture concurrent", "archived": false}, nil))
	page.MustElementR("#editor button", "^Save$").MustClick()
	page.MustElementR("#editor-error", "saved state changed")
	require.Equal(t, "Local edit", page.MustElement("#editor [name=title]").MustProperty("value").Str())
	page.MustElement("#cancel").MustClick()
	page.MustElementR("#histories form button", "^Search$").MustClick()
	page.MustElementR("#histories strong", "Architecture concurrent")
	page.MustElementR("#histories button", "^Archive conversation$").MustClick()
	page.MustElementR("#editor button", "^Save$").MustClick()
	page.MustElementR("#histories [role=status]", "0 conversations")
	page.MustElement("[aria-label='History archive']").MustSelect("Archived")
	page.MustElementR("#histories [role=status]", "1 conversations")
	page.MustElementR("#histories button", "^Restore from archive$").MustClick()
	page.MustElementR("#editor button", "^Save$").MustClick()
	page.MustElementR("#histories [role=status]", "0 conversations")
	page.MustElement("#logout").MustClick()
	page.MustElementR("#histories [role=status]", "Signed out")
	require.Len(t, page.MustElements(".history-turn"), 0)

}
