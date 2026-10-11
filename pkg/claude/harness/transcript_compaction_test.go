package harness

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

func decodeFixture(t *testing.T, c TranscriptCodec, id string, raw []byte) *transcript.Transcript {
	t.Helper()
	tr, err := c.Decode(bytes.NewReader(raw), id)
	require.NoError(t, err)
	return tr
}

func entryTexts(tr *transcript.Transcript) []string {
	var out []string
	for _, e := range tr.Entries {
		var parts []string
		for _, b := range e.Blocks {
			switch b.Kind {
			case transcript.BlockSummary:
				parts = append(parts, "SUMMARY:"+b.Text)
			case transcript.BlockToolCall:
				parts = append(parts, "CALL:"+b.CallID)
			case transcript.BlockToolResult:
				parts = append(parts, "RESULT:"+b.CallID)
			case transcript.BlockMedia:
				parts = append(parts, "MEDIA")
			case transcript.BlockText:
				parts = append(parts, b.Text)
			}
		}
		out = append(out, string(e.Role)+"|"+strings.Join(parts, ","))
	}
	return out
}

// Real Claude Code partial compaction: the boundary has no parent, the
// summary is transcript-only (but model context), and the messages kept
// verbatim live before the boundary, anchored after the summary.
func TestClaudeDecodeCompactionPreservedSegmentAndParallelResults(t *testing.T) {
	const id = fixtureClaude
	rec := func(typ, uuid string, parent any, msg obj, extra ...obj) obj {
		r := obj{"type": typ, "uuid": uuid, "parentUuid": parent, "sessionId": id, "cwd": "/src", "message": msg}
		for _, e := range extra {
			for k, v := range e {
				r[k] = v
			}
		}
		return r
	}
	asst := func(content ...obj) obj { return obj{"id": "m", "role": "assistant", "content": content} }
	raw := jsonl(t,
		rec("user", "old1", nil, obj{"role": "user", "content": "old prompt"}),
		rec("assistant", "old2", "old1", asst(obj{"type": "text", "text": "old answer"})),
		rec("user", "keep1", "old2", obj{"role": "user", "content": "kept prompt"}),
		rec("assistant", "keep2", "keep1", asst(obj{"type": "text", "text": "kept answer"})),
		obj{"type": "system", "subtype": "compact_boundary", "uuid": "bnd", "parentUuid": nil, "logicalParentUuid": "keep2", "sessionId": id,
			"compactMetadata": obj{"preservedSegment": obj{"headUuid": "keep1", "anchorUuid": "sum", "tailUuid": "tail"},
				"preservedMessages": obj{"anchorUuid": "sum", "uuids": []string{"keep1", "keep2", "tail"}}}},
		obj{"type": "attachment", "uuid": "att", "parentUuid": "bnd", "sessionId": id},
		rec("user", "sum", "att", obj{"role": "user", "content": "This session is being continued. SUMMARY-TEXT"}, obj{"isCompactSummary": true, "isVisibleInTranscriptOnly": true}),
		obj{"type": "attachment", "uuid": "tail", "parentUuid": "sum", "sessionId": id},
		rec("user", "p1", "tail", obj{"role": "user", "content": "run two things"}),
		// Parallel calls: the second call's parent is the first call, and the
		// first result hangs off the first call on a side branch.
		rec("assistant", "c1", "p1", asst(obj{"type": "tool_use", "id": "t1", "name": "Bash", "input": obj{"command": "a"}})),
		rec("assistant", "c2", "c1", asst(obj{"type": "tool_use", "id": "t2", "name": "Bash", "input": obj{"command": "b"}})),
		rec("user", "r1", "c1", obj{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": "t1", "content": "A"}}}),
		rec("user", "r2", "c2", obj{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": "t2", "content": "B"}}}),
		rec("assistant", "done", "r2", asst(obj{"type": "text", "text": "both ran"})),
	)
	tr := decodeFixture(t, claudeTranscript{}, id, raw)
	assert.Equal(t, []string{
		"user|SUMMARY:This session is being continued. SUMMARY-TEXT",
		"user|kept prompt",
		"assistant|kept answer",
		"user|run two things",
		"assistant|CALL:t1,CALL:t2",
		"user|RESULT:t1,RESULT:t2",
		"assistant|both ran",
	}, entryTexts(tr))
	assert.Equal(t, 2, tr.Losses.Count(transcript.LossPreCompaction))
	assert.Equal(t, 2, tr.Losses.Count(transcript.LossHarnessContext), "only the attachments on the active chain")
	p := transcript.NewPlan(tr, transcript.Options{Target: CodexName})
	assert.Zero(t, p.Report.Count(transcript.LossInterruptedCall), "both parallel results pair with their calls")
}

func TestCodexDecodeCompactions(t *testing.T) {
	meta := obj{"type": "session_meta", "payload": obj{"id": fixtureCodex, "cwd": "/src"}}
	msg := func(role, kind, text string) obj {
		return obj{"type": "message", "role": role, "content": []obj{{"type": kind, "text": text}}}
	}
	t.Run("local summary is not duplicated", func(t *testing.T) {
		raw := jsonl(t, meta,
			obj{"type": "response_item", "payload": msg("user", "input_text", "old prompt")},
			obj{"type": "compacted", "payload": obj{"message": "SUMMARY-TEXT", "replacement_history": []obj{
				msg("developer", "input_text", "<permissions instructions>"),
				msg("user", "input_text", "kept prompt"),
				msg("user", "input_text", "Another model summarised this work:\nSUMMARY-TEXT"),
			}}},
			obj{"type": "response_item", "payload": msg("user", "input_text", "next prompt")},
		)
		tr := decodeFixture(t, codexTranscript{}, fixtureCodex, raw)
		assert.Equal(t, []string{"user|kept prompt", "user|SUMMARY:Another model summarised this work:\nSUMMARY-TEXT", "user|next prompt"}, entryTexts(tr))
		assert.Equal(t, 1, tr.Losses.Count(transcript.LossPreCompaction))
	})
	t.Run("remote summary is encrypted", func(t *testing.T) {
		raw := jsonl(t, meta,
			obj{"type": "response_item", "payload": msg("user", "input_text", "old prompt")},
			obj{"type": "compacted", "payload": obj{"message": "", "replacement_history": []obj{
				msg("user", "input_text", "kept prompt"),
				{"type": "compaction", "encrypted_content": "gAAAA"},
			}}},
		)
		tr := decodeFixture(t, codexTranscript{}, fixtureCodex, raw)
		assert.Equal(t, []string{"user|kept prompt"}, entryTexts(tr))
		assert.Equal(t, 1, tr.Losses.Count(transcript.LossEncryptedSummary))
	})
	t.Run("legacy summary only", func(t *testing.T) {
		raw := jsonl(t, meta,
			obj{"type": "response_item", "payload": msg("user", "input_text", "old prompt")},
			obj{"type": "compacted", "payload": obj{"message": "SUMMARY-TEXT"}},
		)
		assert.Equal(t, []string{"user|SUMMARY:SUMMARY-TEXT"}, entryTexts(decodeFixture(t, codexTranscript{}, fixtureCodex, raw)))
	})
}

func TestGeminiDecodeEdgeRecords(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(fixturePNG)
	raw := jsonl(t,
		obj{"sessionId": fixtureGemini, "projectHash": "abc"},
		obj{"id": "dropped", "type": "user", "content": "before checkpoint"},
		obj{"$set": obj{"messages": []obj{{"id": "u1", "type": "user", "content": "from checkpoint"}}}},
		obj{"id": "u1", "type": "user", "content": "from checkpoint, edited in place"},
		obj{"id": "cmd", "type": "user", "content": "/help"},
		obj{"id": "q", "type": "user", "content": "?"},
		obj{"id": "path", "type": "user", "content": "/src/main.go fails to build"},
		obj{"id": "img", "type": "user", "content": []obj{{"inlineData": obj{"mimeType": "image/png", "data": png}}}},
		obj{"id": "g1", "type": "gemini", "content": "ok"},
	)
	tr := decodeFixture(t, geminiTranscript{}, fixtureGemini, raw)
	assert.Equal(t, []string{"user|from checkpoint, edited in place", "user|/src/main.go fails to build", "user|MEDIA", "assistant|ok"}, entryTexts(tr))
	assert.Equal(t, 2, tr.Losses.Count(transcript.LossHarnessContext), "/help and ? are gemini commands")
}

func TestOpenCodeAndCopilotDecodeCompaction(t *testing.T) {
	oc := jsonl(t,
		obj{"info": obj{"id": fixtureOC, "directory": "/src"}},
		obj{"message": obj{"info": obj{"id": "msg_a1", "sessionID": fixtureOC, "role": "user"}, "parts": []obj{{"id": "prt_a1", "sessionID": fixtureOC, "messageID": "msg_a1", "type": "text", "text": "old"}}}},
		obj{"message": obj{"info": obj{"id": "msg_a2", "sessionID": fixtureOC, "role": "user"}, "parts": []obj{{"id": "prt_a2", "sessionID": fixtureOC, "messageID": "msg_a2", "type": "compaction"}}}},
		obj{"message": obj{"info": obj{"id": "msg_a3", "sessionID": fixtureOC, "role": "assistant", "summary": true}, "parts": []obj{{"id": "prt_a3", "sessionID": fixtureOC, "messageID": "msg_a3", "type": "text", "text": "SUMMARY-TEXT"}}}},
		obj{"message": obj{"info": obj{"id": "msg_a4", "sessionID": fixtureOC, "role": "user"}, "parts": []obj{{"id": "prt_a4", "sessionID": fixtureOC, "messageID": "msg_a4", "type": "text", "text": "next"}}}},
	)
	assert.Equal(t, []string{"user|SUMMARY:SUMMARY-TEXT", "user|next"}, entryTexts(decodeFixture(t, openCodeTranscript{}, fixtureOC, oc)))

	cop := jsonl(t,
		obj{"type": "session.start", "data": obj{"sessionId": fixtureCop}},
		obj{"type": "user.message", "data": obj{"content": "old"}},
		obj{"type": "session.compaction_complete", "data": obj{"success": false, "summaryContent": "FAILED"}},
		obj{"type": "session.compaction_complete", "data": obj{"success": true, "summaryContent": "SUMMARY-TEXT"}},
		obj{"type": "user.message", "data": obj{"content": "next"}},
	)
	assert.Equal(t, []string{"user|SUMMARY:SUMMARY-TEXT", "user|next"}, entryTexts(decodeFixture(t, copilotTranscript{}, fixtureCop, cop)))
}

func TestPlanInlinesOnlyAcceptedUserImages(t *testing.T) {
	tr := transcript.New(transcript.Source{Harness: OpenCodeName})
	svg := transcript.Media{MediaType: "image/svg+xml", Data: []byte("<svg/>")}
	png := transcript.Media{MediaType: "image/png", Data: fixturePNG}
	tr.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{{Kind: transcript.BlockMedia, Media: []transcript.Media{svg, png}}}})
	tr.Append(transcript.Entry{Role: transcript.RoleAssistant, Origin: transcript.OriginAgent, Blocks: []transcript.Block{{Kind: transcript.BlockMedia, Media: []transcript.Media{png}}}})
	p := transcript.NewPlan(tr, transcript.Options{Target: DefaultName, InlineMedia: true, MediaDir: t.TempDir()})
	assert.Equal(t, 2, p.Report.Count(transcript.LossMediaExternalized), "the svg and the assistant-side image are saved, not inlined")
	var buf bytes.Buffer
	sid, err := claudeTranscript{}.Encode(p, "/dst", &buf)
	require.NoError(t, err)
	require.NoError(t, jsonlHistory{harness: DefaultName}.Validate(buf.Bytes(), sid))
	assert.Equal(t, 1, strings.Count(buf.String(), `"type":"image"`))
	assert.NotContains(t, buf.String(), `"content":null`)
}

func TestOpenCodeEncodeNeedsQualifiedModel(t *testing.T) {
	tr := transcript.New(transcript.Source{Harness: DefaultName})
	tr.Append(transcript.Entry{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text("hi")}})
	_, err := openCodeTranscript{}.Encode(transcript.NewPlan(tr, transcript.Options{Target: OpenCodeName, TargetModel: "sonnet"}), "/dst", &bytes.Buffer{})
	assert.ErrorContains(t, err, "provider/model")
}
