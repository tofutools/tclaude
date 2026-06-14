package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// harnessUsesSlashContextControls gates the stopped-hook path's context
// nudge (the hint naming /reincarnate) on the harness understanding context
// controls, folding to its compact capability. Claude Code and Codex both
// expose /compact; an empty/unknown harness falls back to the legacy CC
// behaviour so the common path is never accidentally muted.
func TestHarnessUsesSlashContextControls(t *testing.T) {
	cases := []struct {
		harness string
		want    bool
	}{
		{"", true},                         // untagged ⇒ legacy CC behaviour
		{"claude", true},                   // CC has /compact
		{"codex", true},                    // Codex has /compact
		{"definitely-not-a-harness", true}, // unknown ⇒ safe CC default
	}
	for _, c := range cases {
		assert.Equal(t, c.want, harnessUsesSlashContextControls(c.harness),
			"harnessUsesSlashContextControls(%q)", c.harness)
	}
}
