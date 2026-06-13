package conv

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// appendNonClaudeHarnessEntries merges conversations from every registered
// harness OTHER than Claude Code into entries the caller already loaded for
// the same working directory. cwd is the real working directory to scope
// to; the empty string is the documented "all working directories"
// (global) sentinel, matching ConvStore.ListConvs.
//
// Claude Code is deliberately skipped here: the caller loads CC through its
// rich LoadSessionsIndex path (mtime cache, --reindex, branch history),
// which routing CC through its ConvStore would only duplicate. Every other
// harness (Codex today) is read-only and tags its own entries with
// Harness, so the merged list stays harness-agnostic for the downstream
// filter/sort/render.
//
// A harness whose enumeration fails is logged and skipped — one harness's
// unreadable store must never break `conv ls` / `conv search` for the rest.
func appendNonClaudeHarnessEntries(entries []SessionEntry, cwd string) []SessionEntry {
	for _, name := range harness.Names() {
		if name == harness.DefaultName {
			continue
		}
		h, ok := harness.Get(name)
		if !ok || h.Convs == nil {
			continue
		}
		got, err := h.Convs.ListConvs(cwd)
		if err != nil {
			slog.Warn("conv: harness conversation enumeration failed; skipping",
				"harness", name, "error", err)
			continue
		}
		entries = append(entries, got...)
	}
	return entries
}
