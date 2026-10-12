package harness

import (
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
)

// piHistory transfers a pi session file as it is: the header, then the entry
// tree. An import remints the session id and rewrites the header's cwd to the
// receiver's; entry ids are scoped to their file and need no remint. The
// parent-session link names a file on the sender's machine and is dropped, and
// the header's start time becomes the import's.
// Settings, credentials, extensions and the agent directory are never part of
// a transfer.
type piHistory struct{}

var (
	_ HistoryTransfer          = piHistory{}
	_ StreamingHistoryTransfer = piHistory{}
)

func (piHistory) Format() string { return "pi-session-jsonl" }

// Open returns the session file for id. cwd locates a session directory that
// settings configure relative to the working directory.
func (piHistory) Open(id, cwd string) (io.ReadCloser, error) {
	path := piLocateSessionFile(id, cwd)
	if path == "" {
		return nil, os.ErrNotExist
	}
	return os.Open(path)
}

// piLocateSessionFile finds a session file by id across every session root,
// plus the directory a cwd-relative setting names.
func piLocateSessionFile(id, cwd string) string {
	roots := piSessionRoots()
	if cwd != "" {
		if dir, err := piSessionsDir(nil, cwd); err == nil {
			roots = append(roots, dir)
		}
	}
	for _, root := range roots {
		if path := piFindSessionFile(root, id); path != "" {
			return path
		}
	}
	return ""
}

func (h piHistory) Export(id, cwd string) ([]byte, error) {
	r, err := h.Open(id, cwd)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r, agentbundle.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > agentbundle.MaxBytes {
		return nil, errors.New("pi history exceeds federation.agent_transfer_max_bytes default (2 GiB)")
	}
	return raw, h.Validate(raw, id)
}

func (h piHistory) Validate(raw []byte, id string) error {
	return h.ValidateReader(bytes.NewReader(raw), id)
}

func (piHistory) ValidateReader(r io.Reader, id string) error {
	return rewritePiHistory(r, id, "", "", io.Discard)
}

// rewritePiHistory validates a session file whose header names source and,
// with a target, writes it out under the target id and cwd. Every record must
// be a JSON object with a type; the first is the header.
func rewritePiHistory(r io.Reader, source, target, cwd string, out io.Writer) error {
	if source == "" {
		return errors.New("pi history source identity is empty")
	}
	found := false
	err := scanHistoryRecords(r, func(line int, raw []byte) error {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rec); err != nil || rec == nil {
			return fmt.Errorf("invalid pi history record at line %d", line)
		}
		typ := geminiHistoryRawString(rec, "type")
		if typ == "" {
			return fmt.Errorf("pi history record at line %d has no type", line)
		}
		if !found {
			if typ != piSessionHeaderType || geminiHistoryRawString(rec, "id") != source {
				return errors.New("pi history has no matching session header")
			}
			var version int
			_ = json.Unmarshal(rec["version"], &version)
			if version > piSessionVersion {
				return fmt.Errorf("pi session version %d is newer than tclaude reads (%d)", version, piSessionVersion)
			}
			found = true
			if target != "" {
				geminiHistoryPutString(rec, "id", target)
				geminiHistoryPutString(rec, "cwd", cwd)
				// The session starts now, as pi's own /clone stamps it:
				// the extension counts spend from the header's time on,
				// so the copied history's spend stays with its source.
				geminiHistoryPutString(rec, "timestamp", rfc3339ms(time.Now()))
				delete(rec, "parentSession")
				var err error
				if raw, err = json.Marshal(rec); err != nil {
					return err
				}
			}
		} else if typ == piSessionHeaderType {
			return errors.New("pi history has a second session header")
		}
		if _, err := out.Write(raw); err != nil {
			return err
		}
		_, err := out.Write([]byte{'\n'})
		return err
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("pi history has no session header")
	}
	return nil
}

func (h piHistory) Import(raw []byte, source, cwd string) (string, func(), error) {
	return h.ImportReader(bytes.NewReader(raw), source, cwd)
}

// ImportReader lands a session in the store pi uses for cwd, under a new id,
// so that `pi --session-id <id>` started in cwd resumes it.
func (piHistory) ImportReader(r io.Reader, source, cwd string) (string, func(), error) {
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", nil, err
	}
	if !filepath.IsAbs(cwd) {
		return "", nil, errors.New("pi history requires an absolute receiver cwd")
	}
	dir, err := piNewSessionDir(nil, cwd)
	if err != nil {
		return "", nil, fmt.Errorf("cannot determine pi's session directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	id := uuid.NewString()
	path := filepath.Join(dir, piSessionFileName(time.Now(), id))
	cleanup := func() {
		_ = os.Remove(path)
		_ = db.DeleteConvIndex(id)
		_ = db.DeleteConvBranchHistory(id)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	err = rewritePiHistory(r, source, id, cwd, f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: id, ProjectDir: dir, FullPath: path, ProjectPath: cwd, Harness: PiName,
		FileSize: info.Size(), FileMtime: info.ModTime(),
	}); err != nil {
		cleanup()
		return "", nil, err
	}
	return id, cleanup, nil
}
