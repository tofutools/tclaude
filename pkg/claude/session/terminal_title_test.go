package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// attach sets the terminal title with an OSC sequence of its own, around a
// title an agent can set by renaming itself. A BEL or ESC in the title would
// end the sequence early and hand the rest to the terminal (tcl-7gm2ec).
func TestTerminalTitleCannotEscapeItsSequence(t *testing.T) {
	got := termTitle("fix\x07\x1b[2J\x1b]52;c;cm0=\x07it\u202egpj\nexe")
	for _, bad := range []string{"\x07", "\x1b", "\u202e", "\n"} {
		assert.NotContains(t, got, bad)
	}
	assert.Equal(t, "fix[2J]52;c;cm0=itgpj exe", got)
}
