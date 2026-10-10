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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"gopkg.in/yaml.v3"
)

// Native resume rebuilds conversation state from events.jsonl. Transfer no
// database, checkpoint executable artifacts, rewind files or permissions.
type copilotHistory struct{}

func (copilotHistory) Format() string { return "copilot-events-jsonl" }
func (copilotHistory) Open(id, cwd string) (io.ReadCloser, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, errors.New("invalid Copilot session identity")
	}
	return os.Open(filepath.Join(copilotHome(), copilotSessionStateDirName, id, copilotEventsFileName))
}
func (h copilotHistory) Export(id, cwd string) ([]byte, error) {
	r, err := h.Open(id, cwd)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, agentbundle.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > agentbundle.MaxBytes {
		return nil, errors.New("Copilot history exceeds federation.agent_transfer_max_bytes default (2 GiB)")
	}
	return raw, h.Validate(raw, id)
}
func (h copilotHistory) Validate(raw []byte, id string) error {
	return h.ValidateReader(bytes.NewReader(raw), id)
}
func (h copilotHistory) ValidateReader(r io.Reader, id string) error {
	return rewriteCopilotHistory(r, id, "", "", io.Discard)
}
func rewriteCopilotHistory(r io.Reader, source, target, cwd string, out io.Writer) error {
	if _, err := uuid.Parse(source); err != nil {
		return errors.New("Copilot history source identity must be a UUID")
	}
	limit := MaxHistoryRecordBytes
	if bound, ok := r.(interface{ HistoryRecordLimit() int }); ok && bound.HistoryRecordLimit() > 0 {
		limit = bound.HistoryRecordLimit()
	}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, min(64<<10, limit)), limit)
	found := false
	ids := map[string]string{}
	remint := func(id string) string {
		if id == "" {
			return id
		}
		if v, ok := ids[id]; ok {
			return v
		}
		v := uuid.NewString()
		ids[id] = v
		return v
	}
	encoder := json.NewEncoder(out)
	for scan.Scan() {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal(scan.Bytes(), &event); err != nil || event == nil {
			return errors.New("invalid Copilot history event")
		}
		kind := rawString(event, "type")
		if kind == "session.start" || kind == "session.resume" {
			var data map[string]json.RawMessage
			if err := json.Unmarshal(event["data"], &data); err != nil || data == nil {
				return errors.New("invalid Copilot session metadata")
			}
			if rawString(data, "sessionId") != source {
				return errors.New("Copilot history session identity mismatch")
			}
			if kind == "session.start" {
				if found {
					return errors.New("duplicate Copilot session.start")
				}
				found = true
			}
			if target != "" {
				putRawString(data, "sessionId", target)
				data["context"], _ = json.Marshal(map[string]string{"cwd": cwd})
				event["data"], _ = json.Marshal(data)
			}
		}
		if target != "" {
			if id := rawString(event, "id"); id != "" {
				putRawString(event, "id", remint(id))
			}
			if parent := rawString(event, "parentId"); parent != "" {
				putRawString(event, "parentId", remint(parent))
			}
		}
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("Copilot history record exceeds federation.agent_history_record_max_bytes=%d or cannot be read: %w", limit, err)
	}
	if !found {
		return errors.New("Copilot history has no matching session.start")
	}
	return nil
}
func (h copilotHistory) Import(raw []byte, source, cwd string) (string, func(), error) {
	return h.ImportReader(bytes.NewReader(raw), source, cwd)
}
func (h copilotHistory) ImportReader(r io.Reader, source, cwd string) (string, func(), error) {
	id := uuid.NewString()
	root := filepath.Join(copilotHome(), copilotSessionStateDirName)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir); _ = db.DeleteConvIndex(id); _ = db.DeleteConvBranchHistory(id) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }
	path := filepath.Join(dir, copilotEventsFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(err)
	}
	err = rewriteCopilotHistory(r, source, id, cwd, f)
	ce := f.Close()
	if err != nil {
		return fail(err)
	}
	if ce != nil {
		return fail(ce)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	metadata, err := yaml.Marshal(copilotWorkspace{ID: id, Cwd: cwd, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return fail(err)
	}
	if err = os.WriteFile(filepath.Join(dir, copilotWorkspaceFileName), metadata, 0600); err != nil {
		return fail(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	if err = db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: dir, FullPath: path, ProjectPath: cwd, Harness: CopilotName, FileSize: info.Size(), FileMtime: info.ModTime(), Created: now}); err != nil {
		return fail(err)
	}
	return id, cleanup, nil
}
