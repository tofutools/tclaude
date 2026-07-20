package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Scenario: a human (or the dashboard / CLI) spawns an agent with a name
// that isn't a safe token — a space, punctuation, or unicode. With
// auto-normalization ON (the default — config agent.spawn_name_normalize),
// the daemon coerces it to the safe [A-Za-z0-9_-] branch-token charset
// instead of 400ing, so any typed name "just works": "code reviewer!" lands
// as the agent's launch name "code-reviewer". The name doubles as a git
// worktree branch name and becomes the conversation title, hence the
// charset.
func TestSpawn_InvalidNameNormalized(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	cases := map[string]struct{ in, want string }{
		"space":       {"code reviewer", "code-reviewer"},
		"punctuation": {"code reviewer!", "code-reviewer"},
		"slash":       {"code/reviewer", "code-reviewer"},
		"brackets":    {"[reviewer]", "reviewer"},
		"unicode":     {"café", "caf"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			resp := f.AsHuman().SpawnWith("alpha", map[string]any{"name": tc.in})
			require.Equalf(t, http.StatusOK, resp.Code,
				"spawn with name %q should normalize + succeed; body=%s", tc.in, resp.Raw)
			f.AssertSpawnName(resp.ConvID, tc.want, 10*time.Second)
		})
	}
}

// Scenario: the operator opted OUT of auto-normalization
// (agent.spawn_name_normalize = false). Now the daemon rejects an unsafe
// name at the spawn boundary with a 400 instead of normalizing it — the
// strict legacy behaviour, kept available for anyone who wants names to fail
// loudly rather than be rewritten.
func TestSpawn_InvalidNameRejectedWhenNormalizeOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// Persist the opt-out into the test HOME's config.json; handleGroupSpawn
	// reads config live, so the next spawn sees it.
	off := false
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{SpawnNameNormalize: &off}}))

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
				"spawn with name %q should 400 when normalize is off; body=%s", name, resp.Raw)
			assert.Containsf(t, string(resp.Raw), "A-Za-z0-9_-",
				"the error should name the allowed charset; got %s", resp.Raw)
		})
	}
}

// Scenario: a valid name spawns cleanly and is applied as the agent's
// launch display name. Leading/trailing whitespace is trimmed at the
// boundary, so " worker " lands as "worker". (Normalization is a no-op on
// an already-valid name, so this holds regardless of the toggle.)
func TestSpawn_ValidNameAcceptedAndTrimmed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "  code-reviewer_2  "})
	require.Equalf(t, http.StatusOK, resp.Code,
		"spawn with a valid name should succeed; body=%s", resp.Raw)
	f.AssertSpawnName(resp.ConvID, "code-reviewer_2", 10*time.Second)
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
