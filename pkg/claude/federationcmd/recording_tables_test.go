package federationcmd

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"strings"
	"testing"
)

func TestRecordingListCommandsTableAndJSON(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl = oldAvail; agent.DaemonRequestImpl = oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	for _, tc := range []struct{ action, tail, reply, heading string }{
		{"list", "", `{"boards":[{"id":"board","name":"Recipes\nSafe","role":"owner","epoch":1}],"next_cursor":"next"}`, "ID"},
		{"items", "/board/items", `{"items":[{"id":"item","name":"Role pack","kind":"config","version":"v1","update_available":true}]}`, "ID"},
		{"invites", "/board/invites", `{"invites":[{"token_id":"hash","role":"reader","expires_at":"soon"}]}`, "TOKEN ID"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
				require.Equal(t, "GET", method)
				require.Equal(t, "/v1/federation/boards"+tc.tail, path)
				require.True(t, opts.NoRetry)
				return json.Unmarshal([]byte(tc.reply), out)
			}
			for _, asJSON := range []bool{false, true} {
				var out, stderr bytes.Buffer
				require.Zero(t, runBoardCommand(&boardCommandParams{Action: tc.action, Board: "board", JSON: asJSON}, &out, &stderr), stderr.String())
				if asJSON {
					require.True(t, json.Valid(out.Bytes()))
				} else {
					require.True(t, strings.HasPrefix(out.String(), tc.heading))
					require.False(t, json.Valid(out.Bytes()))
					require.NotContains(t, out.String(), "Recipes\nSafe")
				}
			}
		})
	}
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "/v1/federation/hub/status", path)
		return json.Unmarshal([]byte(`{"hub_id":"hub","hub_version":"v1-dev","connected":true,"admin":true}`), out)
	}
	for _, asJSON := range []bool{false, true} {
		var out, stderr bytes.Buffer
		require.Zero(t, runHubRead("status", &hubReadParams{JSON: asJSON}, &out, &stderr), stderr.String())
		if asJSON {
			require.True(t, json.Valid(out.Bytes()))
		} else {
			require.True(t, strings.HasPrefix(out.String(), "HUB"))
			require.Contains(t, out.String(), "v1-dev")
		}
	}
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "DELETE", method)
		require.Equal(t, "/v1/federation/boards/board", path)
		return json.Unmarshal([]byte(`{"ok":true}`), out)
	}
	var out, stderr bytes.Buffer
	require.Zero(t, runBoardCommand(&boardCommandParams{Action: "delete", Board: "board"}, &out, &stderr))
}
func TestFileListFlagsAndTable(t *testing.T) {
	cmd, _, err := fileCmd().Find([]string{"ls"})
	require.NoError(t, err)
	require.NotNil(t, cmd.Flags().Lookup("json"))
	require.Nil(t, cmd.Flags().Lookup("output"))
	var out bytes.Buffer
	require.Zero(t, printRecordingTable(&out, "files", map[string]any{"entries": []any{map[string]any{"path": "src", "kind": "directory", "size": 12}}, "truncated": true}))
	require.Contains(t, out.String(), "PATH")
	require.Contains(t, out.String(), "directory")
	require.Contains(t, out.String(), "Listing capped")
}
