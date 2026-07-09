package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CLI's templateJSON/templateAgentJSON mirror the daemon's wire shape, and
// `templates edit --file` is a FULL REPLACE — any field the mirror doesn't
// carry is silently destroyed by the show --json → edit --file loop the
// agent-circles skill teaches. This test pins the lossless round-trip of the
// template-local spawn profile (profile_inline), which the mirror deliberately
// keeps as raw JSON so daemon-side shape growth can never be stripped here.
func TestTemplateJSON_ProfileInlineRoundTripsLosslessly(t *testing.T) {
	daemonWire := `{
		"name": "crew",
		"agents": [
			{
				"name": "lead",
				"permissions": [],
				"profile_inline": {
					"model": "haiku",
					"effort": "high",
					"remote_control": true,
					"is_owner": true,
					"permission_overrides": {"groups.spawn": "grant", "message.direct": "deny"},
					"some_future_field": "must-survive-too"
				}
			}
		],
		"work_pattern": [],
		"process": [],
		"rhythms": []
	}`

	var tj templateJSON
	require.NoError(t, json.Unmarshal([]byte(daemonWire), &tj))
	require.Len(t, tj.Agents, 1)
	require.NotEmpty(t, tj.Agents[0].ProfileInline, "profile_inline decoded")

	out, err := json.Marshal(tj)
	require.NoError(t, err)

	var back map[string]any
	require.NoError(t, json.Unmarshal(out, &back))
	agents := back["agents"].([]any)
	pi, ok := agents[0].(map[string]any)["profile_inline"].(map[string]any)
	require.True(t, ok, "profile_inline survives the re-marshal")
	assert.Equal(t, "haiku", pi["model"])
	assert.Equal(t, true, pi["remote_control"])
	assert.Equal(t, true, pi["is_owner"])
	assert.Equal(t, "must-survive-too", pi["some_future_field"],
		"unknown daemon-side fields round-trip too (raw JSON, not a typed mirror)")
	po := pi["permission_overrides"].(map[string]any)
	assert.Equal(t, "grant", po["groups.spawn"])
	assert.Equal(t, "deny", po["message.direct"])
}

// The human `templates show` view must surface a custom launch config —
// including the access bits (owner, permission overrides) — so an operator
// auditing a template sees what it grants.
func TestInlineProfileTag_RendersAccessBits(t *testing.T) {
	tag := inlineProfileTag(json.RawMessage(
		`{"model":"haiku","remote_control":true,"is_owner":true,` +
			`"permission_overrides":{"groups.spawn":"grant","message.direct":"deny"}}`))
	assert.True(t, strings.HasPrefix(tag, "custom-launch={"), "tag shape: %s", tag)
	assert.Contains(t, tag, "model haiku")
	assert.Contains(t, tag, "remote-control true")
	assert.Contains(t, tag, "owner true")
	assert.Contains(t, tag, "groups.spawn:grant")
	assert.Contains(t, tag, "message.direct:deny")

	assert.Empty(t, inlineProfileTag(nil), "absent profile renders nothing")
	assert.Equal(t, "custom-launch=(empty)", inlineProfileTag(json.RawMessage(`{}`)))
}
