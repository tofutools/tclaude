# Native reset and branch fixtures

`TestTranscriptResetMatrix` builds minimal native JSONL fixtures. Prose and IDs
are invented sentinels; no private transcripts, credentials, model calls or
provider access are needed. Existing compaction fixtures also cover Claude's
preserved segment and parallel tool results, plus Codex encrypted replacement
history. These are schema-derived fixtures, not claimed real model captures.

The replay rules were checked against these primary sources:

- Claude's JSONL parent chain and `compact_boundary`/`preservedSegment` shape:
  the existing sanitized partial-compaction fixture in
  `transcript_compaction_test.go`. `/clear` starts a fresh root/session; a
  sidechain is not the main conversation.
- [Codex rollout reconstruction](https://github.com/openai/codex/blob/main/codex-rs/core/src/session/rollout_reconstruction.rs)
  and [protocol](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs):
  `compacted.replacement_history` replaces history; `thread_rolled_back.num_turns`
  removes the latest real user turns, including compactions inside those turns.
- [OpenCode v1.2.27 message replay](https://github.com/anomalyco/opencode/blob/v1.2.27/packages/opencode/src/session/message-v2.ts)
  and [revert cleanup](https://github.com/anomalyco/opencode/blob/v1.2.27/packages/opencode/src/session/revert.ts):
  only completed, successful summaries reset context; a pending revert excludes
  the indicated message/part and everything after it. Fork/new-session exports
  are separate session projections.
- [Copilot SDK event schema](https://github.com/github/copilot-sdk/blob/main/nodejs/src/generated/session-events.ts):
  successful compaction, `session.context_cleared` (optional initial message),
  snapshot rewind (inclusive boundary or explicit removed IDs), and sub-agent
  events. Snapshot rewinds normally rewrite the durable log and emit an
  ephemeral notification; the decoder also handles a recorded notification.
  Truncation exposes only counts, not retained message identities: decoding
  refuses rather than guessing, allowing morph's existing handoff rung.
- [Gemini recording/replay](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/services/chatRecordingService.ts)
  and [compression](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/context/chatCompressionService.ts):
  checkpoints replace messages; `$rewindTo` is inclusive; `$patch` updates,
  removes and reorders messages. Compression introduces a user
  `<state_snapshot>` summary, followed by preserved recent history.
  The metadata `summary` is a title, not a compaction summary.

Auto/manual/multiple compactions share replay records. Trigger annotations do
not change the resulting context. Clear/new-session coverage follows each
harness's native storage behavior, not a made-up common clear event.
