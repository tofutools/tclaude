package paneinput

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// ErrAwaitingHumanInput means nothing may be typed or submitted until the
// human closes the active permission/question dialog. Async callers retry;
// synchronous callers return this refusal to the user.
var ErrAwaitingHumanInput = errors.New("pane is awaiting human input; close the permission or question dialog and retry")

// IsAwaitingHumanInput reports the two modal states that own the keyboard.
func IsAwaitingHumanInput(status string) bool {
	return status == "awaiting_permission" || status == "awaiting_input"
}

// CheckHumanInput reads the current projection, rather than a session row
// captured before waiting for a pane lock. lockTarget is session-shaped even
// when the actual send uses a pane ID.
func CheckHumanInput(lockTarget string) error {
	// Legacy, unmanaged task panes may have no tclaude database at all.
	if _, err := os.Stat(db.DBPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read pane input state: %w", err)
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(lockTarget, "="), ":")
	status, err := db.LatestSessionStatusForTmux(name)
	if err != nil {
		return fmt.Errorf("read pane input state: %w", err)
	}
	if IsAwaitingHumanInput(status) {
		return ErrAwaitingHumanInput
	}
	return nil
}

// ScreenAwaitsHumanInput recognizes Claude's permission UI before its async
// hook arrives. Both the question and modal footer are required; ordinary
// model text mentioning permissions alone must not hold a pane.
func ScreenAwaitsHumanInput(screen string) bool {
	screen = strings.ToLower(screen)
	return strings.Contains(screen, "do you want to proceed?") && strings.Contains(screen, "esc to cancel")
}
