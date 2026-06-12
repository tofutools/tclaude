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
