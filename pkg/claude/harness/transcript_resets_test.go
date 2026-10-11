package harness

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// These minimal native records follow the upstream replay schemas linked in
// testdata/transcript-resets.md. Sentinel prose is invented, not private history.
func TestTranscriptResetMatrix(t *testing.T) {
	claude := func(id, parent, text string, extra obj) obj {
		r := obj{"type": "user", "uuid": id, "parentUuid": parent, "sessionId": fixtureClaude, "message": obj{"role": "user", "content": text}}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	codex := func(text string) obj {
		return obj{"type": "response_item", "payload": obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": text}}}}
	}
	oc := func(id, text string, summary bool) obj {
		role := "user"
		if summary {
			role = "assistant"
		}
		return obj{"message": obj{"info": obj{"id": id, "sessionID": fixtureOC, "role": role, "summary": summary, "finish": "stop"}, "parts": []obj{{"id": "prt_" + id, "type": "text", "text": text}}}}
	}
	cop := func(id, text string) obj { return obj{"id": id, "type": "user.message", "data": obj{"content": text}} }
	gem := func(id, text string) obj { return obj{"id": id, "type": "user", "content": text} }
	cases := []struct {
		name    string
		codec   TranscriptCodec
		id      string
		records []obj
		want    []string
		loss    transcript.LossKind
	}{
		{"claude/manual-auto-repeat", claudeTranscript{}, fixtureClaude, []obj{
			claude("old", "", "SECRET OLD", nil), obj{"type": "system", "subtype": "compact_boundary", "uuid": "b1", "sessionId": fixtureClaude},
			claude("s1", "b1", "SUMMARY OLD", obj{"isCompactSummary": true}), claude("middle", "s1", "SECRET MIDDLE", nil),
			obj{"type": "system", "subtype": "compact_boundary", "uuid": "b2", "sessionId": fixtureClaude},
			claude("s2", "b2", "SUMMARY FINAL", obj{"isCompactSummary": true}), claude("tail", "s2", "TAIL", nil),
		}, []string{"user|SUMMARY:SUMMARY FINAL", "user|TAIL"}, transcript.LossPreCompaction},
		{"claude/clear-new-root-and-sidechain", claudeTranscript{}, fixtureClaude, []obj{
			claude("old", "", "SECRET OLD", nil), claude("new", "", "TASK", nil), claude("side", "old", "SECRET SIDE", obj{"isSidechain": true}), claude("tail", "new", "TAIL", nil),
		}, []string{"user|TASK", "user|TAIL"}, transcript.LossInactiveBranch},
		{"claude/foreign-session", claudeTranscript{}, fixtureClaude, []obj{
			claude("new", "", "TASK", nil), claude("foreign", "", "SECRET FOREIGN", obj{"sessionId": fixtureCodex}),
		}, []string{"user|TASK"}, transcript.LossInactiveBranch},
		{"codex/manual-auto-repeat", codexTranscript{}, fixtureCodex, []obj{
			{"type": "session_meta", "payload": obj{"id": fixtureCodex}}, codex("SECRET OLD"),
			{"type": "compacted", "payload": obj{"message": "SUMMARY OLD"}}, codex("SECRET MIDDLE"),
			{"type": "compacted", "payload": obj{"message": "SUMMARY FINAL", "replacement_history": []obj{{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": "SUMMARY FINAL"}}}}}}, codex("TAIL"),
		}, []string{"user|SUMMARY:SUMMARY FINAL", "user|TAIL"}, transcript.LossPreCompaction},
		{"codex/rewind", codexTranscript{}, fixtureCodex, []obj{
			{"type": "session_meta", "payload": obj{"id": fixtureCodex}}, codex("TASK"), codex("SECRET REWOUND"),
			{"type": "event_msg", "payload": obj{"type": "thread_rolled_back", "num_turns": 1}}, codex("TAIL"),
		}, []string{"user|TASK", "user|TAIL"}, transcript.LossInactiveBranch},
		{"opencode/manual-auto-repeat", openCodeTranscript{}, fixtureOC, []obj{
			{"info": obj{"id": fixtureOC}}, oc("msg_1", "SECRET OLD", false), oc("msg_2", "SUMMARY OLD", true), oc("msg_3", "SECRET MIDDLE", false), oc("msg_4", "SUMMARY FINAL", true), oc("msg_5", "TAIL", false),
		}, []string{"user|SUMMARY:SUMMARY FINAL", "user|TAIL"}, transcript.LossPreCompaction},
		{"opencode/revert", openCodeTranscript{}, fixtureOC, []obj{
			{"info": obj{"id": fixtureOC, "revert": obj{"messageID": "msg_2"}}}, oc("msg_1", "TASK", false), oc("msg_2", "SECRET REWOUND", false), oc("msg_3", "SECRET LATER", false),
		}, []string{"user|TASK"}, transcript.LossInactiveBranch},
		{"copilot/manual-auto-repeat", copilotTranscript{}, fixtureCop, []obj{
			{"type": "session.start", "data": obj{"sessionId": fixtureCop}}, cop("u1", "SECRET OLD"),
			{"type": "session.compaction_complete", "data": obj{"success": true, "summaryContent": "SUMMARY OLD"}}, cop("u2", "SECRET MIDDLE"),
			{"type": "session.compaction_complete", "data": obj{"success": true, "summaryContent": "SUMMARY FINAL"}}, cop("u3", "TAIL"),
		}, []string{"user|SUMMARY:SUMMARY FINAL", "user|TAIL"}, transcript.LossPreCompaction},
		{"copilot/clear", copilotTranscript{}, fixtureCop, []obj{
			{"type": "session.start", "data": obj{"sessionId": fixtureCop}}, cop("u1", "SECRET OLD"),
			{"type": "session.context_cleared", "data": obj{"messagesCleared": 1, "initialMessage": "TASK"}}, cop("u2", "TAIL"),
		}, []string{"user|TASK", "user|TAIL"}, transcript.LossClearedHistory},
		{"copilot/rewind", copilotTranscript{}, fixtureCop, []obj{
			{"type": "session.start", "data": obj{"sessionId": fixtureCop}}, cop("u1", "TASK"), cop("u2", "SECRET REWOUND"),
			{"type": "session.snapshot_rewind", "data": obj{"upToEventId": "u2", "eventsRemoved": 1}}, cop("u3", "TAIL"),
		}, []string{"user|TASK", "user|TAIL"}, transcript.LossInactiveBranch},
		{"gemini/compress-repeat-checkpoints", geminiTranscript{}, fixtureGemini, []obj{
			{"sessionId": fixtureGemini}, gem("u1", "SECRET OLD"),
			{"$set": obj{"messages": []obj{gem("s1", "<state_snapshot>SUMMARY OLD</state_snapshot>"), gem("u2", "SECRET MIDDLE")}}},
			{"$set": obj{"messages": []obj{gem("s2", "<state_snapshot>SUMMARY FINAL</state_snapshot>"), gem("u3", "TAIL")}}},
		}, []string{"user|SUMMARY:<state_snapshot>SUMMARY FINAL</state_snapshot>", "user|TAIL"}, transcript.LossPreCompaction},
		{"gemini/clear", geminiTranscript{}, fixtureGemini, []obj{
			{"sessionId": fixtureGemini}, gem("u1", "SECRET OLD"), {"$set": obj{"messages": []obj{}}}, gem("u2", "TAIL"),
		}, []string{"user|TAIL"}, transcript.LossClearedHistory},
		{"gemini/compress-patch", geminiTranscript{}, fixtureGemini, []obj{
			{"sessionId": fixtureGemini}, gem("u1", "SECRET OLD"), gem("u2", "TAIL"), gem("s", "<state_snapshot>SUMMARY FINAL</state_snapshot>"),
			{"$patch": obj{"removeIds": []string{"u1"}, "orderIds": []string{"s", "u2"}}},
		}, []string{"user|SUMMARY:<state_snapshot>SUMMARY FINAL</state_snapshot>", "user|TAIL"}, transcript.LossPreCompaction},
		{"gemini/rewind-edit", geminiTranscript{}, fixtureGemini, []obj{
			{"sessionId": fixtureGemini}, gem("u1", "TASK"), gem("u2", "SECRET REWOUND"), {"$rewindTo": "u2"}, gem("u3", "TAIL"),
		}, []string{"user|TASK", "user|TAIL"}, transcript.LossInactiveBranch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := make([]any, len(tc.records))
			for i := range tc.records {
				records[i] = tc.records[i]
			}
			tr := decodeFixture(t, tc.codec, tc.id, jsonl(t, records...))
			assert.Equal(t, tc.want, entryTexts(tr))
			assert.Positive(t, tr.Losses.Count(tc.loss))
		})
	}
}

func TestCopilotTruncationRequiresHandoff(t *testing.T) {
	raw := jsonl(t, obj{"type": "session.start", "data": obj{"sessionId": fixtureCop}}, obj{"type": "user.message", "data": obj{"content": "SECRET OLD"}}, obj{"type": "session.truncation", "data": obj{"messagesRemovedDuringTruncation": 1, "preTruncationMessagesLength": 3, "postTruncationMessagesLength": 2}}, obj{"type": "user.message", "data": obj{"content": "TAIL"}})
	_, err := (copilotTranscript{}).Decode(bytes.NewReader(raw), fixtureCop)
	require.ErrorContains(t, err, "truncation")
}
