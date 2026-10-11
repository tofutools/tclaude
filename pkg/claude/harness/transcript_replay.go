package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// Rewind events remove native records, including any compaction in the removed
// turn. Apply them before decoding so a discarded summary cannot become the
// active context. These are session-specific replay rules, not generic relaying.
func scanCodexTranscriptRecords(r io.Reader, losses *transcript.LossReport, visit func(int, []byte) error) error {
	store, err := newTranscriptReplayStore()
	if err != nil {
		return err
	}
	defer store.close()
	type record struct {
		line int
		ref  transcriptReplayRef
		user bool
	}
	var records []record
	turnStart := -1
	err = scanHistoryRecords(r, func(line int, raw []byte) error {
		var env codexEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("invalid Codex rollout JSON at line %d", line)
		}
		user := false
		if env.Type == "response_item" {
			var item codexItem
			if json.Unmarshal(env.Payload, &item) == nil && item.Type == "message" && item.Role == "user" {
				var content []codexContent
				_ = json.Unmarshal(item.Content, &content)
				for _, c := range content {
					if c.Type == "input_image" || c.Type == "input_text" && !codexHarnessText(c.Text) {
						user = true
						break
					}
				}
			}
		}
		if env.Type == "event_msg" {
			var event struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(env.Payload, &event)
			switch event.Type {
			case "task_started", "turn_started":
				turnStart = len(records)
			case "user_message":
				// Accepted input can precede its response_item, especially
				// when compaction interrupts the turn. Both identify the
				// same turn, whose compaction metadata must be discarded.
				if turnStart >= 0 && turnStart < len(records) {
					records[turnStart].user = true
				}
			}
		}
		if user && turnStart >= 0 && turnStart < len(records) {
			records[turnStart].user = true
			user = false
		}
		if env.Type == "event_msg" {
			var ev struct {
				Type  string `json:"type"`
				Turns int    `json:"num_turns"`
			}
			if json.Unmarshal(env.Payload, &ev) == nil && ev.Type == "thread_rolled_back" {
				if ev.Turns < 0 {
					return fmt.Errorf("invalid Codex rollback at line %d", line)
				}
				for ; ev.Turns > 0; ev.Turns-- {
					at := len(records) - 1
					for at >= 0 && !records[at].user {
						at--
					}
					if at < 0 {
						return fmt.Errorf("codex rollback has no recorded user boundary at line %d; use the agent-written handoff", line)
					}
					losses.Add(transcript.LossInactiveBranch, len(records)-at, "codex rollback records")
					records = records[:at]
				}
				turnStart = -1
				return nil
			}
		}
		ref, err := store.append(raw)
		if err != nil {
			return err
		}
		records = append(records, record{line, ref, user})
		return nil
	})
	if err != nil {
		return err
	}
	for _, rec := range records {
		raw, err := store.read(rec.ref)
		if err != nil {
			return err
		}
		if err := visit(rec.line, raw); err != nil {
			return err
		}
	}
	return nil
}

func scanCopilotTranscriptRecords(r io.Reader, losses *transcript.LossReport, visit func(int, []byte) error) error {
	store, err := newTranscriptReplayStore()
	if err != nil {
		return err
	}
	defer store.close()
	type record struct {
		line    int
		ref     transcriptReplayRef
		id      string
		context bool
	}
	var records []record
	err = scanHistoryRecords(r, func(line int, raw []byte) error {
		var ev struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			AgentID string          `json:"agentId"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("invalid Copilot event at line %d", line)
		}
		if ev.AgentID != "" {
			losses.Add(transcript.LossSidechain, 1, "")
			return nil
		}
		if ev.Type == "session.snapshot_rewind" {
			var d struct {
				Target string   `json:"upToEventId"`
				IDs    []string `json:"eventIds"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil || d.Target == "" {
				return fmt.Errorf("invalid Copilot rewind at line %d", line)
			}
			remove := map[string]bool{}
			for _, id := range d.IDs {
				remove[id] = true
			}
			found := false
			kept := records[:0]
			for _, rec := range records {
				if rec.id == d.Target {
					found = true
				}
				drop := remove[rec.id]
				if d.IDs == nil {
					drop = found
				}
				if drop {
					if rec.context {
						losses.Add(transcript.LossInactiveBranch, 1, "")
					}
					continue
				}
				kept = append(kept, rec)
			}
			if !found && d.IDs == nil {
				return fmt.Errorf("copilot rewind target is absent at line %d; use the agent-written handoff", line)
			}
			records = kept
			return nil
		}
		context := ev.Type == "user.message" || ev.Type == "assistant.message" || ev.Type == "tool.execution_complete"
		ref, err := store.append(raw)
		if err != nil {
			return err
		}
		records = append(records, record{line, ref, ev.ID, context})
		return nil
	})
	if err != nil {
		return err
	}
	for _, rec := range records {
		raw, err := store.read(rec.ref)
		if err != nil {
			return err
		}
		if err := visit(rec.line, raw); err != nil {
			return err
		}
	}
	return nil
}

// Keep only offsets in memory: long-lived rollouts can contain hundreds of MiB
// of tool output. The private spool is removed on success and on every error.
type transcriptReplayRef struct {
	offset int64
	size   int
}
type transcriptReplayStore struct {
	file   *os.File
	offset int64
}

func newTranscriptReplayStore() (*transcriptReplayStore, error) {
	f, err := os.CreateTemp("", "tclaude-transcript-replay-*")
	if err != nil {
		return nil, fmt.Errorf("create transcript replay spool: %w", err)
	}
	return &transcriptReplayStore{file: f}, nil
}
func (s *transcriptReplayStore) close() { _ = s.file.Close(); _ = os.Remove(s.file.Name()) }
func (s *transcriptReplayStore) append(raw []byte) (transcriptReplayRef, error) {
	ref := transcriptReplayRef{s.offset, len(raw)}
	n, err := s.file.Write(raw)
	s.offset += int64(n)
	if err != nil {
		return ref, fmt.Errorf("write transcript replay spool: %w", err)
	}
	if n != len(raw) {
		return ref, io.ErrShortWrite
	}
	return ref, nil
}
func (s *transcriptReplayStore) read(ref transcriptReplayRef) ([]byte, error) {
	raw := make([]byte, ref.size)
	_, err := s.file.ReadAt(raw, ref.offset)
	if err != nil {
		return nil, fmt.Errorf("read transcript replay spool: %w", err)
	}
	return raw, nil
}
