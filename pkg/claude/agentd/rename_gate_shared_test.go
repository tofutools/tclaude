package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/renametitle/renametitletest"
)

// The daemon's rename gates agree with the shared table, which the hook
// callback's and the CLI's gates are pinned to as well.
func TestRenameGates_MatchSharedTable(t *testing.T) {
	for _, c := range renametitletest.Cases {
		assert.Equal(t, c.Title, isValidRenameTitle(c.In), "isValidRenameTitle %s (%q)", c.Name, c.In)
		assert.Equal(t, c.Sink, isValidRenameSink(c.In), "isValidRenameSink %s (%q)", c.Name, c.In)
	}
}
