// Package convops provides conversation file operations shared between packages.
package convops

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// SystemTags are Claude Code system tags that should be stripped entirely
// (both the tags and their content) from display text. Lives here rather
// than in convindex so the convops parser doesn't pull convindex →
// convops back-edge.
var SystemTags = []string{
	"local-command-caveat",
	"command-name",
	"command-message",
	"command-args",
	"local-command-stdout",
	"system-reminder",
}

// SessionEntry represents a single session/conversation in the index
type SessionEntry struct {
	SessionID    string `json:"sessionId"`
	FullPath     string `json:"fullPath"`
	FileMtime    int64  `json:"fileMtime"`
	FirstPrompt  string `json:"firstPrompt"`
	Summary      string `json:"summary,omitempty"`
	CustomTitle  string `json:"customTitle,omitempty"`
	MessageCount int    `json:"messageCount"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	GitBranch    string `json:"gitBranch"`
	// GitBranchStartup is the branch the FIRST turn was stamped with —
	// the branch Claude Code was launched on. First-wins and immutable,
	// the counterpart to GitBranch's last-wins "current branch".
	GitBranchStartup string `json:"gitBranchStartup,omitempty"`
	ProjectPath      string `json:"projectPath"`
	IsSidechain      bool   `json:"isSidechain"`
	FileSize         int64  `json:"-"` // Populated at load time, not persisted in index
	// ArchivedAt is the canonical archived signal sourced from
	// `conv_index.archived_at`. RFC3339 timestamp string when archived,
	// empty when active. Populated at load time from the DB row;
	// `IsArchived()` checks this column first, with the `-x` title
	// suffix as a fallback for legacy convs that pre-date the column.
	ArchivedAt string `json:"-"`
	// BranchHistory is the distinct set of git branches this .jsonl
	// touched — every branch stamped onto a turn, with the dir + the
	// timestamps bracketing where it appeared. Populated only by a
	// fresh parseJSONLSession scan (empty on a DB cache hit, where the
	// branch history already persisted from the prior scan), and never
	// stored on the conv_index row; LoadSessionsIndexWithOptions feeds
	// it to db.RebuildConvBranchHistoryScan.
	BranchHistory []db.BranchObservation `json:"-"`
}

// DisplayTitle returns the best available title for display
func (e *SessionEntry) DisplayTitle() string {
	if e.CustomTitle != "" {
		return e.CustomTitle
	}
	if e.Summary != "" {
		return e.Summary
	}
	return e.FirstPrompt
}

// IsArchivedTitle returns true when a CustomTitle ends with the `-x`
// archived-marker suffix that reincarnate writes onto the old conv's
// .jsonl right before /exit. Used by listing surfaces (conv ls,
// dashboard) to default-hide dead convs without needing a separate
// "is_active" column. Only checks CustomTitle — Summary / FirstPrompt
// happening to end with `-x` is coincidental, not an archive mark.
//
// Mnemonic: `-x` = archived (mark of expiration / supersession).
// Pairs with `-r-N` (reincarnated successor) and `-c-N` (clone) on
// the live side. Unifies with `groups archive` — both are
// soft-delete states.
//
// Note: this is the title-based fallback. The canonical check is the
// (future) `conv_index.archived_at` column; this helper covers
// legacy convs that pre-date the column. New code should prefer the
// column-based check when one is available.
func IsArchivedTitle(customTitle string) bool {
	return strings.HasSuffix(customTitle, "-x")
}

// IsArchived is the canonical archived check on a SessionEntry. Reads
// the conv_index.archived_at column (preferred) and falls back to
// the title-suffix marker for legacy convs that pre-date schema v17.
// Either signal is enough to mark the conv as archived.
func (e *SessionEntry) IsArchived() bool {
	if e.ArchivedAt != "" {
		return true
	}
	return IsArchivedTitle(e.CustomTitle)
}

// HasTitle returns true if the entry has a custom title or summary
func (e *SessionEntry) HasTitle() bool {
	return e.CustomTitle != "" || e.Summary != ""
}

// SessionsIndex represents the sessions-index.json file
type SessionsIndex struct {
	Version int            `json:"version"`
	Entries []SessionEntry `json:"entries"`
}

// ClaudeProjectsDir returns the Claude projects directory path
func ClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// PathToProjectDir converts a real path to the Claude project directory name
func PathToProjectDir(realPath string) string {
	absPath, err := filepath.Abs(realPath)
	if err != nil {
		absPath = realPath
	}
	projectDir := strings.ReplaceAll(absPath, string(filepath.Separator), "-")
	projectDir = strings.ReplaceAll(projectDir, ".", "-")
	projectDir = strings.ReplaceAll(projectDir, ":", "")
	return projectDir
}

// GetClaudeProjectPath returns the full path to a Claude project directory
func GetClaudeProjectPath(realPath string) string {
	return filepath.Join(ClaudeProjectsDir(), PathToProjectDir(realPath))
}

// DebugLog controls whether LoadSessionsIndex prints debug timing information.
// Flipped on by `tclaude conv ls --verbose`.
var DebugLog = false

// LoadSessionsIndexOptions configures LoadSessionsIndex behavior.
type LoadSessionsIndexOptions struct {
	// ForceRescan forces a full rescan of all entries regardless of mtime.
	ForceRescan bool
}

// LoadSessionsIndex loads conversations for a Claude project directory.
// Uses our SQLite conv_index as cache, scanning .jsonl files only when
// their mtime has changed. This is the SINGLE source-of-truth lookup
// path used across the codebase — conv listing, clone, git status, etc.
// The legacy `sessions-index.json` file is no longer consulted (it may
// still be written for tooling compatibility but is otherwise ignored).
func LoadSessionsIndex(projectPath string) (*SessionsIndex, error) {
	return LoadSessionsIndexWithOptions(projectPath, LoadSessionsIndexOptions{})
}

// LoadSessionsIndexWithOptions loads conversations with configurable behavior.
// Flow: list .jsonl files -> check DB mtime -> parse only if stale/new ->
// return entries from DB. Files that have disappeared from disk are
// evicted from the SQLite cache.
func LoadSessionsIndexWithOptions(projectPath string, opts LoadSessionsIndexOptions) (*SessionsIndex, error) {
	start := time.Now()

	// 1. List .jsonl files on disk
	files, err := os.ReadDir(projectPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &SessionsIndex{Version: 1, Entries: []SessionEntry{}}, nil
		}
		return nil, fmt.Errorf("failed to read project dir: %w", err)
	}

	// 2. Load existing DB entries for this project
	dbRows, err := db.ListConvIndex(projectPath)
	if err != nil {
		slog.Warn("conv_index: db read failed, will scan all files", "error", err)
		dbRows = nil
	}
	dbByID := make(map[string]*db.ConvIndexRow, len(dbRows))
	for _, r := range dbRows {
		dbByID[r.ConvID] = r
	}

	// 3. For each .jsonl file, use DB cache or scan
	var entries []SessionEntry
	seenIDs := make(map[string]bool)
	scannedCount := 0

	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}
		convID := strings.TrimSuffix(file.Name(), ".jsonl")
		if len(convID) != 36 { // UUID length
			continue
		}
		seenIDs[convID] = true

		filePath := filepath.Join(projectPath, file.Name())
		info, err := file.Info()
		if err != nil {
			continue
		}
		fileMtime := info.ModTime().Unix()
		fileSize := info.Size()

		// Serve a fresh, non-stub cache hit without re-scanning.
		//
		// A stub row is deliberately NOT trusted as fresh. A stub only
		// records that the LAST scan found nothing indexable — but the
		// scanner improves over time (it used to discard conversations
		// that were named before their first turn), and a file can gain
		// content after a stub was written. Stub files are tiny, so we
		// always re-scan them: a stub left by older logic then
		// self-heals into a real row. `tclaude conv prune-empty` is the
		// cleanup path for the underlying genuinely-empty .jsonl files.
		if cached, ok := dbByID[convID]; ok && !opts.ForceRescan && cached.FileMtime >= fileMtime && !isStubRow(cached) {
			entries = append(entries, dbRowToEntry(cached, fileSize))
			continue
		}

		// Need to scan the file
		scannedCount++
		slog.Info("conv_index: scanning file",
			"conv_id", convID[:8],
			"project", filepath.Base(projectPath),
			"reason", scanReason(dbByID[convID], opts.ForceRescan))

		scanned, scanComplete := parseJSONLSession(filePath, convID)
		if scanned == nil {
			// File has no useful data (e.g., only file-history-snapshot lines).
			// Store a stub so we don't rescan on every startup.
			stub := &db.ConvIndexRow{
				ConvID:     convID,
				ProjectDir: projectPath,
				FullPath:   filePath,
				FileMtime:  fileMtime,
				FileSize:   fileSize,
			}
			if err := db.UpsertConvIndex(stub); err != nil {
				slog.Warn("conv_index: db upsert stub failed", "conv_id", convID[:8], "error", err)
			}
			// A conv that once had branch-stamped turns and was later
			// truncated to stub-only content must shed its stale scan
			// rows — an empty rebuild drops them (hook rows survive),
			// keeping the history a true mirror of the .jsonl. Only when
			// the scan reached EOF: a truncated scan is not evidence the
			// branches are gone. Not gated on the stub upsert (unlike
			// the create path below): an empty rebuild only DELETES scan
			// rows, so it can never strand one — and running it even on
			// a failed stub upsert still reclaims them.
			if scanComplete {
				if err := db.RebuildConvBranchHistoryScan(convID, nil); err != nil {
					slog.Warn("conv_branch_history: stub rebuild failed", "conv_id", convID[:8], "error", err)
				}
			}
			continue
		}
		scanned.FileSize = fileSize

		// Upsert into DB
		row := entryToDBRow(scanned, projectPath)
		convIndexed := true
		if err := db.UpsertConvIndex(row); err != nil {
			slog.Warn("conv_index: db upsert failed", "conv_id", convID[:8], "error", err)
			convIndexed = false
		}

		// Rebuild this conv's branch history from the scan. The scan is
		// the source of truth: re-running it with the same .jsonl
		// converges to the same rows, so the history self-heals on
		// every re-scan rather than depending on incremental state.
		// Only the scan path reaches here — a DB cache hit keeps the
		// branch history persisted from the prior scan.
		//
		// Gated on the conv_index upsert succeeding: branch-history
		// rows are reclaimed by an eviction sweep that walks conv_index,
		// so writing them for a conv with no conv_index row would strand
		// them. Also gated on scanComplete — a truncated scan's branch
		// set is partial, and a rebuild would delete the branches past
		// the truncation point. A skipped rebuild self-heals on the
		// next successful scan.
		if convIndexed && scanComplete {
			if err := db.RebuildConvBranchHistoryScan(convID, scanned.BranchHistory); err != nil {
				slog.Warn("conv_branch_history: rebuild failed", "conv_id", convID[:8], "error", err)
			}
		}

		entries = append(entries, *scanned)
	}

	// 4. Remove DB entries for files that no longer exist on disk
	for convID := range dbByID {
		if !seenIDs[convID] {
			if err := db.DeleteConvIndex(convID); err != nil {
				slog.Warn("conv_index: db delete failed", "conv_id", convID[:8], "error", err)
			}
			// The branch history is keyed off the same conv — evict it
			// alongside conv_index so the table tracks live convs only.
			if err := db.DeleteConvBranchHistory(convID); err != nil {
				slog.Warn("conv_branch_history: db delete failed", "conv_id", convID[:8], "error", err)
			}
		}
	}

	if DebugLog {
		fmt.Fprintf(os.Stderr, "[DEBUG] LoadSessionsIndex %s: total=%d scanned=%d cached=%d elapsed=%v\n",
			filepath.Base(projectPath), len(entries), scannedCount, len(entries)-scannedCount, time.Since(start))
	}

	backfillProjectPaths(entries)
	return &SessionsIndex{Version: 1, Entries: entries}, nil
}

// scanReason returns a human-readable reason for why a file is being scanned.
func scanReason(cached *db.ConvIndexRow, forceRescan bool) string {
	if forceRescan {
		return "force-rescan"
	}
	if cached == nil {
		return "new-file"
	}
	return "mtime-changed"
}

// dbRowToEntry converts a DB row to a SessionEntry.
func dbRowToEntry(r *db.ConvIndexRow, fileSize int64) SessionEntry {
	archived := ""
	if !r.ArchivedAt.IsZero() {
		archived = r.ArchivedAt.Format(time.RFC3339Nano)
	}
	return SessionEntry{
		SessionID:        r.ConvID,
		FullPath:         r.FullPath,
		FileMtime:        r.FileMtime,
		FirstPrompt:      r.FirstPrompt,
		Summary:          r.Summary,
		CustomTitle:      r.CustomTitle,
		MessageCount:     r.MessageCount,
		Created:          r.Created,
		Modified:         r.Modified,
		GitBranch:        r.GitBranch,
		GitBranchStartup: r.GitBranchStartup,
		ProjectPath:      r.ProjectPath,
		IsSidechain:      r.IsSidechain,
		FileSize:         fileSize,
		ArchivedAt:       archived,
	}
}

// entryToDBRow converts a SessionEntry to a DB row for storage.
func entryToDBRow(e *SessionEntry, projectDir string) *db.ConvIndexRow {
	return &db.ConvIndexRow{
		ConvID:           e.SessionID,
		ProjectDir:       projectDir,
		FullPath:         e.FullPath,
		FileMtime:        e.FileMtime,
		FileSize:         e.FileSize,
		FirstPrompt:      e.FirstPrompt,
		Summary:          e.Summary,
		CustomTitle:      e.CustomTitle,
		MessageCount:     e.MessageCount,
		Created:          e.Created,
		Modified:         e.Modified,
		GitBranch:        e.GitBranch,
		GitBranchStartup: e.GitBranchStartup,
		ProjectPath:      e.ProjectPath,
		IsSidechain:      e.IsSidechain,
		IndexedAt:        time.Now(),
	}
}

// JSONLMessage represents a line in the .jsonl conversation file.
// Exported so the rest of the codebase doesn't need to duplicate the
// shape when streaming .jsonl files.
type JSONLMessage = jsonlMessage

// jsonlMessage is the internal representation used by parseJSONLSession.
// JSONLMessage is the exported alias for callers outside this package.
type jsonlMessage struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	Timestamp   string `json:"timestamp"`
	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Summary     string `json:"summary"`     // For type="summary" messages
	CustomTitle string `json:"customTitle"` // For type="custom-title" messages
	Message     struct {
		Role    string `json:"role"`
		Content any    `json:"content"` // Can be string or array
	} `json:"message"`
}

// ParseJSONLSessionPublic is the exported version of parseJSONLSession for use by repair code.
func ParseJSONLSessionPublic(filePath, sessionID string) *SessionEntry {
	entry, _ := parseJSONLSession(filePath, sessionID)
	return entry
}

// maxJSONLLineBytes caps a single .jsonl line in parseJSONLSession. A
// turn carrying a big tool result can be large, so the default is
// generous (10 MiB); a line past it makes bufio.Scanner stop with
// bufio.ErrTooLong, which parseJSONLSession reports as an incomplete
// scan. A var, not a const, so a test can shrink it to force that
// path without writing a 10 MiB fixture.
var maxJSONLLineBytes = 10 * 1024 * 1024

// parseJSONLTimestamp best-effort parses a .jsonl turn timestamp into a
// time.Time. Claude Code writes RFC3339 with a fractional-second part;
// an empty or unparseable value yields the zero time, which callers
// treat as "no timestamp" rather than an error.
func parseJSONLTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

// parseJSONLSession parses a .jsonl file and extracts session metadata.
// Reads forward for prompt/title/summary and accumulates the conv's
// branch history; runs to EOF so the last-wins branch and the history
// set are both complete.
//
// The second return value is the scan-complete flag: false when the
// file couldn't be opened, or when the bufio.Scanner stopped on an
// error (an I/O failure, or a line past maxJSONLLineBytes) rather than
// at EOF. A caller must NOT treat the BranchHistory of an incomplete
// scan as authoritative — RebuildConvBranchHistoryScan deletes
// unobserved rows, so rebuilding from a truncated set would drop real
// branches.
func parseJSONLSession(filePath, sessionID string) (*SessionEntry, bool) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, false
	}

	var entry SessionEntry
	entry.SessionID = sessionID
	entry.FullPath = filePath
	entry.FileMtime = info.ModTime().Unix()
	entry.FileSize = info.Size()

	scanner := bufio.NewScanner(file)
	// bufio.Scanner's effective max-token size is the larger of the max
	// arg and the initial buffer's cap, so the initial cap must not
	// exceed maxJSONLLineBytes or a shrunk limit (tests) would have no
	// effect. 64 KiB is the normal initial cap.
	initCap := 64 * 1024
	if maxJSONLLineBytes < initCap {
		initCap = maxJSONLLineBytes
	}
	scanner.Buffer(make([]byte, 0, initCap), maxJSONLLineBytes)

	var firstTimestamp string

	// branchAccum gathers, per (canonical repo dir, branch), the
	// timestamps bracketing its appearance — folded into
	// entry.BranchHistory once the whole file is read. The repo dir is
	// part of the key: one conversation can touch the same branch name
	// in two different repos, and those are distinct history entries.
	type branchAccum struct {
		repoDir   string
		branch    string
		firstSeen time.Time
		lastSeen  time.Time
	}
	branches := map[string]*branchAccum{}
	// canonCwd memoises db.CanonicalizeRepoDir. Claude Code stamps a
	// fixed cwd onto every turn, so this resolves to a single entry —
	// the per-turn cost is a map hit, not a filesystem stat.
	canonCwd := map[string]string{}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg jsonlMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Track first timestamp
		if firstTimestamp == "" && msg.Timestamp != "" {
			firstTimestamp = msg.Timestamp
		}

		// Capture project path from the first message that has it — the
		// cwd is fixed for the life of a conversation, so first-wins.
		if entry.ProjectPath == "" && msg.Cwd != "" {
			entry.ProjectPath = msg.Cwd
		}
		// Git branch, by contrast, can change mid-conversation (a
		// `git checkout`, a new worktree). Claude Code stamps the
		// *current* branch onto every turn, so keep the LAST one seen
		// in GitBranch: the index then reflects where the agent is now.
		// GitBranchStartup keeps the FIRST one — the launch branch —
		// so a UI can show an immutable "init" alongside "now".
		if msg.GitBranch != "" {
			if entry.GitBranchStartup == "" {
				entry.GitBranchStartup = msg.GitBranch
			}
			entry.GitBranch = msg.GitBranch

			// Accumulate the branch into the history set, keyed by the
			// canonical repo dir + branch. Turns are chronological, so
			// first/last seen fall out of min/max over their
			// (best-effort parsed) timestamps.
			repoDir, ok := canonCwd[msg.Cwd]
			if !ok {
				repoDir = db.CanonicalizeRepoDir(msg.Cwd)
				canonCwd[msg.Cwd] = repoDir
			}
			accKey := repoDir + "\x00" + msg.GitBranch
			acc := branches[accKey]
			if acc == nil {
				acc = &branchAccum{repoDir: repoDir, branch: msg.GitBranch}
				branches[accKey] = acc
			}
			if ts := parseJSONLTimestamp(msg.Timestamp); !ts.IsZero() {
				if acc.firstSeen.IsZero() || ts.Before(acc.firstSeen) {
					acc.firstSeen = ts
				}
				if ts.After(acc.lastSeen) {
					acc.lastSeen = ts
				}
			}
		}

		// Capture custom title (written by Claude Code after the first exchange)
		if msg.Type == "custom-title" && msg.CustomTitle != "" {
			entry.CustomTitle = msg.CustomTitle
		}

		// Capture summary (keep last one seen)
		if msg.Type == "summary" && msg.Summary != "" {
			entry.Summary = msg.Summary
		}

		// Capture first user message with actual text content as the prompt
		if entry.FirstPrompt == "" && msg.Type == "user" && msg.Message.Role == "user" {
			text := extractMessageContent(msg.Message.Content)
			// Skip messages without text (e.g., tool_result blocks from resumed sessions)
			// Also skip system-generated messages like "[Request interrupted by user...]"
			// and system-injected messages (local-command-caveat, command-name, etc.)
			if text != "" && !strings.HasPrefix(text, "[Request interrupted") && !isSystemInjectedMessage(text) {
				entry.FirstPrompt = text
				if msg.Timestamp != "" {
					firstTimestamp = msg.Timestamp
				}
			}
		}

		// The scan deliberately runs to EOF. An earlier version broke
		// out once CustomTitle/Summary/FirstPrompt/ProjectPath were all
		// set, but that cut a compacted conv short and reported its
		// branch as of the early-exit point — and would now miss every
		// branch the agent moved onto after a summary turn. Reading the
		// whole file keeps GitBranch (last-wins) exact and gives
		// BranchHistory the complete set. This path only runs on a
		// cache miss, so the extra reads are infrequent.
	}

	// A scanner that stopped on an error rather than at EOF gives a
	// TRUNCATED view: the BranchHistory below is a partial set, and
	// rebuilding from it would let RebuildConvBranchHistoryScan delete
	// real branches past the truncation point. Report it so the caller
	// skips the rebuild and leaves the existing history intact.
	scanComplete := true
	if err := scanner.Err(); err != nil {
		slog.Warn("conv_index: .jsonl scan stopped before EOF; branch history not rebuilt",
			"conv_id", sessionID, "error", err)
		scanComplete = false
	}

	// Fold the accumulated branches into the history set. Map order is
	// unstable, but RebuildConvBranchHistoryScan treats it as a set.
	for _, acc := range branches {
		entry.BranchHistory = append(entry.BranchHistory, db.BranchObservation{
			Branch:    acc.branch,
			RepoDir:   acc.repoDir,
			FirstSeen: acc.firstSeen,
			LastSeen:  acc.lastSeen,
		})
	}

	if firstTimestamp == "" {
		// No timestamped line anywhere in the file. Usually that's a
		// genuinely empty .jsonl with nothing to index — but a
		// conversation can be NAMED before it ever takes a turn: a
		// spawned/reincarnated agent is /rename'd at startup, so its
		// .jsonl holds a `custom-title` line (Claude Code's state-marker
		// lines are not timestamped) yet no user/assistant turn. That
		// named conversation is real and must be indexed — only a file
		// with no title, no summary AND no prompt is a true empty stub.
		if entry.CustomTitle == "" && entry.Summary == "" && entry.FirstPrompt == "" {
			return nil, scanComplete
		}
		// Named-but-turnless: fall back to the file mtime so Created is
		// non-empty (an empty Created marks a stub — see isStubRow).
		entry.Created = info.ModTime().UTC().Format(time.RFC3339)
	} else {
		entry.Created = firstTimestamp
	}
	entry.Modified = info.ModTime().UTC().Format(time.RFC3339)
	entry.MessageCount = 0

	return &entry, scanComplete
}

// IsSystemInjectedMessage returns true if the text is a system-injected
// message from Claude Code (not actual user input). These start with
// known system XML tags. Exported wrapper around the internal helper.
func IsSystemInjectedMessage(text string) bool {
	return isSystemInjectedMessage(text)
}

// ExtractMessageContent extracts text content from a JSONL message.
// Content can be a string or an array of content blocks.
func ExtractMessageContent(content any) string {
	return extractMessageContent(content)
}

// isSystemInjectedMessage returns true if the text is a system-injected message
// from Claude Code (not actual user input). These start with known system XML tags.
func isSystemInjectedMessage(text string) bool {
	if !strings.HasPrefix(text, "<") {
		return false
	}
	for _, tag := range SystemTags {
		if strings.HasPrefix(text, "<"+tag+">") {
			return true
		}
	}
	return false
}

// LoadEntriesFromDB loads conversation entries directly from the SQLite cache
// without touching the filesystem. Used by watch mode for fast refreshes.
// If projectPath is empty, loads all entries across all projects (global mode).
// Stub rows (placeholder entries for .jsonl files that contained no real
// session data; see isStubRow) are filtered out — same rationale as the
// LoadSessionsIndexWithOptions cache path.
func LoadEntriesFromDB(projectPath string) ([]SessionEntry, error) {
	var dbRows []*db.ConvIndexRow
	var err error
	if projectPath == "" {
		dbRows, err = db.ListAllConvIndex()
	} else {
		dbRows, err = db.ListConvIndex(projectPath)
	}
	if err != nil {
		return nil, err
	}

	entries := make([]SessionEntry, 0, len(dbRows))
	for _, r := range dbRows {
		if isStubRow(r) {
			continue
		}
		entries = append(entries, dbRowToEntry(r, r.FileSize))
	}
	backfillProjectPaths(entries)
	return entries, nil
}

// backfillProjectPaths fills in a missing ProjectPath from a sibling
// conversation in the same Claude project directory, and persists it.
//
// Claude Code stamps the working directory onto every conversation
// *turn*; a conversation that was named but never took a turn (see
// parseJSONLSession) records no cwd at all. But every .jsonl filed
// under the same ~/.claude/projects/<dir> was launched from the same
// cwd — so any sibling that did take a turn is an authoritative
// source. The key is the .jsonl's parent directory (filepath.Dir of
// FullPath): that IS the per-cwd Claude project directory.
//
// The derived cwd is written back onto the conv_index row, so it is
// resolved once and then served from the cache like any other field.
// LoadSessionsIndex and LoadEntriesFromDB call this, so the first
// `conv ls` / watch refresh after a named-but-turnless conversation
// appears heals its row for every reader.
func backfillProjectPaths(entries []SessionEntry) {
	pathByDir := make(map[string]string)
	for i := range entries {
		e := &entries[i]
		if e.ProjectPath == "" || e.FullPath == "" {
			continue
		}
		if dir := filepath.Dir(e.FullPath); pathByDir[dir] == "" {
			pathByDir[dir] = e.ProjectPath
		}
	}
	if len(pathByDir) == 0 {
		return
	}
	for i := range entries {
		e := &entries[i]
		if e.ProjectPath != "" || e.FullPath == "" {
			continue
		}
		p := pathByDir[filepath.Dir(e.FullPath)]
		if p == "" {
			continue
		}
		e.ProjectPath = p
		// Persist the derived cwd so every reader — the next watch
		// refresh, the dashboard, `agent` lookups — sees it without
		// re-deriving. Best-effort: a failed write only costs the
		// re-derivation on the next listing.
		if err := db.SetConvIndexProjectPath(e.SessionID, p); err != nil {
			slog.Warn("conv_index: project-path backfill write failed",
				"conv_id", e.SessionID, "error", err)
		}
	}
}

// isStubRow reports whether a conv_index row is a stub — the
// placeholder written when parseJSONLSession finds nothing indexable
// in a .jsonl (no first prompt, no summary, no custom title). Stubs
// sit in the DB and are hidden from listing surfaces.
//
// `Created` is the load-bearing signal: parseJSONLSession always sets
// Created on a non-nil entry — to the first turn's timestamp, or, for
// a conversation named before its first turn, to the file mtime. Only
// the nothing-indexable path leaves Created empty, so an empty Created
// uniquely identifies a stub.
//
// Stubs are re-scanned (not trusted as fresh) by LoadSessionsIndex and
// RefreshConvIndexEntry, so a stub written by older logic self-heals.
func isStubRow(r *db.ConvIndexRow) bool {
	return r != nil && r.Created == ""
}

// RefreshConvIndexEntry returns the conv_index row for convID, rescanning
// the underlying .jsonl file when its mtime is newer than the cached
// FileMtime. Mirrors the per-file freshness check that LoadSessionsIndex
// runs in its loop, but for a single conv — useful when a caller already
// knows the conv-id and doesn't want to enumerate the whole project dir.
//
// Returns the freshest row available; returns nil if the conv is unknown
// to the DB or its underlying file has been deleted (in which case we
// also evict the stale cache entry so callers don't keep resolving to it).
func RefreshConvIndexEntry(convID string) *db.ConvIndexRow {
	row, _ := db.GetConvIndex(convID)
	if row == nil {
		return nil
	}
	if row.FullPath == "" {
		return row
	}
	info, err := os.Stat(row.FullPath)
	if err != nil {
		// File disappeared between the cache write and now (manual
		// delete, log rotation, etc.). Drop the stale row so the next
		// lookup doesn't resolve to a ghost conv.
		if os.IsNotExist(err) {
			_ = db.DeleteConvIndex(convID)
			_ = db.DeleteConvBranchHistory(convID)
			return nil
		}
		return row
	}
	// Both mtime AND size must match the cached values. mtime alone
	// is insufficient — most filesystems report it at 1-second
	// resolution, so two writes inside the same second leave mtime
	// unchanged. The size check catches those (every JSONL append
	// grows the file), and is essentially free since os.Stat already
	// returned both.
	//
	// A stub row is never trusted as fresh — it only records that the
	// last scan found nothing indexable. Re-scan it (cheap; stub files
	// are tiny) so a stub written by older scanning logic self-heals.
	if info.ModTime().Unix() <= row.FileMtime && info.Size() == row.FileSize && !isStubRow(row) {
		return row
	}
	if ScanAndUpsertFile(row.FullPath) == nil {
		return row
	}
	if refreshed, err := db.GetConvIndex(convID); err == nil && refreshed != nil {
		return refreshed
	}
	return row
}

// ScanAndUpsertFile scans a single .jsonl conversation file and upserts the
// result into the DB cache. The project dir is derived from the file's parent
// directory. Returns the resulting SessionEntry, or nil if the file has no
// useful data or was deleted.
func ScanAndUpsertFile(filePath string) *SessionEntry {
	convID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	if len(convID) != 36 {
		return nil
	}
	projectDir := filepath.Dir(filePath)

	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			_ = db.DeleteConvIndex(convID)
			_ = db.DeleteConvBranchHistory(convID)
		}
		return nil
	}

	scanned, scanComplete := parseJSONLSession(filePath, convID)
	if scanned == nil {
		stub := &db.ConvIndexRow{
			ConvID:     convID,
			ProjectDir: projectDir,
			FullPath:   filePath,
			FileMtime:  info.ModTime().Unix(),
			FileSize:   info.Size(),
			IndexedAt:  time.Now(),
		}
		_ = db.UpsertConvIndex(stub)
		// Shed any stale scan rows from a prior non-stub scan — see the
		// matching empty rebuild in LoadSessionsIndexWithOptions. Only
		// when the scan reached EOF (a truncated scan proves nothing).
		if scanComplete {
			_ = db.RebuildConvBranchHistoryScan(convID, nil)
		}
		return nil
	}
	scanned.FileSize = info.Size()

	row := entryToDBRow(scanned, projectDir)
	convIndexErr := db.UpsertConvIndex(row)
	// Rebuild branch history from the same scan — see the matching
	// call in LoadSessionsIndexWithOptions. Gated on the conv_index
	// upsert succeeding (history rows are reclaimed by a sweep over
	// conv_index, so one for an unindexed conv would strand) and on a
	// complete scan (a truncated branch set is partial).
	if convIndexErr == nil && scanComplete {
		_ = db.RebuildConvBranchHistoryScan(convID, scanned.BranchHistory)
	}
	return scanned
}

// extractMessageContent extracts text content from a message.
// Content can be a string or an array of content blocks.
func extractMessageContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		// Array of content blocks - look for text type
		for _, block := range v {
			if m, ok := block.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					return text
				}
			}
		}
	}
	return ""
}

// The legacy `sessions-index.json` file is no longer the source-of-truth
// for tclaude — the SQLite `conv_index` table is. We never read from it
// for our own logic. We DO keep it consistent on conv mutations
// (cp/mv/delete/prune) for any external tooling (Claude Code itself
// included) that may still consult it.
//
// The helpers below perform SURGICAL updates that preserve any
// top-level fields and per-entry fields we don't recognise — important
// forward-compat for future tclaude versions or anything else that
// writes the file. Never rewrite the whole file from scratch.
//
// If the file doesn't exist they no-op (we never create it; we only
// maintain it).

// sessionIDProbe is the minimal shape we deserialize a raw entry into
// when we just need its conv-id for filtering.
type sessionIDProbe struct {
	SessionID string `json:"sessionId"`
}

func readRawSessionsIndex(projectPath string) (top map[string]json.RawMessage, entries []json.RawMessage, exists bool, err error) {
	path := filepath.Join(projectPath, "sessions-index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, true, fmt.Errorf("parse sessions-index.json: %w", err)
	}
	if raw, ok := top["entries"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, true, fmt.Errorf("parse sessions-index.json entries: %w", err)
		}
	}
	return top, entries, true, nil
}

func writeRawSessionsIndex(projectPath string, top map[string]json.RawMessage, entries []json.RawMessage) error {
	entriesRaw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	top["entries"] = entriesRaw
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(projectPath, "sessions-index.json"), out, 0600)
}

func rawEntrySessionID(raw json.RawMessage) string {
	var p sessionIDProbe
	_ = json.Unmarshal(raw, &p)
	return p.SessionID
}

// RemoveSessionsIndexEntry surgically removes an entry by sessionID from
// the legacy sessions-index.json file in projectPath. Other entries and
// any unknown top-level / per-entry fields are preserved verbatim.
// No-op when the file doesn't exist or the entry isn't there.
func RemoveSessionsIndexEntry(projectPath, sessionID string) error {
	top, entries, exists, err := readRawSessionsIndex(projectPath)
	if err != nil || !exists {
		return err
	}
	filtered := entries[:0]
	changed := false
	for _, raw := range entries {
		if rawEntrySessionID(raw) == sessionID {
			changed = true
			continue
		}
		filtered = append(filtered, raw)
	}
	if !changed {
		return nil
	}
	return writeRawSessionsIndex(projectPath, top, filtered)
}

// UpsertSessionsIndexEntry surgically inserts or replaces an entry in
// the legacy sessions-index.json file. Other entries and any unknown
// top-level fields are preserved verbatim. No-op when the file doesn't
// exist — we never create it; we only maintain it if external tooling
// already wrote it.
func UpsertSessionsIndexEntry(projectPath string, entry SessionEntry) error {
	top, entries, exists, err := readRawSessionsIndex(projectPath)
	if err != nil || !exists {
		return err
	}
	newRaw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	replaced := false
	for i, raw := range entries {
		if rawEntrySessionID(raw) == entry.SessionID {
			entries[i] = newRaw
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, newRaw)
	}
	return writeRawSessionsIndex(projectPath, top, entries)
}

// FindSessionByID finds a session entry by its ID (full or prefix)
func FindSessionByID(index *SessionsIndex, sessionID string) (*SessionEntry, int) {
	// First try exact match
	for i, entry := range index.Entries {
		if entry.SessionID == sessionID {
			return &index.Entries[i], i
		}
	}
	// Then try prefix match
	var matches []int
	for i, entry := range index.Entries {
		if strings.HasPrefix(entry.SessionID, sessionID) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return &index.Entries[matches[0]], matches[0]
	}
	return nil, -1
}

// RemoveSessionByID removes a session from the index by its ID
func RemoveSessionByID(index *SessionsIndex, sessionID string) bool {
	for i, entry := range index.Entries {
		if entry.SessionID == sessionID {
			index.Entries = append(index.Entries[:i], index.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// CopyConversationFile copies a conversation file and updates sessionId references
func CopyConversationFile(src, dst, oldID, newID string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	content := strings.ReplaceAll(string(data), oldID, newID)
	return os.WriteFile(dst, []byte(content), 0600)
}

// CopyDir recursively copies a directory
func CopyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := CopyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := CopyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// CopyFile copies a single file
func CopyFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}

// GenerateUUID generates a new UUID v4
func GenerateUUID() string {
	return uuid.New().String()
}

// FormatTime returns current time in RFC3339 format (local time)
func FormatTime() string {
	return time.Now().Format(time.RFC3339)
}

// CopyConversationResult contains the result of copying a conversation
type CopyConversationResult struct {
	NewConvID      string
	DstProjectPath string
}

// CopyConversationToPath copies a conversation to a new project path
func CopyConversationToPath(convID, destPath string, global bool) (*CopyConversationResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	var srcEntry *SessionEntry
	var srcProjectPath string
	dstProjectPath := GetClaudeProjectPath(destPath)

	if global {
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			return nil, err
		}

		for _, dirEntry := range entries {
			if !dirEntry.IsDir() {
				continue
			}
			projPath := projectsDir + "/" + dirEntry.Name()
			index, err := LoadSessionsIndex(projPath)
			if err != nil {
				continue
			}
			if found, _ := FindSessionByID(index, convID); found != nil {
				srcEntry = found
				srcProjectPath = projPath
				break
			}
		}
	} else {
		srcProjectPath = GetClaudeProjectPath(cwd)
		srcIndex, err := LoadSessionsIndex(srcProjectPath)
		if err != nil {
			return nil, err
		}
		srcEntry, _ = FindSessionByID(srcIndex, convID)
	}

	if srcEntry == nil {
		return nil, os.ErrNotExist
	}

	// Create destination directory if needed
	if err := os.MkdirAll(dstProjectPath, 0700); err != nil {
		return nil, err
	}

	// Generate new UUID
	newConvID := GenerateUUID()
	oldConvID := srcEntry.SessionID

	// Copy conversation file
	srcConvFile := filepath.Join(srcProjectPath, oldConvID+".jsonl")
	dstConvFile := filepath.Join(dstProjectPath, newConvID+".jsonl")

	if err := CopyConversationFile(srcConvFile, dstConvFile, oldConvID, newConvID); err != nil {
		return nil, err
	}

	// Copy conversation directory if exists
	srcConvDir := filepath.Join(srcProjectPath, oldConvID)
	dstConvDir := filepath.Join(dstProjectPath, newConvID)
	if info, err := os.Stat(srcConvDir); err == nil && info.IsDir() {
		if err := CopyDir(srcConvDir, dstConvDir); err != nil {
			return nil, err
		}
	}

	// Keep the legacy sessions-index.json in sync for external tooling
	// — surgical upsert preserves any unknown fields.
	dstInfo, err := os.Stat(dstConvFile)
	if err != nil {
		return nil, err
	}
	now := FormatTime()
	newEntry := SessionEntry{
		SessionID:        newConvID,
		FullPath:         dstConvFile,
		FileMtime:        dstInfo.ModTime().UnixMilli(),
		FirstPrompt:      srcEntry.FirstPrompt,
		Summary:          srcEntry.Summary,
		CustomTitle:      srcEntry.CustomTitle,
		MessageCount:     srcEntry.MessageCount,
		Created:          now,
		Modified:         now,
		GitBranch:        srcEntry.GitBranch,
		GitBranchStartup: srcEntry.GitBranchStartup,
		ProjectPath:      destPath,
		IsSidechain:      srcEntry.IsSidechain,
	}
	if err := UpsertSessionsIndexEntry(dstProjectPath, newEntry); err != nil {
		return nil, err
	}

	return &CopyConversationResult{
		NewConvID:      newConvID,
		DstProjectPath: dstProjectPath,
	}, nil
}
