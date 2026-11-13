package agentd

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The ticket reaches the forked launcher on the descriptor the environment
// names, and nowhere a bystander can read: not argv, not the environment.
func TestHandLaunchTicketToReachesTheChildOnAPipeOnly(t *testing.T) {
	cmd := exec.Command("sh", "-c", `cat <&"$`+session.LaunchTicketFDEnvVar+`"`)
	cmd.Env = os.Environ()
	var out strings.Builder
	cmd.Stdout = &out
	closePipe, revoke, err := handLaunchTicketTo(cmd, clcommon.SpawnArgs{Label: "spwn-handoff", ConvID: "conv-handoff"})
	require.NoError(t, err)
	t.Cleanup(revoke)
	require.NoError(t, cmd.Start())
	closePipe()
	require.NoError(t, cmd.Wait())

	ticket := out.String()
	issued := lookupLaunchTicket(ticket)
	require.NotNil(t, issued, "the child read the ticket the daemon minted")
	assert.Equal(t, "spwn-handoff", issued.scope.sessionID, "minted for the session the launcher will derive: its label")
	for _, arg := range cmd.Args {
		assert.NotContains(t, arg, ticket)
	}
	for _, kv := range cmd.Env {
		assert.NotContains(t, kv, ticket)
	}

	revoke()
	assert.Nil(t, lookupLaunchTicket(ticket), "the ticket dies with its launcher")

	resumed := exec.Command("true")
	closePipe, revoke, err = handLaunchTicketTo(resumed, clcommon.SpawnArgs{ConvID: "conv-handoff"})
	require.NoError(t, err)
	closePipe()
	revoke()
}
