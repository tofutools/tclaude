package agentd

import (
	"log/slog"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// harnessForConv resolves the harness a conversation's live session runs
// under, defaulting to the Claude Code harness when the harness is unknown
// or unregistered. Every in-pane slash injection in agentd sources its
// command tokens (/rename, /compact, /exit) from this harness's Lifecycle,
// so a pane is never typed a command the harness can't parse. Today every
// session is tagged "claude", so this is the CC harness and the tokens are
// the same literals as before the seam.
func harnessForConv(convID string) *harness.Harness {
	if rows, err := db.FindSessionsByConvID(convID); err == nil {
		for _, s := range rows {
			if s.Harness == "" {
				continue
			}
			if h, err := harness.Resolve(s.Harness); err == nil {
				return h
			}
			// An unknown tag (a harness this build doesn't register) falls
			// through to the default rather than failing the operation.
			slog.Warn("harnessForConv: unknown harness tag; defaulting to claude",
				"conv", convID, "harness", s.Harness)
			break
		}
	}
	return harness.Default()
}

// resolveSpawnHarness resolves a requested harness name for a daemon
// spawn, trimming surrounding whitespace first. It delegates to
// harness.ResolveSpawnable, so an empty name is the default (Claude Code)
// and an unknown or not-yet-spawnable harness is an error the spawn
// boundary surfaces as a 400 — rather than a silent failure once the
// forked session exits. The returned harness's Models is guaranteed
// non-nil so the caller can validate effort/model through it.
func resolveSpawnHarness(name string) (*harness.Harness, error) {
	return harness.ResolveSpawnable(strings.TrimSpace(name))
}

// deliverRename renames a conversation the way its harness dictates and
// reports whether delivery succeeded. A harness with an in-pane rename
// command (Claude Code's /rename) gets it injected into the live pane; one
// without (e.g. Codex, which has no TUI rename command) is renamed
// out-of-band through its ConvStore.SetTitle.
//
// The title must already be charset-validated by the caller — on the
// injection path it becomes literal send-keys input, so an un-gated title
// is a keystroke-injection sink (a newline would submit early). deliverRename
// does NOT validate; it only routes.
func deliverRename(convID, title string) bool {
	h := harnessForConv(convID)

	// Slash-injection rename (Claude Code): type `<rename-cmd> <title>`
	// into the live pane. RenameCommand is a compile-time constant, never
	// caller input, so it adds no injection surface.
	if h.SupportsRename() {
		return injectSlashCommand(convID, h.Life.RenameCommand()+" "+title, "")
	}

	// Out-of-band rename (direct title store): no live pane needed.
	if h.SupportsConvs() {
		if err := h.Convs.SetTitle(convID, title); err != nil {
			slog.Warn("rename: ConvStore.SetTitle failed",
				"conv", convID, "harness", h.Name, "error", err)
			return false
		}
		return true
	}

	slog.Warn("rename: harness supports neither an in-pane rename nor a title store",
		"conv", convID, "harness", h.Name)
	return false
}
