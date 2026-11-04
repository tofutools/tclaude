package renametitle

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/renametitle/renametitletest"
)

func TestGates_MatchSharedTable(t *testing.T) {
	for _, c := range renametitletest.Cases {
		assert.Equal(t, c.Title, Valid(c.In), "Valid %s (%q)", c.Name, c.In)
		assert.Equal(t, c.Sink, ValidSink(c.In), "ValidSink %s (%q)", c.Name, c.In)
	}
}
