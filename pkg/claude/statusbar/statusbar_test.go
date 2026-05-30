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
		"model": { "display_name": "Opus 4.8" },
		"workspace": { "current_dir": "/tmp/proj" },
		"context_window": { "used_percentage": 8 },
		"effort": { "level": "high" }
	}`

	var input StatusLineInput
	require.NoError(t, json.Unmarshal([]byte(payload), &input), "unmarshal statusline JSON")
	assert.Equal(t, "high", input.Effort.Level, "effort.level field path")
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
