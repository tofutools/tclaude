package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Transfer the supported CLI projection, never OpenCode's private SQLite DB.
// The wire representation is record-wise so tclaude does not buffer a session.
type openCodeHistory struct{ environment []string }

// WithEnvironment scopes native I/O to a daemon-validated private XDG store.
func (h openCodeHistory) WithEnvironment(env []string) HistoryTransfer {
	h.environment = append([]string(nil), env...)
	return h
}

func (openCodeHistory) Format() string { return "opencode-session-jsonl" }

func (h openCodeHistory) command(cwd string, stdout io.Writer, args ...string) error {
	executable, err := OpenCodeExecutable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 72*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, append(args, "--pure", "--log-level", "ERROR")...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), h.environment...)
	cmd.Stdout = stdout
	// Native diagnostic output must not grow with the transcript.
	var detail bytes.Buffer
	cmd.Stderr = &limitedOpenCodeDiagnostic{buffer: &detail}
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("OpenCode %s: %w: %s", args[0], err, detail.String())
	}
	return nil
}

type limitedOpenCodeDiagnostic struct{ buffer *bytes.Buffer }

func (w *limitedOpenCodeDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - w.buffer.Len(); remaining > 0 {
		_, _ = w.buffer.Write(p[:min(len(p), remaining)])
	}
	return n, nil
}

type openCodeHistoryFile struct{ *os.File }

func (f *openCodeHistoryFile) Close() error {
	err := f.File.Close()
	_ = os.Remove(f.Name())
	return err
}

