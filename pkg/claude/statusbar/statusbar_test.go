package statusbar

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusLineInput_ParsesEffortLevel pins the effort.level field path
// against Claude Code's documented statusline schema. The whole feature
// hinges on reading the right key — CC emits the reasoning-effort level as
// a nested {"effort":{"level":"high"}} block — so this guards against a
// silent rename/typo of the json tag. The payload mirrors the documented
// example (https://code.claude.com/docs/en/statusline), trimmed to the
// fields the statusbar reads.
func TestStatusLineInput_ParsesEffortLevel(t *testing.T) {
	const payload = `{
		"session_id": "abc123",
		"model": { "id": "claude-opus-4-8", "display_name": "Opus 4.8" },
		"workspace": { "current_dir": "/tmp/proj" },
		"context_window": { "used_percentage": 8 },
		"effort": { "level": "high" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "high", input.Effort.Level, "effort.level field path")
}

// TestStatusLineInput_ParsesModelID pins the model.id field path — the
// full model ID the documented schema carries alongside display_name.
// Model inheritance on reincarnate/clone/resume hinges on persisting
// exactly this key (it's the only statusline model field `claude
// --model` accepts back), so guard the json tag the same way the
// effort test does.
func TestStatusLineInput_ParsesModelID(t *testing.T) {
	const payload = `{
		"session_id": "abc123",
		"model": { "id": "claude-fable-5", "display_name": "Fable 5" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "claude-fable-5", input.Model.ID, "model.id field path")
	assert.Equal(t, "Fable 5", input.Model.DisplayName, "model.display_name field path")
}

// TestStatusLineInput_ModelIDAbsent confirms an older Claude Code
// payload without model.id leaves the field empty — the signal
// UpdateSessionModelID treats as a no-op so inheritance degrades to
// claude's default instead of writing garbage.
func TestStatusLineInput_ModelIDAbsent(t *testing.T) {
	const payload = `{
		"model": { "display_name": "Opus 4.8" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "", input.Model.ID, "absent model.id leaves field empty")
}

// TestShortModelLabel pins the head-of-context-line model tag, including
// the fallback that keeps a full-ID-selected model (e.g. `--model
// claude-opus-5`, which CC doesn't emit a two-word display name for) from
// collapsing to the bare "ctx" placeholder.
func TestShortModelLabel(t *testing.T) {
	cases := []struct {
		name        string
		displayName string
		id          string
		want        string
	}{
		{"known two-word display name", "Opus 4.6", "claude-opus-4-6", "o4.6"},
		{"three-word display name", "Claude Opus 4.8", "claude-opus-4-8", "cOpus4.8"},
		{"display name with 1m suffix trimmed", "Opus 4.8[1m]", "claude-opus-4-8[1m]", "o4.8"},
		{"empty display name falls back to id", "", "claude-opus-5", "opus-5"},
		{"empty display name id with 1m suffix", "", "claude-opus-5[1m]", "opus-5"},
		{"single-token display name preferred over id", "Fable", "claude-fable-5", "fable"},
		{"single-token raw id display name", "claude-opus-5", "claude-opus-5", "opus-5"},
		{"nothing known yields placeholder", "", "", "ctx"},
		{"whitespace-only fields yield placeholder", "   ", "  ", "ctx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shortModelLabel(tc.displayName, tc.id))
		})
	}
}

// TestStatusLineInput_EffortAbsent confirms a payload without the effort
// block (model lacks reasoning-effort support) leaves Level empty — the
// signal both surfaces use to omit the effort token rather than render a
// blank one.
func TestStatusLineInput_EffortAbsent(t *testing.T) {
	const payload = `{
		"model": { "display_name": "Sonnet 4.6" },
		"context_window": { "used_percentage": 12 }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "", input.Effort.Level, "absent effort block leaves level empty")
}
