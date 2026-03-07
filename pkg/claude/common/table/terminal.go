package table

import (
	"os"

	"golang.org/x/term"
)

const (
	// DefaultTerminalWidth is used when terminal width cannot be determined
	DefaultTerminalWidth = 80
)

// GetTerminalWidth returns the current terminal width.
// Falls back to DefaultTerminalWidth if width cannot be determined.
func GetTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return DefaultTerminalWidth
	}
	return width
}
