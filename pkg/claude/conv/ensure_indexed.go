package conv

import (
	"fmt"
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
// A resolve error (an ambiguous prefix, or an unreadable store) is SURFACED
// rather than folded into "not found": a caller about to report "no such
// conversation" must not do so because a store could not be read.
func ensureIndexedConv(idPrefix string) (*db.ConvIndexRow, error) {
	if idPrefix == "" {
		return nil, nil
	}
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
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if ref == nil {
			continue
		}
		// A store that maintains the cache (Copilot, OpenCode) fills in the
		// full row — title, timestamps, counts — as a side effect of listing.
		// The error is deliberately ignored: enrichment is a bonus, and the
		// minimal row written below is enough for the caller either way.
		_, _ = h.Convs.ListConvs("")
		if row, err := db.GetConvIndex(ref.ConvID); err == nil && row != nil {
			return row, nil
		}
		// A store that does NOT maintain the cache still has to be archivable,
		// so write the minimum the column needs: identity, project and
		// harness. The title stays empty rather than guessed.
		title, _ := h.Convs.Title(ref.ConvID)
		row := &db.ConvIndexRow{
			ConvID:      ref.ConvID,
			ProjectDir:  ref.ProjectPath,
			ProjectPath: ref.ProjectPath,
			Summary:     title,
			IndexedAt:   time.Now(),
			Harness:     ref.Harness,
		}
		if err := db.UpsertConvIndex(row); err != nil {
			return nil, fmt.Errorf("%s: cache conversation %s: %w", name, ref.ConvID, err)
		}
		return row, nil
	}
	return nil, nil
}
