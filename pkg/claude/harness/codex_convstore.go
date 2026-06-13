package harness

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// This file is the Codex analog of claude.go's claudeConvStore: it
// assembles convops.SessionEntry values from Codex's split storage model
// (date-indexed rollout files + the threads state DB) so every downstream
// reader — conv ls, search, the dashboard — stays harness-agnostic. The
// three exported behaviors (list / resolve / title) are thin wrappers in
// codex.go over the interface-free helpers here, which take an explicit
// `home` so they are testable against a temp HOME.
//
// Assembly model: the rollout files are the enumeration source and the
// fallback; the threads DB enriches. A rollout WITH a threads row is
// assembled from the row (title, cwd, branch, model, timestamps) without
// reading the file — which is also how a cold `.jsonl.zst` is handled
// without decompressing. A rollout WITHOUT a row is parsed from its head.

// scanCodexEntries walks the rollout tree and assembles one SessionEntry
// per rollout file, enriched by the threads DB where a row exists. cwd ==
// "" is the documented sentinel for "all conversations across every
// working directory"; a non-empty cwd keeps only entries whose real
// project path matches. A missing rollout tree yields no entries (not an
// error); an unreadable threads DB degrades to rollout-only assembly with
// a warning rather than failing the whole listing.
func scanCodexEntries(home, cwd string) ([]convops.SessionEntry, error) {
	paths, err := scanCodexRollouts(home)
	if err != nil {
		return nil, fmt.Errorf("scan codex rollouts: %w", err)
	}
	threads, err := loadCodexThreads(home)
	if err != nil {
		slog.Warn("codex convstore: threads DB unreadable, assembling from rollouts only", "error", err)
		threads = map[string]codexThread{}
	}

	var entries []convops.SessionEntry
	for _, path := range paths {
		id := codexIDFromRolloutName(filepath.Base(path))
		if id == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue // file vanished between the walk and the stat
		}

		var entry convops.SessionEntry
		if t, ok := threads[id]; ok {
			entry = codexThreadEntry(t, id, path, info)
		} else {
			entry, err = codexRolloutEntry(id, path, info)
			if err != nil {
				slog.Warn("codex convstore: rollout parse failed, skipping", "path", path, "error", err)
				continue
			}
		}

		if cwd != "" && entry.ProjectPath != cwd {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// codexThreadEntry assembles a SessionEntry from a threads row + the file's
// stat — the enriched path, taken whenever a row exists (so a cold `.zst`
// never has to be decompressed). The title's rename-vs-derived split drives
// CustomTitle (see codexIsRename).
func codexThreadEntry(t codexThread, id, path string, info os.FileInfo) convops.SessionEntry {
	e := convops.SessionEntry{
		SessionID:    id,
		FullPath:     path,
		FileMtime:    info.ModTime().Unix(),
		FileSize:     info.Size(),
		FirstPrompt:  t.FirstUserMessage,
		ProjectPath:  t.Cwd,
		GitBranch:    t.GitBranch,
		Created:      codexUnixToRFC3339(t.CreatedAt),
		Modified:     info.ModTime().UTC().Format(time.RFC3339),
		Model:        t.Model,
		Harness:      codexHarnessName,
		MessageCount: 0, // not surfaced for Codex in v1 (documented)
	}
	if e.FirstPrompt == "" {
		e.FirstPrompt = t.Preview
	}
	if codexIsRename(t.Title, t.FirstUserMessage) {
		e.CustomTitle = t.Title
	}
	if t.Archived {
		e.ArchivedAt = codexArchivedAt(t)
	}
	return e
}

// codexRolloutEntry assembles a SessionEntry by parsing the rollout head —
// the fallback path for a session with no threads row. With no DB there is
// no rename signal, so CustomTitle stays empty (the title is the derived
// first prompt) and GitBranch/ArchivedAt are unknown.
func codexRolloutEntry(id, path string, info os.FileInfo) (convops.SessionEntry, error) {
	head, err := parseCodexRolloutHead(path)
	if err != nil {
		return convops.SessionEntry{}, err
	}
	return convops.SessionEntry{
		SessionID:    id,
		FullPath:     path,
		FileMtime:    info.ModTime().Unix(),
		FileSize:     info.Size(),
		FirstPrompt:  head.FirstUserMsg,
		ProjectPath:  head.Cwd,
		Created:      codexRolloutCreated(head, info),
		Modified:     info.ModTime().UTC().Format(time.RFC3339),
		Model:        head.Model,
		Harness:      codexHarnessName,
		MessageCount: 0,
	}, nil
}

// codexIsRename decides whether a threads.title is a real user rename or
// just Codex's auto-derived title. Codex derives an un-renamed title from
// the first user message — sometimes verbatim (short single-line
// messages, where title == first_user_message), sometimes as the trimmed
// first line (long/multi-line messages, where title == first-line of
// first_user_message). Comparing against BOTH forms keeps a long-message
// auto-title from reading as a rename, narrowing the known false-positive
// window the rename-detection spike (JOH-161) will close properly.
func codexIsRename(title, firstUserMsg string) bool {
	if title == "" {
		return false
	}
	if title == firstUserMsg {
		return false
	}
	if title == codexFirstLine(firstUserMsg) {
		return false
	}
	return true
}

// codexFirstLine trims a message to a one-line, 80-char preview — the way
// Codex derives a title/preview from the first user message. Used only for
// the rename heuristic; the stored FirstPrompt keeps the full message.
func codexFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 80
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// codexUnixToRFC3339 formats unix seconds as an RFC3339 UTC string (the
// SessionEntry timestamp shape Claude Code uses). A zero/negative stamp
// yields "" — an empty Created marks a stub, as in the CC parser.
func codexUnixToRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// codexRolloutCreated derives Created from the session_meta timestamp
// (already UTC, RFC3339 with millis), normalised to plain RFC3339. A
// missing/unparseable stamp falls back to the file mtime so Created is
// never empty for a real rollout.
func codexRolloutCreated(head *codexRollout, info os.FileInfo) string {
	if head.Created != "" {
		if ts, err := time.Parse(time.RFC3339, head.Created); err == nil {
			return ts.UTC().Format(time.RFC3339)
		}
	}
	return info.ModTime().UTC().Format(time.RFC3339)
}

// codexArchivedAt renders the archived timestamp as RFC3339 UTC. It
// prefers threads.archived_at, falling back to updated_at then created_at
// when a row is flagged archived but carries no explicit archived_at — the
// column is nullable and older rows may not have set it. Returns "" only
// when no usable timestamp exists.
func codexArchivedAt(t codexThread) string {
	var sec int64
	switch {
	case t.ArchivedAt.Valid && t.ArchivedAt.Int64 > 0:
		sec = t.ArchivedAt.Int64
	case t.UpdatedAt > 0:
		sec = t.UpdatedAt
	case t.CreatedAt > 0:
		sec = t.CreatedAt
	default:
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// resolveCodex maps a (possibly short) id prefix to a ConvRef. It mirrors
// the ConvStore contract exactly: (nil, nil) for no match, (nil, err) for
// a scan failure OR an ambiguous prefix, and an exact id always wins over
// prefix matches. cwd scopes the search to one project unless global.
func resolveCodex(home, idPrefix, cwd string, global bool) (*ConvRef, error) {
	if idPrefix == "" {
		return nil, nil
	}
	filterCwd := cwd
	if global {
		filterCwd = ""
	}
	entries, err := scanCodexEntries(home, filterCwd)
	if err != nil {
		return nil, fmt.Errorf("resolve conversation %q: %w", idPrefix, err)
	}

	// An exact id match is unambiguous regardless of how many ids share it
	// as a prefix.
	for _, e := range entries {
		if e.SessionID == idPrefix {
			return codexConvRef(e), nil
		}
	}

	var matches []convops.SessionEntry
	for _, e := range entries {
		if strings.HasPrefix(e.SessionID, idPrefix) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return codexConvRef(matches[0]), nil
	default:
		return nil, fmt.Errorf("ambiguous conversation id %q: matches %d conversations", idPrefix, len(matches))
	}
}

// codexTitle returns a conversation's display title: the threads.title
// when it's a real rename, otherwise the derived first user message
// (threads.first_user_message, or the rollout's first user message when
// there's no row). An unknown conv yields ("", nil). Mirrors a
// SessionEntry's DisplayTitle for the same conv so list and title agree.
func codexTitle(home, convID string) (string, error) {
	threads, err := loadCodexThreads(home)
	if err != nil {
		slog.Warn("codex convstore: threads DB unreadable, deriving title from rollout", "error", err)
		threads = map[string]codexThread{}
	}
	if t, ok := threads[convID]; ok {
		if codexIsRename(t.Title, t.FirstUserMessage) {
			return t.Title, nil
		}
		switch {
		case t.FirstUserMessage != "":
			return t.FirstUserMessage, nil
		case t.Preview != "":
			return t.Preview, nil
		default:
			return t.Title, nil
		}
	}

	path, err := findCodexRollout(home, convID)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	head, err := parseCodexRolloutHead(path)
	if err != nil {
		return "", err
	}
	return head.FirstUserMsg, nil
}

// codexConvRef projects a SessionEntry to the minimal handle Resolve
// returns. ProjectPath is the REAL cwd (threads.cwd / session_meta.cwd),
// the `codex resume` target.
func codexConvRef(e convops.SessionEntry) *ConvRef {
	return &ConvRef{
		ConvID:      e.SessionID,
		ProjectPath: e.ProjectPath,
		Harness:     codexHarnessName,
	}
}
