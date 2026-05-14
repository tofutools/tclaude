package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Ensures the agent package's init() hooks runJoinGroup into session,
// so `tclaude --join-group …` is reachable from runNew without a
// session→agent import cycle.
func TestJoinGroupHandlerWired(t *testing.T) {
	require.NotNil(t, session.JoinGroupHandler, "session.JoinGroupHandler is nil; agent package init() did not run")
}

// Mutually-exclusive flags should fail before any daemon contact.
// Covers the two "we own the spawn label" guarantees: --resume conflicts
// (we always create a fresh conv, never resume), --label conflicts
// (the daemon picks `spwn-XXXXXX`).
func TestJoinGroupRejectsConflictingFlags(t *testing.T) {
	cases := []struct {
		name   string
		params session.NewParams
		want   string
	}{
		{
			name:   "resume incompatible",
			params: session.NewParams{JoinGroup: "team", Resume: "abc123"},
			want:   "--resume",
		},
		{
			name:   "label incompatible",
			params: session.NewParams{JoinGroup: "team", Label: "my-label"},
			want:   "--label",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RunJoinGroup(&tc.params)
			require.Error(t, err, "expected error, got nil")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
