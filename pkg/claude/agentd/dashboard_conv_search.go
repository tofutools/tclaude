package agentd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
)

const (
	convSearchMinQuery = 3
	convSearchMaxQuery = 200
	convSearchResults  = 50
	convSearchMatches  = 3
	// convSearchBudget bounds one scan. The newest conversations are read
	// first, so running out of budget drops the oldest ones.
	convSearchBudget = 5 * time.Second
	// convSearchQueueWait lets a query wait out the one it replaced, whose
	// aborted request frees the slot within milliseconds.
	convSearchQueueWait = 2 * time.Second
)

// convSearchSlot admits one transcript scan at a time, however many tabs ask.
var convSearchSlot = make(chan struct{}, 1)

type dashboardConvSearchMatch struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type dashboardConvSearchRow struct {
	ConvID   string                     `json:"conv_id"`
	AgentID  string                     `json:"agent_id,omitempty"`
	Retired  bool                       `json:"retired,omitempty"`
	Title    string                     `json:"title"`
	Online   bool                       `json:"online"`
	Modified string                     `json:"modified,omitempty"`
	Matches  []dashboardConvSearchMatch `json:"matches"`
}

// handleDashboardConversationSearch serves GET /api/conversations/search?q= —
// the Groups tab's content search. It reads conv_index for the candidates and
// the transcripts for matches, on request only, so it adds nothing to the poll
// and builds no index. The request context ends the scan when the panel moves
// on to a newer query.
func handleDashboardConversationSearch(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := len([]rune(q)); !utf8.ValidString(q) || n < convSearchMinQuery || n > convSearchMaxQuery {
		writeError(w, http.StatusBadRequest, "invalid_arg", "search text must be 3 to 200 characters")
		return
	}
	wait, cancelWait := context.WithTimeout(r.Context(), convSearchQueueWait)
	defer cancelWait()
	select {
	case convSearchSlot <- struct{}{}:
		defer func() { <-convSearchSlot }()
	case <-wait.Done():
		if errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		writeError(w, http.StatusServiceUnavailable, "search_busy", "another conversation search is running")
		return
	}
	rows, err := db.ListSearchableConvIndex()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), convSearchBudget)
	defer cancel()
	found := conv.SearchIndexed(ctx, rows, conv.IndexedSearchOptions{
		Query: q, MaxResults: convSearchResults, MaxMatches: convSearchMatches,
	})
	if r.Context().Err() != nil {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"results":   convSearchRows(found.Results),
		"scanned":   found.Scanned,
		"total":     found.Total,
		"truncated": found.Truncated,
	})
}

func convSearchRows(results []conv.IndexedSearchMatch) []dashboardConvSearchRow {
	convIDs := make([]string, 0, len(results))
	for _, result := range results {
		convIDs = append(convIDs, result.Row.ConvID)
	}
	agents, _ := db.AgentsByConv(convIDs)
	sessions, _ := db.FindSessionsByConvIDs(convIDs)
	alive, _ := cachedLiveTmuxSessions()
	rows := make([]dashboardConvSearchRow, 0, len(results))
	for _, result := range results {
		row := result.Row
		matches := make([]dashboardConvSearchMatch, 0, len(result.Matches))
		for _, match := range result.Matches {
			matches = append(matches, dashboardConvSearchMatch{
				Line: match.LineNum, Text: strings.ToValidUTF8(match.Content, "�"),
			})
		}
		owner := agents[row.ConvID]
		// Only the head generation promotes back; a replaced one is history.
		retired := owner.Retired && owner.CurrentConvID == row.ConvID
		rows = append(rows, dashboardConvSearchRow{
			ConvID:   row.ConvID,
			AgentID:  owner.AgentID,
			Retired:  retired,
			Title:    convindex.FormatConvTitle(row.CustomTitle, row.Summary, row.FirstPrompt),
			Online:   isConvOnlineInSessions(sessions[row.ConvID], alive),
			Modified: row.Modified,
			Matches:  matches,
		})
	}
	return rows
}
