package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The request-shape refusals of POST /v1/groups/{name}/spawn, pinned to the
// exact status, code and message a caller sees. handleGroupSpawn runs these as
// named steps on a spawnRequest (decode, authorize, normalize ...) and writes
// each step's refusal in one place; this table holds the wire response fixed
// while those steps are reshaped.
func TestSpawn_RequestRefusalsKeepTheirWireResponse(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]any
		code       string
		message    string // exact, unless messagePfx is set; neither set means any non-empty message
		messagePfx string
	}{
		{
			name: "non-interactive with identity",
			body: map[string]any{"name": "w", "non_interactive": true, "initial_message": "go", "role": "reviewer"},
			code: "invalid_request",
			message: "non-interactive spawn does not accept identity (other than name), " +
				"messaging, or interactive drive settings",
		},
		{
			name:    "sandbox profile and omit together",
			body:    map[string]any{"name": "w", "sandbox_profile": "strict", "omit_sandbox_profiles": true},
			code:    "invalid_sandbox_profile",
			message: "sandbox_profile and omit_sandbox_profiles are mutually exclusive",
		},
		{
			name:    "non-interactive without a brief",
			body:    map[string]any{"name": "w", "non_interactive": true},
			code:    "invalid_initial_message",
			message: "non_interactive requires initial_message",
		},
		{
			name:    "attachment with a control character",
			body:    map[string]any{"name": "w", "attachments": []string{"/tmp/a\x01b"}},
			code:    "invalid_attachments",
			message: "attachment path contains a control character",
		},
		{
			name:       "unknown permission override",
			body:       map[string]any{"name": "w", "permission_overrides": map[string]any{"no.such.slug": "grant"}},
			code:       "invalid_permission_overrides",
			messagePfx: `unknown permission slug "no.such.slug".`,
		},
		{
			name: "missing worktree directory",
			body: map[string]any{"name": "w", "worktree_path": "/nonexistent/tclaude-worktree"},
			code: "invalid_worktree",
		},
		{
			name: "group context for a shell one-shot",
			body: map[string]any{"name": "w", "harness": "shell", "non_interactive": true,
				"initial_message": "true", "include_group_context": true},
			code:    "invalid_group_context",
			message: "--group-context is not meaningful for a shell command",
		},
		{
			name:    "pi drive on another harness",
			body:    map[string]any{"name": "w", "harness": "claude", "pi_drive": true},
			code:    "invalid_request",
			message: `pi_drive is a pi-only drive; harness "claude" has none`,
		},
		{
			name:    "interactive shell with a brief",
			body:    map[string]any{"name": "w", "harness": "shell", "initial_message": "true"},
			code:    "invalid_initial_message",
			message: "shell spawn requires non_interactive when initial_message is set",
		},
		{
			name: "unknown tool governance",
			body: map[string]any{"name": "w", "tools": "bogus"},
			code: "invalid_tools",
		},
		{
			name: "unknown auto-compact window",
			body: map[string]any{"name": "w", "auto_compact_window": "bogus"},
			code: "invalid_auto_compact_window",
		},
		{
			name:    "missing sandbox profile",
			body:    map[string]any{"name": "w", "sandbox_profile": "nope"},
			code:    "invalid_sandbox_profile",
			message: `sandbox profile "nope" does not exist`,
		},
		{
			name: "task link that is not http(s)",
			body: map[string]any{"name": "w", "task_ref_url": "javascript:alert(1)"},
			code: "invalid_task_url",
		},
		{
			name:       "unresolvable reply_to",
			body:       map[string]any{"name": "w", "reply_to": "agt_nosuchagent"},
			code:       "invalid_reply_to",
			messagePfx: `reply_to "agt_nosuchagent": `,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")

			spawn := f.AsHuman().SpawnWith("alpha", tc.body)
			require.Equal(t, http.StatusBadRequest, spawn.Code, "body=%s", spawn.Raw)
			var got struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(spawn.Raw, &got), "body=%s", spawn.Raw)
			assert.Equal(t, tc.code, got.Code)
			if tc.message == "" && tc.messagePfx == "" {
				assert.NotEmpty(t, got.Error)
			} else if tc.messagePfx != "" {
				assert.True(t, strings.HasPrefix(got.Error, tc.messagePfx), "message %q, want prefix %q", got.Error, tc.messagePfx)
			} else {
				assert.Equal(t, tc.message, got.Error)
			}
		})
	}
}
