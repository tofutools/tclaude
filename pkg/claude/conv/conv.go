package conv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

// Type aliases - use types from convops to avoid duplication
type SessionsIndex = convops.SessionsIndex
type SessionEntry = convops.SessionEntry

// Re-export functions from convops for backward compatibility
var (
	ClaudeProjectsDir   = convops.ClaudeProjectsDir
	PathToProjectDir    = convops.PathToProjectDir
	GetClaudeProjectPath = convops.GetClaudeProjectPath
	SaveSessionsIndex   = convops.SaveSessionsIndex
	FindSessionByID     = convops.FindSessionByID
	RemoveSessionByID   = convops.RemoveSessionByID
	CopyDir             = convops.CopyDir
	CopyFile            = convops.CopyFile
	CopyConversationFile = convops.CopyConversationFile
)

type ConvParams struct {
	Global bool `short:"g" help:"List conversations from all projects"`
}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[ConvParams]{
		Use:         "conv",
		Short:       "Manage Claude Code conversations",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			ListCmd(),
			SearchCmd(),
			IndexEmbeddingsCmd(),
			SearchEmbeddingsCmd(),
			ResumeCmd(),
			CpCmd(),
			MvCmd(),
			DeleteCmd(),
			PruneEmptyCmd(),
		},
		RunFunc: func(params *ConvParams, _ *cobra.Command, _ []string) {
			// Default to interactive watch mode (same as conv ls -w)
			if err := RunConvWatchMode(params.Global, "", ""); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Aliases = []string{"convs", "conversation", "conversations"}
	return cmd
}

// DebugLog controls whether LoadSessionsIndex prints debug timing information
var DebugLog = false

// LoadSessionsIndexOptions configures LoadSessionsIndex behavior
type LoadSessionsIndexOptions struct {
	// ForceRescan forces a full rescan of all entries regardless of mtime
	ForceRescan bool
}

// LoadSessionsIndex loads conversations for a Claude project directory.
// Uses our SQLite conv_index as cache, scanning .jsonl files only when their mtime has changed.
func LoadSessionsIndex(projectPath string) (*SessionsIndex, error) {
	return LoadSessionsIndexWithOptions(projectPath, LoadSessionsIndexOptions{})
}

// LoadSessionsIndexWithOptions loads conversations with configurable behavior.
// Flow: list .jsonl files -> check DB mtime -> parse only if stale/new -> return entries from DB.
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

		// Check if DB entry is fresh
		if cached, ok := dbByID[convID]; ok && !opts.ForceRescan && cached.FileMtime >= fileMtime {
			// DB is fresh, use cached data
			entries = append(entries, dbRowToEntry(cached, fileSize))
			continue
		}

		// Need to scan the file
		scannedCount++
		slog.Info("conv_index: scanning file",
			"conv_id", convID[:8],
			"project", filepath.Base(projectPath),
			"reason", scanReason(dbByID[convID], opts.ForceRescan))

		scanned := parseJSONLSession(filePath, convID)
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
			continue
		}
		scanned.FileSize = fileSize

		// Upsert into DB
		row := entryToDBRow(scanned, projectPath)
		if err := db.UpsertConvIndex(row); err != nil {
			slog.Warn("conv_index: db upsert failed", "conv_id", convID[:8], "error", err)
		}

		entries = append(entries, *scanned)
	}

	// 4. Remove DB entries for files that no longer exist on disk
	for convID := range dbByID {
		if !seenIDs[convID] {
			if err := db.DeleteConvIndex(convID); err != nil {
				slog.Warn("conv_index: db delete failed", "conv_id", convID[:8], "error", err)
			}
		}
	}

	if DebugLog {
		fmt.Fprintf(os.Stderr, "[DEBUG] LoadSessionsIndex %s: total=%d scanned=%d cached=%d elapsed=%v\n",
			filepath.Base(projectPath), len(entries), scannedCount, len(entries)-scannedCount, time.Since(start))
	}

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
	return SessionEntry{
		SessionID:    r.ConvID,
		FullPath:     r.FullPath,
		FileMtime:    r.FileMtime,
		FirstPrompt:  r.FirstPrompt,
		Summary:      r.Summary,
		CustomTitle:  r.CustomTitle,
		MessageCount: r.MessageCount,
		Created:      r.Created,
		Modified:     r.Modified,
		GitBranch:    r.GitBranch,
		ProjectPath:  r.ProjectPath,
		IsSidechain:  r.IsSidechain,
		FileSize:     fileSize,
	}
}

// entryToDBRow converts a SessionEntry to a DB row for storage.
func entryToDBRow(e *SessionEntry, projectDir string) *db.ConvIndexRow {
	return &db.ConvIndexRow{
		ConvID:       e.SessionID,
		ProjectDir:   projectDir,
		FullPath:     e.FullPath,
		FileMtime:    e.FileMtime,
		FileSize:     e.FileSize,
		FirstPrompt:  e.FirstPrompt,
		Summary:      e.Summary,
		CustomTitle:  e.CustomTitle,
		MessageCount: e.MessageCount,
		Created:      e.Created,
		Modified:     e.Modified,
		GitBranch:    e.GitBranch,
		ProjectPath:  e.ProjectPath,
		IsSidechain:  e.IsSidechain,
		IndexedAt:    time.Now(),
	}
}

