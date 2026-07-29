package session

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An empty response must write NO bytes. Every hook that has no opinion took
// this path before HookResponse existed, and a harness reads empty stdout as
// "no instruction" — emitting `{}` instead would change behaviour on every
// event tclaude observes.
func TestHookResponseEmptyWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, HookResponse{}.Write(&buf, "SessionStart"))
	assert.Empty(t, buf.Bytes())
	assert.True(t, HookResponse{}.IsEmpty())
}

func TestHookResponseWritesAdditionalContext(t *testing.T) {
	var buf bytes.Buffer
	resp := HookResponse{AdditionalContext: "Standing orders in force: push the PR early."}
	require.NoError(t, resp.Write(&buf, "SessionStart"))

	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "SessionStart", got.HookSpecificOutput.HookEventName,
		"the event name must be echoed or the harness ignores the document")
	assert.Contains(t, got.HookSpecificOutput.AdditionalContext, "push the PR early")
}

func TestHookResponseWritesBlockDecision(t *testing.T) {
	var buf bytes.Buffer
	resp := HookResponse{Decision: "block", Reason: "context is too small to compact"}
	require.NoError(t, resp.Write(&buf, "PreCompact"))

	var got struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "block", got.Decision)
	assert.Contains(t, got.Reason, "too small")
}

// One stream, one document. On a blocking exit the harness treats the decision
// as the whole answer, so writing both would emit two JSON documents into a
// stream parsed as one — and the context half would be the one silently lost.
func TestHookResponseBlockWinsOverContext(t *testing.T) {
	var buf bytes.Buffer
	resp := HookResponse{
		Decision:          "block",
		Reason:            "refused",
		AdditionalContext: "this must not be emitted alongside the decision",
	}
	require.NoError(t, resp.Write(&buf, "PreCompact"))

	assert.NotContains(t, buf.String(), "must not be emitted")

	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	var first map[string]any
	require.NoError(t, dec.Decode(&first))
	assert.Equal(t, "block", first["decision"])
	assert.False(t, dec.More(), "exactly one document must be written")
}
