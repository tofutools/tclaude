package federationcmd

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"strings"
	"testing"
)

func TestBoardsActionSpecificFlags(t *testing.T) {
	root := boardsCmd()
	require.Empty(t, root.Flags().FlagUsages())
	for _, tc := range []struct {
		name      string
		args      []string
		required  string
		forbidden string
	}{
		{"ls", nil, "", "board"}, {"create", []string{"--name", "shared"}, "name", "token"},
		{"join", []string{"--token", "private"}, "token", "board"},
		{"invite", []string{"--board", "b"}, "board", "name"},
		{"invites", []string{"--board", "b"}, "board", "token"},
		{"set-role", []string{"--board", "b", "--instance", "i", "--role", "reader"}, "role", "token"},
		{"publish", []string{"--board", "b", "--from-board", "source", "--from-item", "item", "--from-version", "v"}, "board", "preview-token"},
		{"import", []string{"--board", "b", "--item", "i", "--version", "v", "--preview-token", "permit"}, "preview-token", "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, err := root.Find([]string{tc.name})
			require.NoError(t, err)
			require.Equal(t, tc.name, cmd.Name())
			require.NoError(t, cmd.ParseFlags(tc.args))
			require.NoError(t, cmd.ValidateRequiredFlags())
			if tc.required != "" {
				require.Contains(t, cmd.Flags().Lookup(tc.required).Usage, "(required)")
			}
			require.Nil(t, cmd.Flags().Lookup(tc.forbidden))
		})
	}
	cmd, _, err := boardsCmd().Find([]string{"download"})
	require.NoError(t, err)
	require.ErrorContains(t, cmd.ValidateRequiredFlags(), "file")
	require.Len(t, boardsCmd().Commands(), 21)
}

func TestMovesListTableAndJSON(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl = oldAvail; agent.DaemonRequestImpl = oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		require.Equal(t, "GET", method)
		require.Equal(t, "/v1/federation/moves", path)
		return json.Unmarshal([]byte(`{"moves":[{"id":"move-1","direction":"out","peer":"b","state":"awaiting_confirmation","source_agent":"agt_source","group":"team"}]}`), out)
	}
	for _, asJSON := range []bool{false, true} {
		var out, stderr bytes.Buffer
		require.Zero(t, runMovesList(&movesListParams{JSON: asJSON}, &out, &stderr), stderr.String())
		if asJSON {
			require.True(t, json.Valid(out.Bytes()))
			require.Contains(t, out.String(), `"moves"`)
		} else {
			require.True(t, strings.HasPrefix(out.String(), "ID"))
			require.Contains(t, out.String(), "DIRECTION")
			require.Contains(t, out.String(), "move-1")
			require.Contains(t, out.String(), "awaiting_confirmation")
		}
	}
}
