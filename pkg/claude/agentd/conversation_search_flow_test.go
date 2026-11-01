package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type convSearchWire struct {
	Results []struct {
		ConvID  string `json:"conv_id"`
		AgentID string `json:"agent_id"`
		Title   string `json:"title"`
		Matches []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
	} `json:"results"`
	Scanned   int  `json:"scanned"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

func convSearch(t *testing.T, mux http.Handler, q string) (int, convSearchWire) {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/api/conversations/search?q="+url.QueryEscape(q), nil)
	require.NoError(t, err)
	rec := testharness.Serve(mux, r)
	var out convSearchWire
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec.Code, out
}

// Scenario: a phrase that appears only inside a transcript finds its
// conversation from the indexed rows, and archived ones stay out.
func TestConversationSearch_FindsTranscriptContent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	dir := testutil.CanonicalTempDir(t)
	seed := func(id, content string) {
		path := filepath.Join(dir, id+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0600))
		f.HaveConvWithTitle(id, id+"-title")
		row, err := db.GetConvIndex(id)
		require.NoError(t, err)
		require.NotNil(t, row)
		row.FullPath = path
		require.NoError(t, db.UpsertConvIndex(row))
	}
	seed("search-hit-1", `{"text":"the Flux Capacitor overheated"}`)
	seed("search-miss-1", `{"text":"nothing relevant"}`)
	seed("search-gone-1", `{"text":"flux capacitor in an archived one"}`)
	require.NoError(t, db.SetConvIndexArchived("search-gone-1", true))

	mux := agentd.BuildDashboardHandlerForTest()
	code, out := convSearch(t, mux, "flux capacitor")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "search-hit-1", out.Results[0].ConvID)
	require.NotEmpty(t, out.Results[0].Matches)
	assert.Contains(t, out.Results[0].Matches[0].Text, "Flux Capacitor")
	assert.False(t, out.Truncated)

	code, _ = convSearch(t, mux, "ab")
	assert.Equal(t, http.StatusBadRequest, code, "too short to scan every transcript for")
}
