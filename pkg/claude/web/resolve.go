package web

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// resolveSession finds the tmux session to attach to.
// If sessionID is provided, uses that. Otherwise auto-detects if only one running.
// Returns (tmuxSessionName, sessionID, error).
func resolveSession(sessionID string) (string, string, error) {
	if err := session.CheckTmuxInstalled(); err != nil {
		return "", "", err
	}

	states, err := session.ListSessionStates()
	if err != nil {
		return "", "", fmt.Errorf("failed to list sessions: %w", err)
	}

	// Filter to alive sessions
	var alive []*session.SessionState
	for _, s := range states {
		session.RefreshSessionStatus(s)
		if s.Status != session.StatusExited {
			alive = append(alive, s)
		}
	}

	if len(alive) == 0 {
		return "", "", fmt.Errorf("no running sessions found. Start one with: tclaude")
	}

	if sessionID != "" {
		// Find by exact or prefix match
		for _, s := range alive {
			if s.ID == sessionID || strings.HasPrefix(s.ID, sessionID) {
				return s.TmuxSession, s.ID, nil
			}
		}
		return "", "", fmt.Errorf("session %s not found among running sessions", sessionID)
	}

	// Auto-detect
	if len(alive) == 1 {
		return alive[0].TmuxSession, alive[0].ID, nil
	}

	// Multiple sessions - list them
	fmt.Println("Multiple running sessions found. Specify one:")
	for _, s := range alive {
		fmt.Printf("  %s  %s  %s\n", s.ID, s.Status, s.Cwd)
	}
	return "", "", fmt.Errorf("specify a session ID: tclaude web <session-id>")
}