// jsonlMessage represents a line in the .jsonl conversation file
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
	return parseJSONLSession(filePath, sessionID)
}

// parseJSONLSession parses a .jsonl file and extracts session metadata.
// Always does a full scan: reads forward for prompt/title/summary, then
// tail-scans if title data wasn't found in the forward pass.
func parseJSONLSession(filePath, sessionID string) *SessionEntry {
	file, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil
	}

	var entry SessionEntry
	entry.SessionID = sessionID
	entry.FullPath = filePath
	entry.FileMtime = info.ModTime().Unix()
	entry.FileSize = info.Size()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var firstTimestamp string

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

		// Capture project path and git branch from first message that has them
		if entry.ProjectPath == "" && msg.Cwd != "" {
			entry.ProjectPath = msg.Cwd
		}
		if entry.GitBranch == "" && msg.GitBranch != "" {
			entry.GitBranch = msg.GitBranch
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

		// Stop early only if we have ALL the fields we care about
		if entry.CustomTitle != "" && entry.Summary != "" && entry.FirstPrompt != "" && entry.ProjectPath != "" {
			break
		}
	}

	if firstTimestamp == "" {
		// No valid data found
		return nil
	}

	entry.Created = firstTimestamp
	entry.Modified = info.ModTime().UTC().Format(time.RFC3339)
	entry.MessageCount = 0

	return &entry
}

// isSystemInjectedMessage returns true if the text is a system-injected message
// from Claude Code (not actual user input). These start with known system XML tags.
func isSystemInjectedMessage(text string) bool {
	if !strings.HasPrefix(text, "<") {
		return false
	}
	for _, tag := range convindex.SystemTags {
		if strings.HasPrefix(text, "<"+tag+">") {
			return true
		}
	}
	return false
}

// extractMessageContent extracts text content from a message
// Content can be a string or an array of content blocks
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

// LoadEntriesFromDB loads conversation entries directly from the SQLite cache
// without touching the filesystem. Used by watch mode for fast refreshes.
// If projectPath is empty, loads all entries across all projects (global mode).
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
		entries = append(entries, dbRowToEntry(r, r.FileSize))
	}
	return entries, nil
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
		}
		return nil
	}

	scanned := parseJSONLSession(filePath, convID)
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
		return nil
	}
	scanned.FileSize = info.Size()

	row := entryToDBRow(scanned, projectDir)
	_ = db.UpsertConvIndex(row)
	return scanned
}

// ListSessions returns all sessions from a project directory
func ListSessions(projectPath string) ([]SessionEntry, error) {
	index, err := LoadSessionsIndex(projectPath)
	if err != nil {
		return nil, err
	}
	return index.Entries, nil
}

// ParseTimeParam parses a time parameter string into a time.Time
// Supports formats: "2024-01-15", "2024-01-15T10:30", "24h", "7d", "2w", or any time.Duration
func ParseTimeParam(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}

	// Try standard time.Duration first (e.g., "1h30m", "2h45m30s")
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}

	// Try extended duration with days/weeks (e.g., "7d", "2w")
	if len(s) >= 2 {
		unit := s[len(s)-1]
		numStr := s[:len(s)-1]
		if num, err := strconv.Atoi(numStr); err == nil {
			var duration time.Duration
			switch unit {
			case 'd':
				duration = time.Duration(num) * 24 * time.Hour
			case 'w':
				duration = time.Duration(num) * 7 * 24 * time.Hour
			}
			if duration > 0 {
				return time.Now().Add(-duration), nil
			}
		}
	}

	// Try various date/time formats
	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
		"2006/01/02",
		"01-02-2006",
		"01/02/2006",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse time: %s (try formats like 2024-01-15, 1h30m, 7d)", s)
}

// FilterEntriesByTime filters session entries by time range
func FilterEntriesByTime(entries []SessionEntry, since, before string) ([]SessionEntry, error) {
	sinceTime, err := ParseTimeParam(since)
	if err != nil {
		return nil, fmt.Errorf("invalid --since value: %w", err)
	}

	beforeTime, err := ParseTimeParam(before)
	if err != nil {
		return nil, fmt.Errorf("invalid --before value: %w", err)
	}

	if sinceTime.IsZero() && beforeTime.IsZero() {
		return entries, nil
	}

	var filtered []SessionEntry
	for _, e := range entries {
		modTime, err := time.Parse(time.RFC3339, e.Modified)
		if err != nil {
			continue
		}

		if !sinceTime.IsZero() && modTime.Before(sinceTime) {
			continue
		}
		if !beforeTime.IsZero() && modTime.After(beforeTime) {
			continue
		}
		filtered = append(filtered, e)
	}

	return filtered, nil
}
