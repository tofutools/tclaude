package conv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
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
			AISearchCmd(),
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
	// SkipUnindexedScan skips scanning for unindexed .jsonl files (faster)
	SkipUnindexedScan bool
	// SkipMissingDataRescan skips re-scanning entries with missing display data (faster)
	SkipMissingDataRescan bool
}

// LoadSessionsIndex loads the sessions index from a Claude project directory
// It also scans for unindexed .jsonl files and merges them, deduplicating by sessionId
// Additionally, it re-scans entries with missing display data (no prompt, summary, or title)
func LoadSessionsIndex(projectPath string) (*SessionsIndex, error) {
	return LoadSessionsIndexWithOptions(projectPath, LoadSessionsIndexOptions{})
}

// LoadSessionsIndexWithOptions loads the sessions index with configurable behavior
func LoadSessionsIndexWithOptions(projectPath string, opts LoadSessionsIndexOptions) (*SessionsIndex, error) {
	start := time.Now()
	indexPath := filepath.Join(projectPath, "sessions-index.json")
	data, err := os.ReadFile(indexPath)

	var index SessionsIndex
	if err != nil {
		if os.IsNotExist(err) {
			index = SessionsIndex{Version: 1, Entries: []SessionEntry{}}
		} else {
			return nil, fmt.Errorf("failed to read sessions index: %w", err)
		}
	} else {
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("failed to parse sessions index: %w", err)
		}
	}
	readDur := time.Since(start)

	// Re-scan entries with missing display data
	rescanStart := time.Now()
	rescanCount := 0
	if !opts.SkipMissingDataRescan {
		for i := range index.Entries {
			if index.Entries[i].DisplayTitle() == "" {
				rescanCount++
				// Try to get data from the file
				filePath := filepath.Join(projectPath, index.Entries[i].SessionID+".jsonl")
				if scanned := parseJSONLSession(filePath, index.Entries[i].SessionID); scanned != nil {
					// Update missing fields from scanned data
					if scanned.Summary != "" && index.Entries[i].Summary == "" {
						index.Entries[i].Summary = scanned.Summary
					}
					if scanned.FirstPrompt != "" && index.Entries[i].FirstPrompt == "" {
						index.Entries[i].FirstPrompt = scanned.FirstPrompt
					}
					if scanned.ProjectPath != "" && index.Entries[i].ProjectPath == "" {
						index.Entries[i].ProjectPath = scanned.ProjectPath
					}
					if scanned.GitBranch != "" && index.Entries[i].GitBranch == "" {
						index.Entries[i].GitBranch = scanned.GitBranch
					}
				}
			}
		}
	}
	rescanDur := time.Since(rescanStart)

	// Scan for unindexed .jsonl files and merge them
	scanStart := time.Now()
	var unindexed []SessionEntry
	if !opts.SkipUnindexedScan {
		// Build set of already indexed session IDs to avoid redundant parsing
		indexedIDs := make(map[string]bool)
		for _, e := range index.Entries {
			indexedIDs[e.SessionID] = true
		}
		unindexed = scanUnindexedSessionsExcluding(projectPath, indexedIDs)
		if len(unindexed) > 0 {
			index.Entries = append(index.Entries, unindexed...)
		}
	}
	scanDur := time.Since(scanStart)

	if DebugLog {
		fmt.Fprintf(os.Stderr, "[DEBUG] LoadSessionsIndex %s: read=%v rescan=%d/%d(%v) unindexed=%d(%v)\n",
			filepath.Base(projectPath), readDur, rescanCount, len(index.Entries), rescanDur, len(unindexed), scanDur)
	}

	return &index, nil
}

// scanUnindexedSessionsExcluding scans for .jsonl files, skipping those in the exclude set
// This avoids expensive parsing of already-indexed sessions
func scanUnindexedSessionsExcluding(projectPath string, exclude map[string]bool) []SessionEntry {
	var entries []SessionEntry

	files, err := os.ReadDir(projectPath)
	if err != nil {
		return entries
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}

		// Extract session ID from filename (e.g., "0789725a-bc71-47dd-9ca5-1b4fe7aead9b.jsonl")
		sessionID := strings.TrimSuffix(file.Name(), ".jsonl")
		if len(sessionID) != 36 { // UUID length
			continue
		}

		// Skip if already indexed
		if exclude != nil && exclude[sessionID] {
			continue
		}

		filePath := filepath.Join(projectPath, file.Name())
		entry := parseJSONLSession(filePath, sessionID)
		if entry != nil {
			entries = append(entries, *entry)
		}
	}

	return entries
}

// jsonlMessage represents a line in the .jsonl conversation file
type jsonlMessage struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Summary   string `json:"summary"` // For type="summary" messages
	Message   struct {
		Role    string `json:"role"`
		Content any    `json:"content"` // Can be string or array
	} `json:"message"`
}

// parseJSONLSession parses a .jsonl file and extracts session metadata
// Stops early once it finds display data (firstPrompt or summary)
// to avoid reading entire large conversation files into memory
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

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var firstTimestamp string

	// Scan file looking for display data (firstPrompt or summary)
	// Stop as soon as we have something to show - don't read the whole file
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

		// Capture summaries if we see one (and stop - we have display data)
		if msg.Type == "summary" && msg.Summary != "" {
			entry.Summary = msg.Summary
			break
		}

		// Capture first user message with actual text content as the prompt
		if entry.FirstPrompt == "" && msg.Type == "user" && msg.Message.Role == "user" {
			text := extractMessageContent(msg.Message.Content)
			// Skip messages without text (e.g., tool_result blocks from resumed sessions)
			// Also skip system-generated messages like "[Request interrupted by user...]"
			if text != "" && !strings.HasPrefix(text, "[Request interrupted") {
				entry.FirstPrompt = text
				if msg.Timestamp != "" {
					firstTimestamp = msg.Timestamp
				}
				break // We have display data, stop reading
			}
		}
	}

	if firstTimestamp == "" {
		// No valid data found
		return nil
	}

	entry.Created = firstTimestamp
	// Use file mtime for Modified since we're not reading the whole file
	entry.Modified = info.ModTime().UTC().Format(time.RFC3339)
	entry.MessageCount = 0 // Unknown for unindexed sessions

	return &entry
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
