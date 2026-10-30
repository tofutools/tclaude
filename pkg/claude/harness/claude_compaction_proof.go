package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// CompactionProof is how a harness whose compaction hooks do not say which
// thread compacted lets a caller find out afterwards. The evidence only exists
// once the hooks have returned, so it cannot be checked from the hooks.
type CompactionProof interface {
	// MainTranscriptPath locates the main thread's transcript.
	MainTranscriptPath(convID, cwd string) string
	// CompactedAfter reports whether the main thread recorded a compaction
	// past offset. An error means the transcript could not answer.
	CompactedAfter(path string, offset int64) (bool, error)
}

// claudeCompactionProof reads Claude Code's main transcript. A subagent's
// compaction fires PreCompact, SessionStart(compact) and PostCompact with the
// main session_id, transcript_path and no agent_id (observed on 2.1.314), but
// writes its compact_boundary to its own subagents/agent-<id>.jsonl. Only a
// main-thread compaction appends one here, just after those hooks return.
type claudeCompactionProof struct{}

func (claudeCompactionProof) MainTranscriptPath(convID, cwd string) string {
	return filepath.Join(convops.GetClaudeProjectPath(cwd), convID+".jsonl")
}

func (claudeCompactionProof) CompactedAfter(path string, offset int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < offset {
		return false, fmt.Errorf("transcript %s shrank below offset %d", path, offset)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if bytes.Contains(line, []byte("compact_boundary")) {
			var rec struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "system" && rec.Subtype == "compact_boundary" {
				return true, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
