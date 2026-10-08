package proto

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAgentStatusAllowlistAndTaskURLs(t *testing.T) {
	for _, s := range []string{"file:///private/work", "https://user:secret@tracker.test/1", "https://claude.ai/code/secret", "https://host.test/sessions/secret"} {
		require.Empty(t, SafeStatusTaskURL(s))
	}
	require.Equal(t, "https://tracker.test/issues/1", SafeStatusTaskURL("https://tracker.test/issues/1?token=secret#private"))
	rows := SanitizeAgentStatuses([]AgentStatus{{Agent: "agt_alice", Name: "alice\x1b[31m", Status: "running", RecoveryStatus: "backoff", Model: "model\nsecret", TaskURL: "file:///secret", TaskLabel: "hidden", Context: &AgentContext{Percent: 101, Window: 10}}, {Agent: "bad", Name: "bad"}})
	require.Len(t, rows, 1)
	require.NotContains(t, rows[0].Name, "\x1b")
	require.NotContains(t, rows[0].Model, "\n")
	require.Equal(t, "running", rows[0].Status)
	require.Equal(t, "backoff", rows[0].RecoveryStatus)
	require.Empty(t, rows[0].TaskLabel)
	require.Nil(t, rows[0].Context)
}
