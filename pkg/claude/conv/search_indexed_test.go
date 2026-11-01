package conv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func indexedRow(t *testing.T, dir, id string, lines ...string) *db.ConvIndexRow {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600))
	return &db.ConvIndexRow{ConvID: id, FullPath: path}
}

func TestSearchIndexed_MatchesTitleAndContentLiterally(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	titled := indexedRow(t, dir, "titled", "nothing here")
	titled.CustomTitle = "The a.b Report"
	body := indexedRow(t, dir, "body", "first", `{"text":"saw A.B fail"}`)
	regexOnly := indexedRow(t, dir, "regex", "axb only matches as a regex")

	out := SearchIndexed(context.Background(), []*db.ConvIndexRow{titled, body, regexOnly}, IndexedSearchOptions{Query: "a.b"})

	require.Len(t, out.Results, 2)
	assert.Equal(t, "titled", out.Results[0].Row.ConvID)
	assert.Equal(t, "body", out.Results[1].Row.ConvID)
	assert.Equal(t, 2, out.Results[1].Matches[0].LineNum)
	assert.Equal(t, 3, out.Scanned)
	assert.False(t, out.Truncated)
}

func TestSearchIndexed_StopsAtTheBounds(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	var rows []*db.ConvIndexRow
	for i := range 5 {
		rows = append(rows, indexedRow(t, dir, fmt.Sprintf("conv-%d", i), "needle one", "needle two", "needle three"))
	}

	out := SearchIndexed(context.Background(), rows, IndexedSearchOptions{Query: "needle", MaxResults: 2, MaxMatches: 2})
	require.Len(t, out.Results, 2)
	assert.Len(t, out.Results[0].Matches, 2, "a conversation stops at its match cap")
	assert.Equal(t, 2, out.Scanned)
	assert.Equal(t, 5, out.Total)
	assert.True(t, out.Truncated, "unread conversations must not read as a complete search")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out = SearchIndexed(ctx, rows, IndexedSearchOptions{Query: "needle"})
	assert.Empty(t, out.Results)
	assert.Zero(t, out.Scanned)
	assert.True(t, out.Truncated)
}

// JSONL stores a quote as \" and a backslash as \\, so the text a person sees
// in a transcript is not the text on disk.
func TestSearchIndexed_FindsTextJSONEscapes(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	row := indexedRow(t, dir, "quoted", `{"text":"she said \"hello\" from C:\\foo"}`)
	for _, query := range []string{`said "hello"`, `C:\foo`} {
		out := SearchIndexed(context.Background(), []*db.ConvIndexRow{row}, IndexedSearchOptions{Query: query})
		assert.Len(t, out.Results, 1, query)
	}
}

// A few huge lines must not outrun cancellation, however few of them there are.
func TestScanConversation_CancelStopsPartWayThroughAFile(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	lines := make([]string, 8)
	for i := range lines {
		lines[i] = "needle " + strings.Repeat("x", scanCancelCheckBytes)
	}
	row := indexedRow(t, dir, "long", lines...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	found := scanConversation(ctx, row.FullPath, regexp.MustCompile("needle"), 0)
	assert.Empty(t, found, "a cancelled read stops at the next check")
	assert.Len(t, searchConversation(row.FullPath, regexp.MustCompile("needle")), len(lines), "the CLI path still reads everything")
}
