package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// The same small conversation in each native format: a secret word, one
// tool call with output, an image the user attached, and harness noise that
// must not cross (system/developer context, sidechains, abandoned branches).
var fixturePNG = []byte("\x89PNG\r\n\x1a\nmorph-fixture")

const (
	fixtureSecret = "Remember the secret word: PELICAN."
	fixtureOutput = "README.md\nmain.go"
	fixtureAnswer = "There are two files."
	fixtureImageQ = "What is in this image?"
	fixtureImageA = "A tiny PNG."
	fixtureClaude = "0f0e0d0c-0b0a-4908-8706-050403020100"
	fixtureCodex  = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	fixtureCop    = "3c1f0b8e-5d2a-4e8b-9f61-2a7c0d4e5b6a"
	fixtureGemini = "7d2e4f6a-8b9c-4d0e-a1f2-3b4c5d6e7f80"
	fixtureOC     = "ses_fixturesource"
	fixturePi     = "8e3f5a7b-9c0d-4e1f-a2b3-4c5d6e7f8091"
)

func jsonl(t *testing.T, records ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range records {
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

type obj = map[string]any

func claudeFixture(t *testing.T) []byte {
	png := base64.StdEncoding.EncodeToString(fixturePNG)
	rec := func(typ, id string, parent any, msg obj, extra ...obj) obj {
		r := obj{"type": typ, "uuid": id, "parentUuid": parent, "sessionId": fixtureClaude, "cwd": "/src", "timestamp": "2026-10-01T10:00:00.000Z", "message": msg}
		for _, e := range extra {
			for k, v := range e {
				r[k] = v
			}
		}
		return r
	}
	asst := func(id string, content ...obj) obj {
		return obj{"id": id, "role": "assistant", "model": "claude-opus-5-5", "content": content}
	}
	return jsonl(t,
		obj{"type": "summary", "summary": "old", "leafUuid": "x"},
		rec("user", "u1", nil, obj{"role": "user", "content": fixtureSecret}),
		obj{"type": "attachment", "uuid": "at1", "parentUuid": "u1", "sessionId": fixtureClaude, "attachment": obj{"type": "todo"}},
		rec("assistant", "a2", "at1", asst("m1", obj{"type": "thinking", "thinking": "private", "signature": "sig"})),
		rec("assistant", "a3", "a2", asst("m1", obj{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": obj{"command": "ls"}})),
		rec("user", "u4", "a3", obj{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": "toolu_1", "content": fixtureOutput}}}),
		rec("assistant", "a5", "u4", asst("m2", obj{"type": "text", "text": fixtureAnswer})),
		rec("user", "b1", "a5", obj{"role": "user", "content": "abandoned prompt"}),
		rec("user", "u6", "a5", obj{"role": "user", "content": []obj{{"type": "text", "text": fixtureImageQ}, {"type": "image", "source": obj{"type": "base64", "media_type": "image/png", "data": png}}}}),
		rec("user", "s1", "u6", obj{"role": "user", "content": "sidechain"}, obj{"isSidechain": true}),
		rec("user", "u7", "u6", obj{"role": "user", "content": "<local-command-caveat>Caveat</local-command-caveat>"}, obj{"isMeta": true}),
		rec("assistant", "a8", "u7", asst("m3", obj{"type": "text", "text": fixtureImageA})),
		obj{"type": "custom-title", "customTitle": "Secret demo", "sessionId": fixtureClaude},
	)
}

func codexFixture(t *testing.T) []byte {
	ri := func(p obj) obj {
		return obj{"timestamp": "2026-10-01T10:00:00.000Z", "type": "response_item", "payload": p}
	}
	msg := func(role string, content ...obj) obj {
		return ri(obj{"type": "message", "role": role, "content": content})
	}
	return jsonl(t,
		obj{"timestamp": "2026-10-01T10:00:00.000Z", "type": "session_meta", "payload": obj{"id": fixtureCodex, "cwd": "/src", "timestamp": "2026-10-01T10:00:00.000Z", "cli_version": "0.139.0", "originator": "codex-tui"}},
		obj{"timestamp": "2026-10-01T10:00:00.000Z", "type": "turn_context", "payload": obj{"cwd": "/src", "model": "gpt-5.5"}},
		msg("developer", obj{"type": "input_text", "text": "<permissions instructions>"}),
		msg("user", obj{"type": "input_text", "text": "<environment_context>\n<cwd>/src</cwd>\n</environment_context>"}),
		msg("user", obj{"type": "input_text", "text": fixtureSecret}),
		obj{"timestamp": "2026-10-01T10:00:00.000Z", "type": "event_msg", "payload": obj{"type": "user_message", "message": fixtureSecret}},
		ri(obj{"type": "reasoning", "summary": []obj{}, "encrypted_content": "gAAAA"}),
		ri(obj{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`, "call_id": "call_1"}),
		ri(obj{"type": "function_call_output", "call_id": "call_1", "output": fixtureOutput}),
		ri(obj{"type": "custom_tool_call", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch", "call_id": "call_2"}),
		ri(obj{"type": "custom_tool_call_output", "call_id": "call_2", "output": []obj{{"type": "input_text", "text": "Success"}}}),
		msg("assistant", obj{"type": "output_text", "text": fixtureAnswer}),
		msg("user", obj{"type": "input_text", "text": fixtureImageQ}, obj{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixturePNG)}),
		msg("assistant", obj{"type": "output_text", "text": fixtureImageA}),
		obj{"timestamp": "2026-10-01T10:00:00.000Z", "type": "event_msg", "payload": obj{"type": "token_count"}},
	)
}

func openCodeFixture(t *testing.T) []byte {
	n := 0
	part := func(msg string, p obj) obj {
		n++
		p["id"] = fmt.Sprintf("prt_fixture%02d", n)
		p["sessionID"], p["messageID"] = fixtureOC, msg
		return p
	}
	user := func(id string, parts ...obj) obj {
		return obj{"message": obj{"info": obj{"id": id, "sessionID": fixtureOC, "role": "user", "time": obj{"created": 1790000000000}}, "parts": parts}}
	}
	asst := func(id, parent string, parts ...obj) obj {
		return obj{"message": obj{"info": obj{"id": id, "sessionID": fixtureOC, "role": "assistant", "parentID": parent, "modelID": "claude-sonnet-5-5", "providerID": "anthropic", "time": obj{"created": 1790000000001}}, "parts": parts}}
	}
	return jsonl(t,
		obj{"info": obj{"id": fixtureOC, "directory": "/src", "title": "Secret demo"}},
		user("msg_fixture01", part("msg_fixture01", obj{"type": "text", "text": fixtureSecret}), part("msg_fixture01", obj{"type": "text", "text": "<system-reminder>", "synthetic": true})),
		asst("msg_fixture02", "msg_fixture01",
			part("msg_fixture02", obj{"type": "step-start"}),
			part("msg_fixture02", obj{"type": "reasoning", "text": "private"}),
			part("msg_fixture02", obj{"type": "tool", "callID": "tc1", "tool": "bash", "state": obj{"status": "completed", "input": obj{"command": "ls"}, "output": fixtureOutput}}),
			part("msg_fixture02", obj{"type": "text", "text": fixtureAnswer}),
			part("msg_fixture02", obj{"type": "step-finish"})),
		user("msg_fixture03", part("msg_fixture03", obj{"type": "text", "text": fixtureImageQ}), part("msg_fixture03", obj{"type": "file", "mime": "image/png", "filename": "shot.png", "url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixturePNG)})),
		asst("msg_fixture04", "msg_fixture03", part("msg_fixture04", obj{"type": "text", "text": fixtureImageA})),
	)
}

func copilotFixture(t *testing.T) []byte {
	ev := func(typ string, data obj) obj {
		return obj{"type": typ, "data": data, "timestamp": "2026-10-01T10:00:00.000Z"}
	}
	return jsonl(t,
		ev("session.start", obj{"sessionId": fixtureCop, "version": 1, "producer": "copilot-agent", "copilotVersion": "1.0.95", "startTime": "2026-10-01T10:00:00.000Z", "selectedModel": "gpt-5.5", "context": obj{"cwd": "/src"}}),
		ev("system.message", obj{"role": "system", "content": "You are Copilot"}),
		ev("user.message", obj{"content": fixtureSecret, "transformedContent": "<current_datetime>x</current_datetime>" + fixtureSecret}),
		ev("assistant.turn_start", obj{"turnId": "0"}),
		ev("assistant.message", obj{"messageId": "m1", "content": "", "reasoningText": "private", "reasoningOpaque": "opaque", "toolRequests": []obj{{"toolCallId": "tc1", "name": "bash", "arguments": obj{"command": "ls"}}}}),
		ev("tool.execution_start", obj{"toolCallId": "tc1", "toolName": "bash"}),
		ev("tool.execution_complete", obj{"toolCallId": "tc1", "success": true, "result": obj{"content": fixtureOutput}}),
		ev("assistant.message", obj{"messageId": "m2", "content": fixtureAnswer}),
		ev("assistant.turn_end", obj{"turnId": "0"}),
		ev("user.message", obj{"content": fixtureImageQ, "attachments": []obj{{"type": "blob", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(fixturePNG)}}}),
		ev("assistant.message", obj{"messageId": "m3", "content": fixtureImageA}),
		ev("session.shutdown", obj{}),
	)
}

func geminiFixture(t *testing.T) []byte {
	return jsonl(t,
		obj{"sessionId": fixtureGemini, "projectHash": "abc", "startTime": "2026-10-01T10:00:00.000Z", "lastUpdated": "2026-10-01T10:00:00.000Z", "kind": "main"},
		obj{"id": "u1", "timestamp": "2026-10-01T10:00:00.000Z", "type": "user", "content": []obj{{"text": fixtureSecret}}},
		obj{"id": "g1", "timestamp": "2026-10-01T10:00:01.000Z", "type": "gemini", "content": "", "model": "gemini-3-pro",
			"thoughts":  []obj{{"subject": "Plan", "description": "private"}},
			"toolCalls": []obj{{"id": "c1", "name": "run_shell_command", "args": obj{"command": "ls"}, "status": "success", "result": []obj{{"functionResponse": obj{"id": "c1", "name": "run_shell_command", "response": obj{"output": fixtureOutput}}}}}}},
		obj{"id": "g2", "timestamp": "2026-10-01T10:00:02.000Z", "type": "gemini", "content": fixtureAnswer},
		obj{"id": "u9", "timestamp": "2026-10-01T10:00:03.000Z", "type": "user", "content": "abandoned prompt"},
		obj{"$rewindTo": "u9"},
		obj{"id": "u2", "timestamp": "2026-10-01T10:00:04.000Z", "type": "user", "content": []obj{{"text": fixtureImageQ}, {"inlineData": obj{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(fixturePNG)}}}},
		obj{"id": "i1", "timestamp": "2026-10-01T10:00:05.000Z", "type": "info", "content": "Chat history compressed."},
		obj{"id": "g3", "timestamp": "2026-10-01T10:00:06.000Z", "type": "gemini", "content": fixtureImageA},
		obj{"$set": obj{"summary": "Secret demo"}},
	)
}

func piFixture(t *testing.T) []byte {
	entry := func(typ, id string, parent any, fields obj) obj {
		r := obj{"type": typ, "id": id, "parentId": parent, "timestamp": "2026-10-01T10:00:00.000Z"}
		for k, v := range fields {
			r[k] = v
		}
		return r
	}
	msg := func(id string, parent any, m obj) obj { return entry("message", id, parent, obj{"message": m}) }
	asst := func(stop string, content ...obj) obj {
		return obj{"role": "assistant", "content": content, "provider": "anthropic", "model": "claude-opus-5-5", "stopReason": stop,
			"usage": obj{"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 15, "cost": obj{"total": 0.001}}}
	}
	return jsonl(t,
		obj{"type": "session", "version": 3, "id": fixturePi, "timestamp": "2026-10-01T10:00:00.000Z", "cwd": "/src"},
		entry("model_change", "m0", nil, obj{"provider": "anthropic", "modelId": "claude-opus-5-5"}),
		msg("s0", "m0", obj{"role": "system", "content": "You are pi", "sections": obj{"cwd": "<cwd>/src</cwd>"}}),
		msg("u1", "s0", obj{"role": "user", "content": []obj{{"type": "text", "text": fixtureSecret}}}),
		msg("a2", "u1", asst("toolUse", obj{"type": "thinking", "thinking": "private", "thinkingSignature": "sig"},
			obj{"type": "toolCall", "id": "call_1", "name": "bash", "arguments": obj{"command": "ls"}})),
		msg("r3", "a2", obj{"role": "toolResult", "toolCallId": "call_1", "toolName": "bash", "content": []obj{{"type": "text", "text": fixtureOutput}}, "isError": false}),
		msg("a4", "r3", asst("stop", obj{"type": "text", "text": fixtureAnswer})),
		msg("b1", "a4", obj{"role": "user", "content": "abandoned prompt"}),
		msg("u5", "a4", obj{"role": "user", "content": []obj{{"type": "text", "text": fixtureImageQ}, {"type": "image", "data": base64.StdEncoding.EncodeToString(fixturePNG), "mimeType": "image/png"}}}),
		msg("a6", "u5", asst("stop", obj{"type": "text", "text": fixtureImageA})),
		entry("session_info", "n7", "a6", obj{"name": "Secret demo"}),
	)
}

type nativeFixture struct {
	harness string
	id      string
	raw     func(*testing.T) []byte
	// expected decode losses beyond reasoning
	losses map[transcript.LossKind]int
}

var nativeFixtures = []nativeFixture{
	{DefaultName, fixtureClaude, claudeFixture, map[transcript.LossKind]int{transcript.LossHarnessContext: 2, transcript.LossInactiveBranch: 1, transcript.LossSidechain: 1}},
	{CodexName, fixtureCodex, codexFixture, map[transcript.LossKind]int{transcript.LossHarnessContext: 2}},
	{OpenCodeName, fixtureOC, openCodeFixture, map[transcript.LossKind]int{transcript.LossHarnessContext: 1}},
	{CopilotName, fixtureCop, copilotFixture, map[transcript.LossKind]int{transcript.LossHarnessContext: 1}},
	{GeminiName, fixtureGemini, geminiFixture, map[transcript.LossKind]int{transcript.LossHarnessContext: 1, transcript.LossInactiveBranch: 1}},
	{PiName, fixturePi, piFixture, map[transcript.LossKind]int{transcript.LossHarnessContext: 1, transcript.LossInactiveBranch: 1}},
}

func mustHarness(t *testing.T, name string) *Harness {
	t.Helper()
	h, err := Resolve(name)
	require.NoError(t, err)
	require.NotNil(t, h.Transcript, name)
	return h
}

func allText(tr *transcript.Transcript) string {
	var b strings.Builder
	for _, e := range tr.Entries {
		for _, blk := range e.Blocks {
			b.WriteString(blk.Text + "\n")
			b.WriteString(string(blk.Input) + "\n")
		}
	}
	return b.String()
}

func mediaData(tr *transcript.Transcript) [][]byte {
	var out [][]byte
	for _, e := range tr.Entries {
		for _, blk := range e.Blocks {
			for _, m := range blk.Media {
				out = append(out, m.Data)
			}
		}
	}
	return out
}

// TestTranscriptDecodeEachNativeFormat pins what each decoder carries and
// what it reports as left behind.
func TestTranscriptDecodeEachNativeFormat(t *testing.T) {
	for _, fx := range nativeFixtures {
		t.Run(fx.harness, func(t *testing.T) {
			h := mustHarness(t, fx.harness)
			raw := fx.raw(t)
			require.NoError(t, h.History.Validate(raw, fx.id), "fixture is a valid native history")
			tr, err := h.Transcript.Decode(bytes.NewReader(raw), fx.id)
			require.NoError(t, err)
			assert.Equal(t, fx.harness, tr.Source.Harness)
			if fx.harness != GeminiName { // Gemini records only a project hash; the caller supplies cwd
				assert.Equal(t, "/src", tr.Source.Cwd)
			}

			kinds := map[transcript.BlockKind]int{}
			calls, results := map[string]string{}, map[string]string{}
			for _, e := range tr.Entries {
				for _, b := range e.Blocks {
					kinds[b.Kind]++
					if b.Kind == transcript.BlockToolCall {
						calls[b.CallID] = b.Name
					}
					if b.Kind == transcript.BlockToolResult {
						results[b.CallID] = b.Text
					}
				}
			}
			first := tr.Entries[0]
			assert.Equal(t, transcript.RoleUser, first.Role)
			assert.Equal(t, fixtureSecret, first.Blocks[0].Text, "harness context ahead of the first prompt is not carried")
			assert.Contains(t, allText(tr), `"ls"`)
			assert.Contains(t, results, firstKey(calls), "the tool result pairs with its call")
			assert.Equal(t, fixtureOutput, results[firstKey(calls)])
			assert.Equal(t, [][]byte{fixturePNG}, mediaData(tr), "the attached image is carried byte for byte")
			last := tr.Entries[len(tr.Entries)-1]
			assert.Equal(t, transcript.RoleAssistant, last.Role)
			assert.Equal(t, fixtureImageA, last.Blocks[len(last.Blocks)-1].Text)
			assert.NotContains(t, allText(tr), "abandoned")
			assert.NotContains(t, allText(tr), "sidechain")
			assert.Positive(t, kinds[transcript.BlockThinking], "reasoning is decoded (and dropped later by policy)")
			for kind, n := range fx.losses {
				assert.Equal(t, n, tr.Losses.Count(kind), "loss %s", kind)
			}
			assert.Zero(t, tr.Losses.Count(transcript.LossUnknownRecord), tr.Losses.Lines())
		})
	}
}

func firstKey(m map[string]string) string {
	for k := range m {
		if k != "call_2" {
			return k
		}
	}
	return ""
}

// TestTranscriptMatrixRoundTrip translates every fixture into every
// harness: the encoded history must pass the destination's own importer
// validation and decode back with the conversation intact.
func TestTranscriptMatrixRoundTrip(t *testing.T) {
	for _, fx := range nativeFixtures {
		for _, target := range nativeFixtures {
			dstName := target.harness
			dst := mustHarness(t, dstName)
			t.Run(fx.harness+"->"+dstName, func(t *testing.T) {
				src := mustHarness(t, fx.harness)
				tr, err := src.Transcript.Decode(bytes.NewReader(fx.raw(t)), fx.id)
				require.NoError(t, err)
				mediaDir := t.TempDir()
				plan := transcript.NewPlan(tr, transcript.Options{Target: dstName, TargetModel: "anthropic/claude-sonnet-5-5", InlineMedia: dst.Transcript.InlineMedia(), MediaDir: mediaDir})
				var buf bytes.Buffer
				sid, err := dst.Transcript.Encode(plan, "/dst", &buf)
				require.NoError(t, err)
				require.NoError(t, dst.History.Validate(buf.Bytes(), sid), "translated history passes %s import validation", dstName)
				back, err := dst.Transcript.Decode(bytes.NewReader(buf.Bytes()), sid)
				require.NoError(t, err)
				require.NotEmpty(t, back.Entries)

				text := allText(back)
				for _, want := range []string{fixtureSecret, fixtureOutput, fixtureAnswer, fixtureImageQ, fixtureImageA, "⟦tool call · ", "⟦tool result · ", "tclaude morph", "carried over from " + fx.harness} {
					assert.Contains(t, text, want)
				}
				assert.NotContains(t, text, "private", "reasoning does not cross harnesses")
				assert.Zero(t, back.Losses.Count(transcript.LossUnknownRecord), back.Losses.Lines())
				assert.Zero(t, back.Losses.Count(transcript.LossHarnessContext), "encoded records are not mistaken for harness context")
				if dst.Transcript.InlineMedia() {
					assert.Equal(t, [][]byte{fixturePNG}, mediaData(back), "image survives inline")
				} else {
					assert.Contains(t, text, mediaDir, "image is referenced by its saved path")
					assert.Equal(t, 1, plan.Report.Count(transcript.LossMediaExternalized))
				}
				assert.Equal(t, transcript.RoleUser, back.Entries[0].Role)
				for i := 1; i < len(back.Entries); i++ {
					assert.NotEqual(t, back.Entries[i-1].Role, back.Entries[i].Role, "roles alternate after a round trip")
				}
				assert.Positive(t, plan.Report.Count(transcript.LossReasoningDropped))
				assert.Positive(t, plan.Report.Count(transcript.LossToolCallsFlattened))
				assert.Zero(t, plan.Report.Count(transcript.LossInterruptedCall))

				// A second hop is stable: re-planning the decoded result
				// carries the same conversation text.
				again := transcript.NewPlan(back, transcript.Options{Target: fx.harness, InlineMedia: true})
				assert.Contains(t, planTextAll(again), fixtureSecret)
			})
		}
	}
}

func planTextAll(p *transcript.Plan) string {
	var b strings.Builder
	for _, e := range p.Entries {
		b.WriteString(planText(e) + "\n")
	}
	return b.String()
}