func (h openCodeHistory) Open(id, cwd string) (io.ReadCloser, error) {
	if !validOpenCodeHistoryID(id, "ses_") {
		return nil, errors.New("invalid OpenCode session identity")
	}
	native, err := os.CreateTemp("", "tclaude-opencode-export-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(native.Name())
	defer native.Close()
	if err = h.command(cwd, native, "export", id); err != nil {
		return nil, err
	}
	if _, err = native.Seek(0, 0); err != nil {
		return nil, err
	}
	wire, err := os.CreateTemp("", "tclaude-opencode-history-*.jsonl")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) { _ = wire.Close(); _ = os.Remove(wire.Name()); return nil, err }
	decoder := json.NewDecoder(native)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fail(errors.New("invalid OpenCode export object"))
	}
	encoder := json.NewEncoder(wire)
	infoFound, messagesFound := false, false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return fail(err)
		}
		switch key {
		case "info":
			if infoFound || messagesFound {
				return fail(errors.New("invalid OpenCode export order"))
			}
			infoFound = true
			var info map[string]json.RawMessage
			if err = decoder.Decode(&info); err != nil {
				return fail(err)
			}
			if err = encoder.Encode(map[string]any{"info": info}); err != nil {
				return fail(err)
			}
		case "messages":
			if !infoFound || messagesFound {
				return fail(errors.New("invalid OpenCode export messages"))
			}
			messagesFound = true
			token, err = decoder.Token()
			if err != nil || token != json.Delim('[') {
				return fail(errors.New("invalid OpenCode messages array"))
			}
			for decoder.More() {
				var message json.RawMessage
				if err = decoder.Decode(&message); err != nil {
					return fail(err)
				}
				if err = encoder.Encode(map[string]any{"message": message}); err != nil {
					return fail(err)
				}
			}
			if _, err = decoder.Token(); err != nil {
				return fail(err)
			}
		default:
			return fail(fmt.Errorf("unknown OpenCode export field %v", key))
		}
	}
	if _, err = decoder.Token(); err != nil {
		return fail(err)
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return fail(errors.New("trailing OpenCode export data"))
	}
	if !infoFound || !messagesFound {
		return fail(errors.New("incomplete OpenCode export"))
	}
	if _, err = wire.Seek(0, 0); err != nil {
		return fail(err)
	}
	return &openCodeHistoryFile{wire}, nil
}
func (h openCodeHistory) Export(id, cwd string) ([]byte, error) {
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
		return nil, errors.New("OpenCode history exceeds federation.agent_transfer_max_bytes")
	}
	return raw, h.Validate(raw, id)
}
func (h openCodeHistory) Validate(raw []byte, id string) error {
	return h.ValidateReader(bytes.NewReader(raw), id)
}
func (h openCodeHistory) ValidateReader(r io.Reader, id string) error {
	return rewriteOpenCodeHistory(r, id, "", "", io.Discard)
}
func validOpenCodeHistoryID(id, prefix string) bool {
	if len(id) <= len(prefix) || len(id) > 160 || id[:len(prefix)] != prefix {
		return false
	}
	for _, c := range id[len(prefix):] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func openCodeID(prefix string) string { return prefix + uuid.NewString() }
func rawString(m map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}
func putRawString(m map[string]json.RawMessage, key, value string) { m[key], _ = json.Marshal(value) }

// Remint all globally keyed session/message/part identities, including parent
// references. Only native metadata paths change; text and tool output stay intact.
func rewriteOpenCodeHistory(r io.Reader, source, newID, cwd string, out io.Writer) error {
	if !validOpenCodeHistoryID(source, "ses_") {
		return errors.New("invalid OpenCode history source identity")
	}
	limit := MaxHistoryRecordBytes
	if bounded, ok := r.(interface{ HistoryRecordLimit() int }); ok && bounded.HistoryRecordLimit() > 0 {
		limit = bounded.HistoryRecordLimit()
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, min(64<<10, limit)), limit)
	ids := map[string]string{}
	seen := map[string]bool{}
	seenParts := map[string]bool{}
	header := false
	first := true
	remint := func(id, prefix string) string {
		if newID == "" {
			return id
		}
		if value := ids[id]; value != "" {
			return value
		}
		value := openCodeID(prefix)
		ids[id] = value
		return value
	}
	for scanner.Scan() {
		var record map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || len(record) != 1 {
			return errors.New("invalid OpenCode history record")
		}
		if data, ok := record["info"]; ok {
			if header {
				return errors.New("duplicate OpenCode session header")
			}
			header = true
			var info map[string]json.RawMessage
			if err := json.Unmarshal(data, &info); err != nil || rawString(info, "id") != source {
				return errors.New("OpenCode session identity mismatch")
			}
			if newID != "" {
				putRawString(info, "id", newID)
				putRawString(info, "directory", cwd)
				delete(info, "parentID")
				delete(info, "permission")
				delete(info, "share")
				delete(info, "revert")
				delete(info, "projectID")
				delete(info, "path")
			}
			raw, err := json.Marshal(info)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(out, "{\"info\":%s,\"messages\":[", raw); err != nil {
				return err
			}
			continue
		}
		if !header {
			return errors.New("OpenCode history lacks session header")
		}
		data, ok := record["message"]
		if !ok {
			return errors.New("unknown OpenCode history record")
		}
		var message struct {
			Info  map[string]json.RawMessage   `json:"info"`
			Parts []map[string]json.RawMessage `json:"parts"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			return err
		}
		id := rawString(message.Info, "id")
		if !validOpenCodeHistoryID(id, "msg_") || seen[id] || rawString(message.Info, "sessionID") != source {
			return errors.New("OpenCode message identity mismatch or duplicate")
		}
		seen[id] = true
		parent := rawString(message.Info, "parentID")
		if parent != "" && !validOpenCodeHistoryID(parent, "msg_") {
			return errors.New("invalid OpenCode parent identity")
		}
		if newID != "" {
			putRawString(message.Info, "id", remint(id, "msg_"))
			putRawString(message.Info, "sessionID", newID)
			if parent != "" {
				putRawString(message.Info, "parentID", remint(parent, "msg_"))
			}
			if raw := message.Info["path"]; raw != nil {
				var path map[string]json.RawMessage
				if err := json.Unmarshal(raw, &path); err != nil {
					return err
				}
				putRawString(path, "cwd", cwd)
				putRawString(path, "root", cwd)
				message.Info["path"], _ = json.Marshal(path)
			}
		}
		for _, part := range message.Parts {
			pid := rawString(part, "id")
			if !validOpenCodeHistoryID(pid, "prt_") || seenParts[pid] || rawString(part, "sessionID") != source || rawString(part, "messageID") != id {
				return errors.New("OpenCode part identity mismatch or duplicate")
			}
			seenParts[pid] = true
			if newID != "" {
				putRawString(part, "id", remint(pid, "prt_"))
				putRawString(part, "sessionID", newID)
				putRawString(part, "messageID", remint(id, "msg_"))
			}
		}
		raw, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if !first {
			if _, err = io.WriteString(out, ","); err != nil {
				return err
			}
		}
		first = false
		if _, err = out.Write(raw); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("OpenCode history record exceeds federation.agent_history_record_max_bytes=%d or cannot be read: %w", limit, err)
	}
	if !header {
		return errors.New("OpenCode history lacks session header")
	}
	_, err := io.WriteString(out, "]}")
	return err
}
func (h openCodeHistory) Import(raw []byte, source, cwd string) (string, func(), error) {
	return h.ImportReader(bytes.NewReader(raw), source, cwd)
}
func (h openCodeHistory) ImportReader(r io.Reader, source, cwd string) (string, func(), error) {
	file, err := os.CreateTemp("", "tclaude-opencode-import-*.json")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	id := openCodeID("ses_")
	if err = rewriteOpenCodeHistory(r, source, id, cwd, file); err != nil {
		return "", nil, err
	}
	if err = file.Close(); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = h.command(cwd, io.Discard, "session", "delete", id)
		_ = db.DeleteConvIndex(id)
		_ = db.DeleteConvBranchHistory(id)
	}
	if err = h.command(cwd, io.Discard, "import", file.Name()); err != nil {
		cleanup()
		return "", nil, err
	}
	var listing bytes.Buffer
	err = h.command(cwd, &listing, "session", "list", "--format", "json")
	if err != nil {
		cleanup()
		return "", nil, err
	}
	sessions, err := parseOpenCodeSessions(listing.Bytes())
	if err != nil {
		cleanup()
		return "", nil, err
	}
	for _, session := range sessions {
		if session.ID == id {
			if err = db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: cwd, ProjectPath: cwd, Harness: OpenCodeName, Created: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				cleanup()
				return "", nil, err
			}
			return id, cleanup, nil
		}
	}
	cleanup()
	return "", nil, errors.New("OpenCode did not persist the imported session")
}
