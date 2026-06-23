package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario: a human (or the dashboard / CLI) spawns an agent with a name
// that isn't a safe token — a space, punctuation, or unicode. The daemon
// rejects it at the spawn boundary with a 400 instead of silently
// dropping the name downstream (the old behaviour: executeSpawn only
// applied a name that cleared isValidRenameTitle, so a bad name just
// produced an unnamed agent with no feedback). The name doubles as a git
// worktree branch name and becomes the conversation title, so its charset
// is restricted to [A-Za-z0-9_-].
func TestSpawn_InvalidNameRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	bad := map[string]string{
		"space":    "code reviewer",
		"slash":    "code/reviewer",
		"brackets": "[reviewer]",
		"dot":      "code.reviewer",
		"unicode":  "café",
		"over 64":  "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789ABCD",
	}
	for label, name := range bad {
		t.Run(label, func(t *testing.T) {
			resp := f.AsHuman().SpawnWith("alpha", map[string]any{"name": name})
			assert.Equalf(t, http.StatusBadRequest, resp.Code,
				"spawn with name %q should 400; body=%s", name, resp.Raw)
			assert.Containsf(t, string(resp.Raw), "A-Za-z0-9_-",
				"the error should name the allowed charset; got %s", resp.Raw)
		})
	}
}

// Scenario: a valid name spawns cleanly and is applied as the agent's
// launch display name. Leading/trailing whitespace is trimmed at the
// boundary, so " worker " lands as "worker".
func TestSpawn_ValidNameAcceptedAndTrimmed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "  code-reviewer_2  "})
	require.Equalf(t, http.StatusOK, resp.Code,
		"spawn with a valid name should succeed; body=%s", resp.Raw)
	f.AssertSpawnName(resp.ConvID, "code-reviewer_2", 2*time.Second)
}

// Scenario: the name is optional — an omitted/empty name spawns an
// unnamed agent (auto-generated label) rather than 400ing. The empty case
// must stay valid so "just give me an agent" still works.
func TestSpawn_EmptyNameAllowed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"name": ""})
	require.Equalf(t, http.StatusOK, resp.Code,
		"spawn with an empty name should succeed; body=%s", resp.Raw)
}
