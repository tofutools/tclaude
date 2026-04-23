package session

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type PruneParams struct {
	MaxAge string `long:"max-age" help:"Max age for exited sessions (e.g., 24h, 7d, 1w)" default:"0s"`
	DryRun bool   `long:"dry-run" help:"Show what would be deleted without deleting"`
	All    bool   `short:"a" long:"all" help:"Delete all exited sessions regardless of age"`
}

func PruneCmd() *cobra.Command {
	return boa.CmdT[PruneParams]{
		Use:         "prune",
		Short:       "Remove old exited session states",
		Long:        "Remove session state files for sessions that have exited. By default removes all exited sessions, or use --max-age to only remove sessions older than a threshold.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *PruneParams, cmd *cobra.Command, args []string) {
			if err := runPrune(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runPrune(params *PruneParams) error {
	var maxAge time.Duration

	if params.All {
		// Delete all exited sessions
		maxAge = 0
	} else if params.MaxAge != "0s" {
		// Parse custom max age
		parsed, err := parseDuration(params.MaxAge)
		if err != nil {
			return fmt.Errorf("invalid max-age: %w", err)
		}
		maxAge = parsed
	} else {
		// Default: delete all exited sessions
		maxAge = 0
	}

	states, err := ListSessionStates()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	var toDelete []*SessionState

	for _, state := range states {
		RefreshSessionStatus(state)
		if state.Status != StatusExited {
			continue
		}
		if maxAge == 0 || state.Updated.Before(cutoff) {
			toDelete = append(toDelete, state)
		}
	}

	if len(toDelete) == 0 {
		fmt.Println("No exited sessions to prune")
		return nil
	}

	if params.DryRun {
		fmt.Printf("Would delete %d exited session(s):\n", len(toDelete))
		for _, state := range toDelete {
			age := FormatDuration(time.Since(state.Updated))
			fmt.Printf("  %s  (exited %s ago)\n", state.ID, age)
		}
		return nil
	}

	deleted := 0
	for _, state := range toDelete {
		if err := DeleteSessionState(state.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to delete %s: %v\n", state.ID, err)
			continue
		}
		deleted++
	}

	fmt.Printf("Pruned %d exited session(s)\n", deleted)
	return nil
}

// pruneExitedSessionsSilent removes all exited session state files silently
// Used for automatic cleanup when exiting the session viewer
func pruneExitedSessionsSilent() {
	states, err := ListSessionStates()
	if err != nil {
		return
	}

	for _, state := range states {
		RefreshSessionStatus(state)
		if state.Status == StatusExited {
			_ = DeleteSessionState(state.ID)
		}
	}
}

// parseDuration parses a duration string with support for days (d) and weeks (w)
func parseDuration(s string) (time.Duration, error) {
	// Handle weeks and days which aren't supported by time.ParseDuration
	if len(s) > 1 {
		suffix := s[len(s)-1]
		prefix := s[:len(s)-1]

		switch suffix {
		case 'w', 'W':
			var weeks int
			if _, err := fmt.Sscanf(prefix, "%d", &weeks); err == nil {
				return time.Duration(weeks) * 7 * 24 * time.Hour, nil
			}
		case 'd', 'D':
			var days int
			if _, err := fmt.Sscanf(prefix, "%d", &days); err == nil {
				return time.Duration(days) * 24 * time.Hour, nil
			}
		}
	}

	// Fall back to standard parsing
	return time.ParseDuration(s)
}
