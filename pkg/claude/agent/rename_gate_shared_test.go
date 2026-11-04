package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/renametitle/renametitletest"
)

// The CLI's local rename pre-check agrees with the shared table.
func TestRenameGate_MatchesSharedTable(t *testing.T) {
	for _, c := range renametitletest.Cases {
		assert.Equal(t, c.Title, isValidRenameTitle(c.In), "isValidRenameTitle %s (%q)", c.Name, c.In)
	}
}
