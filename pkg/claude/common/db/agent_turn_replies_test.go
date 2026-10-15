package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecordScribeTurnReply_ScribesOnlyBoundedAndCapped(t *testing.T) {
	setupTestDB(t)
	scribeGroup, err := CreateAgentGroup("process-scribe", ScribeGroupDescr)
	require.NoError(t, err)
	lookalike, err := CreateAgentGroup("not-scribes", "something else")
	require.NoError(t, err)

	scribe, _, err := EnsureAgentForConv("scribe-conv", "test")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: scribeGroup, ConvID: "scribe-conv", Role: "scribe"}))
	plain, _, err := EnsureAgentForConv("plain-conv", "test")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: scribeGroup, ConvID: "plain-conv"}))
	// role "scribe" in a group that is not the scribe group is not a scribe.
	impostor, _, err := EnsureAgentForConv("impostor-conv", "test")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: lookalike, ConvID: "impostor-conv", Role: "scribe"}))

	now := time.Unix(1_800_000_000, 0)
	for _, conv := range []string{"plain-conv", "impostor-conv", "unknown-conv"} {
		stored, err := RecordScribeTurnReply(conv, "secret", now)
		require.NoError(t, err)
		require.False(t, stored, conv)
	}
	for _, agentID := range []string{plain, impostor} {
		got, err := ListScribeTurnReplies(agentID, 0)
		require.NoError(t, err)
		require.Empty(t, got)
	}
	isScribe, err := IsScribeAgent(scribe)
	require.NoError(t, err)
	require.True(t, isScribe)
	isScribe, err = IsScribeAgent(impostor)
	require.NoError(t, err)
	require.False(t, isScribe)

	for i := 0; i < ScribeTurnReplyKeep+5; i++ {
		stored, err := RecordScribeTurnReply("scribe-conv", "reply "+strings.Repeat("x", i), now.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
		require.True(t, stored)
	}
	got, err := ListScribeTurnReplies(scribe, 0)
	require.NoError(t, err)
	require.Len(t, got, ScribeTurnReplyKeep)
	require.Equal(t, "reply "+strings.Repeat("x", 5), got[0].Body, "the oldest replies are pruned")
	require.True(t, got[len(got)-1].CreatedAt.Equal(now.Add(time.Duration(ScribeTurnReplyKeep+4)*time.Second)))

	_, err = RecordScribeTurnReply("scribe-conv", strings.Repeat("é", ScribeTurnReplyMaxBytes), now)
	require.NoError(t, err)
	got, err = ListScribeTurnReplies(scribe, got[len(got)-1].ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.LessOrEqual(t, len(got[0].Body), ScribeTurnReplyMaxBytes)
	require.True(t, strings.HasSuffix(got[0].Body, "…"))
	require.True(t, strings.HasPrefix(got[0].Body, "éé"))
}
