package conv

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ensureIndexedConv resolves an id or id-prefix through the non-Claude harness
// conversation stores and guarantees a conv_index row for the match.
//
// It exists because `conv_index` plays two different roles. For Claude Code it
// IS the conversation index, built by the reindexing scan. For a harness that
// owns its own store — Copilot, OpenCode, Codex — it is a cache, refreshed as
// a side effect of listing, and a conversation the CLI created five seconds
// ago has no row until something lists it.
//
// That asymmetry is invisible until a verb needs to WRITE a tclaude-owned
// column. `conv archive` stamps `conv_index.archived_at`, which is the only
// place the flag can live, so archiving a Copilot conversation used to depend
// on whether an unrelated earlier command had happened to populate the cache.
// Resolving through the stores removes the ordering dependency.
//
// Claude Code is skipped: the caller already tried its rich resolver, and its
// index is authoritative rather than a cache, so a miss there is a real miss.
//
// EVERY store is consulted before answering, rather than the first hit
// winning. `conv archive` accepts an 8-character prefix, and a prefix that
// matches conversations in two different harnesses is genuinely ambiguous —
// silently archiving whichever harness sorts first would act on a
// conversation the operator did not name.
//
// A single store's resolve failure does NOT abort the search. It is recorded
// and reported only if nothing else matched: an unreadable Codex store must
// not turn `conv archive <copilot-id>` into a Codex error, which is what
// returning early would do (harness.Names is sorted, and "codex" precedes
// "copilot"). Failing to read a store is still surfaced when it could be the
// reason for a miss, so a caller never reports "no such conversation" on the
// strength of a store it could not read.
func ensureIndexedConv(idPrefix string) (*db.ConvIndexRow, error) {
	if idPrefix == "" {
		return nil, nil
	}
	var (
		matches  []*convRefWithHarness
		failures []error
	)
	for _, name := range harness.Names() {
		if name == harness.DefaultName {
			continue
		}
		h, ok := harness.Get(name)
		if !ok || h.Convs == nil {
			continue
		}
		// Global: `conv archive` names a conversation by id, and an id is
		// unique across working directories. Scoping to the caller's cwd would
		// refuse to archive a conversation from another project purely because
		// of where the shell happened to be.
		ref, err := h.Convs.Resolve(idPrefix, "", true)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if ref == nil {
			continue
		}
		matches = append(matches, &convRefWithHarness{Ref: ref, Harness: h})
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Ref.Harness)
		}
		return nil, fmt.Errorf(
			"ambiguous conversation id %q: matches conversations in %s — use the full id",
			idPrefix, strings.Join(names, " and "))
	}
	if len(matches) == 0 {
		if len(failures) > 0 {
			return nil, errors.Join(failures...)
		}
		return nil, nil
	}

	match := matches[0]
	// A store that maintains the cache (Copilot, OpenCode) fills in the full
	// row — title, timestamps, counts — as a side effect of listing. The error
	// is deliberately ignored: enrichment is a bonus, and the minimal row
	// written below is enough for the caller either way.
	_, _ = match.Harness.Convs.ListConvs("")
	if row, err := db.GetConvIndex(match.Ref.ConvID); err == nil && row != nil {
		return row, nil
	}
	// A store that does NOT maintain the cache still has to be archivable, so
	// write the minimum the column needs: identity, project and harness. The
	// title stays empty rather than guessed.
	title, _ := match.Harness.Convs.Title(match.Ref.ConvID)
	row := &db.ConvIndexRow{
		ConvID:      match.Ref.ConvID,
		ProjectDir:  match.Ref.ProjectPath,
		ProjectPath: match.Ref.ProjectPath,
		Summary:     title,
		IndexedAt:   time.Now(),
		Harness:     match.Ref.Harness,
	}
	if err := db.UpsertConvIndex(row); err != nil {
		return nil, fmt.Errorf("cache conversation %s: %w", match.Ref.ConvID, err)
	}
	return row, nil
}

// convRefWithHarness pairs a resolved reference with the store that resolved
// it, so the ambiguity check can run before any store is asked to do work.
type convRefWithHarness struct {
	Ref     *harness.ConvRef
	Harness *harness.Harness
}
