package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentImportReview(t *testing.T) {
	review, err := AgentImportReview([]byte(`{
		"agent": {"name": "visitor", "role": "scout\u202e", "harness": "claude"},
		"cwd": "/work", "group": "crew", "history": true,
		"signatures": [{"keyId": "k1", "status": "valid", "trust": "known peer alpha"}],
		"findings": [{"kind": "aws-key", "count": 2, "locations": ["history"]}],
		"permissions": [{"slug": "agent.send", "decision": "dropped"}],
		"managed_script_approval_required": true,
		"carried_workflows": [{"key": "w1", "root": "release"}],
		"confirm": {"digest": "d", "can_keep": false, "items": [{"id": "env:X", "kind": "environment", "label": "environment variable X", "gives": "a value"}]}
	}`))
	require.NoError(t, err)
	for _, want := range []string{
		"Agent: visitor, on claude", "Role: \"scout\\u202e\"", "Group here: crew", "Works in: /work", "Lands with its conversation history.",
		"Signature: valid, key k1 (known peer alpha)", "aws-key: 2",
		"Elevated items, 1, none of which this caller can keep:", "environment variable X",
		"Carries 1 permission row(s)", "managed instructions with code", "also ships 1 workflow(s)",
	} {
		assert.Contains(t, review.Summary, want)
	}
	assert.Contains(t, review.Full, "Only a human on this node can keep these")
	assert.Contains(t, review.Full, "Also carries workflow release")
	assert.NotContains(t, review.Full, "\u202e", "a format character is shown as its escape")
	assert.Empty(t, review.Refused)
}

func TestAgentImportReviewBoundsTheSummary(t *testing.T) {
	items := make([]string, reviewSummaryItems+5)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id": "%d", "label": %q}`, i, strings.Repeat("x", 500))
	}
	review, err := AgentImportReview([]byte(`{"confirm": {"items": [` + strings.Join(items, ",") + `]}}`))
	require.NoError(t, err)
	assert.Contains(t, review.Summary, "and 5 more")
	assert.NotContains(t, review.Summary, strings.Repeat("x", reviewSummaryChars+1))
	assert.Less(t, len(review.Summary), 5000)
}

func TestGroupImportReview(t *testing.T) {
	review, err := GroupImportReview([]byte(`{
		"source": "paf", "source_group": "crew\u202e", "agent_count": 2, "conv_count": 2, "message_count": 9,
		"target_name": "crew2", "target_name_valid": true, "group_name_taken": true,
		"signatures": [{"keyId": "k1", "status": "valid", "trust": "this node"}],
		"findings": [{"kind": "aws-key", "count": 1, "locations": ["a"]}],
		"carried_workflows": [{"key": "w1", "root": "release"}],
		"confirm": {"digest": "d", "can_keep": true, "items": [{"id": "owner:x", "label": "owner x", "gives": "ownership"}]}
	}`))
	require.NoError(t, err)
	for _, want := range []string{
		"Group: \"crew\\u202e\", landing as crew2", "Holds: 2 agent(s), 2 conversation(s), 9 message(s)",
		"Signature: valid, key k1 (this node)", "aws-key: 1", "Elevated items, 1:", "owner x", "also ships 1 workflow(s)",
	} {
		assert.Contains(t, review.Summary, want)
	}
	assert.Contains(t, review.Full, "archive:        group PAF")
	assert.Contains(t, review.Full, "signature:      valid, key k1 (this node)")
	assert.NotContains(t, review.Full, "\u202e")
	assert.Equal(t, `a group named "crew2" already exists`, review.Refused)
}
