package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
)

// tcl-gpzct5: the caller of a Copilot API compaction is told it was accepted
// before the follow-up is sent. A follow-up whose session.send then fails must
// not be lost: it waits in the agent's inbox, where the nudge pipeline
// delivers it over the drive (never by keystrokes).
func TestCopilotAPICompactionFollowUpSurvivesASendFailure(t *testing.T) {
	fixture := newCopilotAPIDriveFixture(t)
	fixture.server.failMethod(copilotapi.MethodSessionSend, "transient")

	_, failure := dispatchSlashCommand(fixture.convID, "/compact", "CARRY ON AFTER COMPACTION", "compact")
	require.Equal(t, slashFailureNone, failure)

	var queued *db.AgentMessage
	require.Eventually(t, func() bool {
		msgs, err := db.ListAgentMessagesForConv(fixture.convID, 50)
		require.NoError(t, err)
		for _, m := range msgs {
			if m.Subject == compactionFollowUpSubject {
				queued = m
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the failed follow-up is queued in the agent's inbox")
	assert.Equal(t, "CARRY ON AFTER COMPACTION", queued.Body)
	fixture.assertNoKeystrokes(t)
}
