package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These pin runCloneOrchestration's pre-spawn refusals that no other clone
// test reaches by code, before it is split into steps: each is refused with
// its status, code and message, and none of them spawns or registers a clone.
// The shell-harness refusal (unsupported_harness) is not among them: a
// recorded relaunch profile carries an approval policy the shell harness
// rejects, so relaunch_profile refuses first.
func TestClone_RefusalsSpawnNothing(t *testing.T) {
	const src = "pin-aaaa-bbbb-cccc-0001"
	for _, tc := range []struct {
		name    string
		setup   func(*testharness.Flow)
		body    map[string]any
		status  int
		code    string
		message string
	}{
		{"a source without a live session", func(f *testharness.Flow) {
			f.HaveConvWithTitle(src, "worker")
		}, nil, http.StatusServiceUnavailable, "no_tmux",
			"target conv pin-aaaa has no live tmux session; can't clone without a cwd to spawn the sibling into"},
		{"a relaunch profile that does not resolve", func(f *testharness.Flow) {
			haveShellSessionSource(t, f)
		}, nil, http.StatusConflict, "relaunch_profile",
			`invalid durable approval policy: harness "shell" has no launch-time approval policy (its approval handling is configured out of band, not via --ask-for-approval)`},
		{"a missing extra group", func(f *testharness.Flow) {
			f.HaveConvWithTitle(src, "worker")
			f.HaveAliveSession(src, "spwn-pin", "tclaude-spwn-pin", f.TestCwd("pin"))
		}, map[string]any{"no_copy_conv": true, "group": "nope"}, http.StatusNotFound, "not_found",
			`group "nope" not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			tc.setup(f)
			f.HaveMember("alpha", src)
			agentsBefore, err := db.ListActiveAgents()
			require.NoError(t, err)
			body := tc.body
			if body == nil {
				body = map[string]any{"no_copy_conv": true}
			}

			resp := f.AsHuman().CloneWith(src, body)

			require.Equal(t, tc.status, resp.Code, "body=%s", resp.Raw)
			var refusal struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(resp.Raw, &refusal), string(resp.Raw))
			assert.Equal(t, tc.code, refusal.Code)
			assert.Equal(t, tc.message, refusal.Error)
			assert.Empty(t, resp.NewConv, "a refused clone must not spawn")
			agentsAfter, err := db.ListActiveAgents()
			require.NoError(t, err)
			assert.Len(t, agentsAfter, len(agentsBefore), "a refused clone registers no agent")
		})
	}
}

// haveShellSessionSource makes the pinned source a live agent whose session
// now records the shell harness, which the approval policy its relaunch
// profile carries does not fit.
func haveShellSessionSource(t *testing.T, f *testharness.Flow) {
	t.Helper()
	f.HaveConvWithTitle("pin-aaaa-bbbb-cccc-0001", "worker")
	f.HaveAliveSession("pin-aaaa-bbbb-cccc-0001", "spwn-pin", "tclaude-spwn-pin", f.TestCwd("pin"))
	row, err := db.LoadSession("spwn-pin")
	require.NoError(t, err)
	row.Harness = harness.ShellName
	require.NoError(t, db.SaveSession(row))
}
