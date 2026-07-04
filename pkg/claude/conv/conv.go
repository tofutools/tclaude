package conv

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/common"
)

// Type aliases - use types from convops to avoid duplication
type SessionsIndex = convops.SessionsIndex
type SessionEntry = convops.SessionEntry
type LoadSessionsIndexOptions = convops.LoadSessionsIndexOptions

// Re-export functions from convops for backward compatibility. convops
// is the single source-of-truth implementation for all conv-data ops
// (parsing, SQLite-cached lookups, file I/O).
var (
	ClaudeProjectsDir            = convops.ClaudeProjectsDir
	PathToProjectDir             = convops.PathToProjectDir
	GetClaudeProjectPath         = convops.GetClaudeProjectPath
	RemoveSessionsIndexEntry     = convops.RemoveSessionsIndexEntry
	UpsertSessionsIndexEntry     = convops.UpsertSessionsIndexEntry
	FindSessionByID              = convops.FindSessionByID
	RemoveSessionByID            = convops.RemoveSessionByID
	CopyDir                      = convops.CopyDir
	CopyFile                     = convops.CopyFile
	CopyConversationFile         = convops.CopyConversationFile
	LoadSessionsIndex            = convops.LoadSessionsIndex
	LoadSessionsIndexWithOptions = convops.LoadSessionsIndexWithOptions
	LoadEntriesFromDB            = convops.LoadEntriesFromDB
	RefreshConvIndexEntry        = convops.RefreshConvIndexEntry
	ScanAndUpsertFile            = convops.ScanAndUpsertFile
	ParseJSONLSessionPublic      = convops.ParseJSONLSessionPublic
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
			WatchCmd(),
			SearchCmd(),
			IndexEmbeddingsCmd(),
			SearchEmbeddingsCmd(),
			ResumeCmd(),
			CpCmd(),
			MvCmd(),
			DeleteCmd(),
			ArchiveCmd(),
			UnarchiveCmd(),
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

// DebugLog is a back-compat pointer alias to convops.DebugLog. Callers
// who used to do `conv.DebugLog = true` should switch to
// `convops.DebugLog = true`; this helper preserves the existing surface
// for one release.
func SetDebugLog(v bool) { convops.DebugLog = v }


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
