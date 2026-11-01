package conv

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// IndexedSearchOptions bounds one SearchIndexed call. The zero value of a
// limit means the default below, so a caller cannot ask for an unbounded scan.
type IndexedSearchOptions struct {
	Query      string
	MaxResults int
	MaxMatches int // per conversation
}

const (
	defaultIndexedSearchResults = 50
	defaultIndexedSearchMatches = 3
)

// IndexedSearchMatch is one conversation with what matched in it.
type IndexedSearchMatch struct {
	Row     *db.ConvIndexRow
	Matches []MatchLine
}

// IndexedSearchOutcome reports how far the scan got, so a caller can say
// "searched the newest N of M" instead of implying the search was complete.
type IndexedSearchOutcome struct {
	Results   []IndexedSearchMatch
	Scanned   int // conversations fully considered
	Total     int // conversations available to search
	Truncated bool
}

// indexedSearchPattern matches the query as typed, in metadata, and as JSON
// stores it, in a raw transcript line: a quote or backslash is escaped on disk.
func indexedSearchPattern(query string) (*regexp.Regexp, error) {
	pattern := "(?i)" + regexp.QuoteMeta(query)
	var encoded strings.Builder
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(query); err != nil {
		return nil, err
	}
	stored := strings.TrimSpace(encoded.String())
	if stored = stored[1 : len(stored)-1]; stored != query {
		pattern += "|" + regexp.QuoteMeta(stored)
	}
	return regexp.Compile(pattern)
}

// SearchIndexed runs the `conv search --content` matching over conv_index rows
// for a caller that cannot afford RunSearch: the daemon. It enumerates nothing
// and builds no index. rows are expected newest first, and the scan stops at
// MaxResults or when ctx ends, keeping what it found. Query is literal and
// case-insensitive, because it comes from a search box rather than a regex-
// aware CLI user.
func SearchIndexed(ctx context.Context, rows []*db.ConvIndexRow, opts IndexedSearchOptions) IndexedSearchOutcome {
	maxResults, maxMatches := opts.MaxResults, opts.MaxMatches
	if maxResults <= 0 {
		maxResults = defaultIndexedSearchResults
	}
	if maxMatches <= 0 {
		maxMatches = defaultIndexedSearchMatches
	}
	re, err := indexedSearchPattern(opts.Query)
	if err != nil {
		return IndexedSearchOutcome{Total: len(rows), Truncated: len(rows) > 0}
	}
	out := IndexedSearchOutcome{Total: len(rows)}
	for _, row := range rows {
		if ctx.Err() != nil || len(out.Results) >= maxResults {
			break
		}
		matches := searchMetadata(SessionEntry{
			CustomTitle: row.CustomTitle, Summary: row.Summary, FirstPrompt: row.FirstPrompt,
		}, re)
		if len(matches) < maxMatches {
			matches = append(matches, scanConversation(ctx, row.FullPath, re, maxMatches-len(matches))...)
		}
		if ctx.Err() != nil {
			// The file may be partly read, so it does not count as scanned, but
			// anything it already matched is still a true result.
			if len(matches) > 0 {
				out.Results = append(out.Results, IndexedSearchMatch{Row: row, Matches: matches})
			}
			break
		}
		out.Scanned++
		if len(matches) > 0 {
			out.Results = append(out.Results, IndexedSearchMatch{Row: row, Matches: matches})
		}
	}
	out.Truncated = out.Scanned < out.Total
	return out
}
