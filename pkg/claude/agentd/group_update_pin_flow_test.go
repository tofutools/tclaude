package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin handleGroupUpdate before it is split into steps and its per-field
// writes share one helper: a rejected field is refused before any field is
// written, a full update writes every field and answers with what it stored,
// and a write that fails part way keeps the writes before it and makes none
// after it.

const pinGroupDescr = "pinned description"

func pinGroup(t *testing.T) *db.AgentGroup {
	t.Helper()
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.NotNil(t, g)
	return g
}

func TestGroupUpdate_ARejectedFieldWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    map[string]any
		code    string
		message string
	}{
		{"nothing to update", map[string]any{}, "invalid_arg",
			"nothing to update (expected descr, default_cwd, default_context, environment, default_spawn_group, default_profile, default_prompts, max_members, notify_enabled, remote_control_policy, reinject_after_compact, permissions and/or owner_scopes)"},
		{"an invalid environment", map[string]any{"environment": []map[string]string{{"name": "", "value": "x"}}},
			"invalid_environment", ""},
		{"an invalid re-injection mode", map[string]any{"reinject_after_compact": "sideways"},
			"invalid_reinject_after_compact", `invalid reinject_after_compact "sideways" (want contexts, identity, or off)`},
		{"an invalid remote-control policy", map[string]any{"remote_control_policy": "maybe"},
			"invalid_remote_control_policy", `invalid remote_control_policy "maybe" (want inherit, optin, or deny)`},
		{"a relative default directory", map[string]any{"default_cwd": "relative/dir"}, "invalid_cwd", ""},
		{"a missing default profile", map[string]any{"default_profile": "no-such-profile"}, "invalid_profile", ""},
		{"invalid owner scopes", map[string]any{"owner_scopes": []string{"not", "a", "map"}}, "invalid_owner_scopes", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			if len(tc.body) > 0 {
				tc.body["descr"] = "must not land"
			}

			rec := humanReq(t, f, http.MethodPatch, "/v1/groups/crew", tc.body)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var refusal struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refusal))
			assert.Equal(t, tc.code, refusal.Code)
			if tc.message != "" {
				assert.Equal(t, tc.message, refusal.Error)
			} else {
				assert.NotEmpty(t, refusal.Error)
			}
			assert.Empty(t, pinGroup(t).Descr, "no field is written before the refusal")
		})
	}
}

func TestGroupUpdate_WritesEveryFieldAndAnswersWithIt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	cwd := testutil.CanonicalTempDir(t)

	rec := humanReq(t, f, http.MethodPatch, "/v1/groups/crew", map[string]any{
		"descr":                  "two\nlines",
		"default_cwd":            cwd,
		"default_spawn_group":    true,
		"default_context":        "line one\r\nline two",
		"environment":            []map[string]string{{"name": "PIN_VAR", "value": "pinned"}},
		"max_members":            -3,
		"notify_enabled":         false,
		"remote_control_policy":  "deny",
		"reinject_after_compact": "off",
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{
		"group": "crew",
		"descr": "two lines",
		"default_cwd": `+mustJSONString(t, cwd)+`,
		"default_spawn_group": true,
		"default_context": "line one\nline two",
		"environment": [{"name": "PIN_VAR", "value": "pinned"}],
		"max_members": 0,
		"notify_enabled": false,
		"remote_control_policy": "deny",
		"reinject_after_compact": "off"
	}`, rec.Body.String())
	g := pinGroup(t)
	assert.Equal(t, "two lines", g.Descr)
	assert.Equal(t, cwd, g.DefaultCwd)
	assert.True(t, g.DefaultSpawnGroup)
	assert.Equal(t, "line one\nline two", g.DefaultContext)
	assert.Zero(t, g.MaxMembers)
	assert.False(t, g.NotifyEnabled)
	assert.Equal(t, "off", g.ReinjectAfterCompact)
}

// The writes run in a fixed order and are not one transaction. The default
// spawn group needs a default directory, which a default_cwd in the same
// update provides because it is written first; without one that write fails,
// leaving the description (written before it) and not the notification
// switch (written after it).
func TestGroupUpdate_AFailedWriteKeepsTheEarlierOnes(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	require.True(t, pinGroup(t).NotifyEnabled)

	rec := humanReq(t, f, http.MethodPatch, "/v1/groups/crew", map[string]any{
		"descr": pinGroupDescr, "default_spawn_group": true, "notify_enabled": false,
	})

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refusal))
	assert.Equal(t, "invalid_default_spawn_group", refusal.Code)
	assert.Equal(t, `group "crew" needs a default spawn directory before it can be the default auto-join group`, refusal.Error)
	g := pinGroup(t)
	assert.Equal(t, pinGroupDescr, g.Descr, "the write before the failure landed")
	assert.False(t, g.DefaultSpawnGroup)
	assert.True(t, g.NotifyEnabled, "no write after the failure ran")

	rec = humanReq(t, f, http.MethodPatch, "/v1/groups/crew", map[string]any{
		"default_spawn_group": true, "default_cwd": testutil.CanonicalTempDir(t),
	})
	require.Equal(t, http.StatusOK, rec.Code, "default_cwd is written before default_spawn_group: %s", rec.Body.String())
	assert.True(t, pinGroup(t).DefaultSpawnGroup)
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	return string(raw)
}
