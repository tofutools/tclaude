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
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// HistoryTransfer is deliberately narrower than cloning. Only a reviewed
// transcript format may be installed and resumed; ancillary native files,
// databases, hooks, credentials and artifacts never travel with it.
type HistoryTransfer interface {
	Format() string
	Export(convID, cwd string) ([]byte, error)
	Validate(raw []byte, sourceID string) error
	// Import remints identity and remaps known cwd metadata. Message/tool text
	// is untouched. Cleanup removes only the freshly created transcript/index.
	Import(raw []byte, sourceID, cwd string) (convID string, cleanup func(), err error)
}

// StreamingHistoryTransfer keeps large transcripts out of whole-conversation
// buffers. The caller snapshots/limits Open's native file before transferring.
type StreamingHistoryTransfer interface {
	Open(convID, cwd string) (io.ReadCloser, error)
	ValidateReader(io.Reader, string) error
	ImportReader(io.Reader, string, string) (string, func(), error)
}

// A single record may contain images or large tool output. This is a record
// memory bound rather than a history-size bound; no record is truncated.
const MaxHistoryRecordBytes = 256 << 20

func (h *Harness) SupportsHistoryTransfer() bool { return h != nil && h.History != nil }

type jsonlHistory struct{ harness string }

func (j jsonlHistory) Format() string {
	if j.harness == CodexName {
		return "codex-rollout-jsonl"
	}
	return "claude-jsonl"
}
func (j jsonlHistory) Open(convID, cwd string) (io.ReadCloser, error) {
	var path string
	row, err := db.GetConvIndex(convID)
	if err != nil {
		return nil, err
	}
	if row != nil && row.Harness == j.harness {
		path = row.FullPath
	}
	if path == "" {
		h, _ := Resolve(j.harness)
		entries, err := h.Convs.ListConvs("")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.SessionID == convID {
				path = e.FullPath
				break
			}
		}
	}
	if path == "" {
		return nil, errors.New("conversation transcript was not found")
	}
	var reader io.ReadCloser
	if j.harness == CodexName {
		reader, err = openCodexRollout(path)
	} else {
		reader, err = os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	return reader, nil
}
func (j jsonlHistory) Export(convID, cwd string) ([]byte, error) {
	reader, err := j.Open(convID, cwd)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, agentbundle.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > agentbundle.MaxBytes {
		return nil, errors.New("transcript exceeds federation.agent_transfer_max_bytes default (2 GiB)")
	}
	if err := j.Validate(raw, convID); err != nil {
		return nil, err
	}
	return raw, nil
}

// rewriteHistory only replaces documented native identity/path fields at
// record boundaries. It never searches/replaces UUIDs or paths in free text.
func (j jsonlHistory) rewriteHistoryReader(reader io.Reader, sourceID, newID, cwd string, out io.Writer) error {
	if _, err := uuid.Parse(sourceID); err != nil {
		return errors.New("history source identity must be a UUID")
	}
	scanner := bufio.NewScanner(reader)
	recordLimit := MaxHistoryRecordBytes
	if bounded, ok := reader.(interface{ HistoryRecordLimit() int }); ok && bounded.HistoryRecordLimit() > 0 {
		recordLimit = bounded.HistoryRecordLimit()
	}
	scanner.Buffer(make([]byte, min(64<<10, recordLimit)), recordLimit)
	found := false
	line := 0
	for scanner.Scan() {
		line++
		record := scanner.Bytes()
		if len(bytes.TrimSpace(record)) == 0 {
			if _, err := out.Write(record); err != nil {
				return err
			}
			if _, err := out.Write([]byte{'\n'}); err != nil {
				return err
			}
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(record, &obj); err != nil || obj == nil {
			return fmt.Errorf("invalid history JSON object at line %d", line)
		}
		changed := false
		set := func(m map[string]json.RawMessage, key, value string) { m[key], _ = json.Marshal(value); changed = true }
		if j.harness == CodexName {
			var kind string
			_ = json.Unmarshal(obj["type"], &kind)
			if kind == "session_meta" || kind == "turn_context" {
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(obj["payload"], &payload); err != nil || payload == nil {
					return fmt.Errorf("invalid %s at line %d", kind, line)
				}
				if kind == "session_meta" {
					var id string
					_ = json.Unmarshal(payload["id"], &id)
					if id != sourceID || found {
						return errors.New("history session_meta identity mismatch or duplicate")
					}
					found = true
					if newID != "" {
						set(payload, "id", newID)
					}
				}
				if newID != "" {
					set(payload, "cwd", cwd)
					obj["payload"], _ = json.Marshal(payload)
				}
			}
		} else {
			var id string
			_ = json.Unmarshal(obj["sessionId"], &id)
			if id != "" {
				if id != sourceID {
					return fmt.Errorf("history sessionId mismatch at line %d", line)
				}
				found = true
				if newID != "" {
					set(obj, "sessionId", newID)
				}
			}
			if newID != "" {
				if _, ok := obj["cwd"]; ok {
					set(obj, "cwd", cwd)
				}
			}
		}
		if changed {
			record, _ = json.Marshal(obj)
		}
		if _, err := out.Write(record); err != nil {
			return err
		}
		if _, err := out.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("history record exceeds federation.agent_history_record_max_bytes=%d or cannot be read: %w", recordLimit, err)
	}
	if !found {
		return errors.New("history has no matching native session identity")
	}
	return nil
}
func (j jsonlHistory) rewriteHistory(raw []byte, sourceID, newID, cwd string) ([]byte, error) {
	var out bytes.Buffer
	if err := j.rewriteHistoryReader(bytes.NewReader(raw), sourceID, newID, cwd, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func (j jsonlHistory) ValidateReader(r io.Reader, sourceID string) error {
	return j.rewriteHistoryReader(r, sourceID, "", "", io.Discard)
}
func (j jsonlHistory) Validate(raw []byte, sourceID string) error {
	_, err := j.rewriteHistory(raw, sourceID, "", "")
	return err
}
func (j jsonlHistory) Import(raw []byte, sourceID, cwd string) (string, func(), error) {
	return j.ImportReader(bytes.NewReader(raw), sourceID, cwd)
}
func (j jsonlHistory) ImportReader(reader io.Reader, sourceID, cwd string) (string, func(), error) {
	id := uuid.NewString()
	var path string
	if j.harness == CodexName {
		root, err := codexConfigDir()
		if err != nil {
			return "", nil, err
		}
		now := time.Now().UTC()
		path = filepath.Join(root, "sessions", now.Format("2006/01/02"), "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	} else {
		path = filepath.Join(convops.GetClaudeProjectPath(cwd), id+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(path); _ = db.DeleteConvIndex(id); _ = db.DeleteConvBranchHistory(id) }
	writeErr := j.rewriteHistoryReader(reader, sourceID, id, cwd, file)
	closeErr := file.Close()
	if writeErr != nil {
		cleanup()
		return "", nil, writeErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	if j.harness == DefaultName {
		convops.ScanAndUpsertFile(path)
		if row, err := db.GetConvIndex(id); err != nil || row == nil {
			cleanup()
			return "", nil, errors.New("could not index imported Claude transcript")
		}
		if err := db.SetConvIndexProjectPath(id, cwd); err != nil {
			cleanup()
			return "", nil, err
		}
	} else {
		info, err := os.Stat(path)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		if err := db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: filepath.Dir(path), FullPath: path, FileMtime: info.ModTime(), FileSize: info.Size(), ProjectPath: cwd, Harness: j.harness, Created: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return id, cleanup, nil
}
