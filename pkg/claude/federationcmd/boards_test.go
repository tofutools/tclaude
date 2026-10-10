package federationcmd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"testing"
)

func TestBoardCLIUsesHumanLocalRoutesWithoutRetry(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	for _, tc := range []struct {
		p            boardCommandParams
		method, path string
	}{
		{boardCommandParams{Action: "list", Cursor: "opaque"}, "GET", "?cursor=opaque"},
		{boardCommandParams{Action: "create", Name: "recipes"}, "POST", ""},
		{boardCommandParams{Action: "join", Token: "secret"}, "POST", "/join"},
		{boardCommandParams{Action: "show", Board: "board"}, "GET", "/board"},
		{boardCommandParams{Action: "invite", Board: "board", Role: "publisher", TTL: "2h"}, "POST", "/board/invites"},
		{boardCommandParams{Action: "revoke-invite", Board: "board", TokenID: "hash"}, "DELETE", "/board/invites/hash"},
		{boardCommandParams{Action: "set-member", Board: "board", Instance: "member", Role: "owner"}, "PUT", "/board/members/member"},
		{boardCommandParams{Action: "remove-member", Board: "board", Instance: "member"}, "DELETE", "/board/members/member"},
		{boardCommandParams{Action: "rotate-key", Board: "board"}, "POST", "/board/rotate-key"},
		{boardCommandParams{Action: "leave", Board: "board"}, "DELETE", "/board/membership"},
	} {
		t.Run(tc.p.Action, func(t *testing.T) {
			called := false
			agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
				called = true
				require.Equal(t, tc.method, method)
				require.Equal(t, "/v1/federation/boards"+tc.path, path)
				require.True(t, opts.NoRetry)
				return nil
			}
			var stdout, stderr bytes.Buffer
			require.Zero(t, runBoardCommand(&tc.p, &stdout, &stderr), stderr.String())
			require.True(t, called)
		})
	}
}
