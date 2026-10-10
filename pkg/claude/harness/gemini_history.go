package harness

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Native session records only: no settings, OAuth, trust, policy, filesystem checkpoints
// or scratchpad files. Gemini's resume folds these JSONL records itself.
type geminiHistory struct{}

func (geminiHistory) Format() string { return "gemini-session-jsonl" }
func (geminiHistory) Open(id, cwd string) (io.ReadCloser, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, errors.New("invalid Gemini session identity")
	}
	path, found, err := locateGeminiHistoryFile(id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".jsonl") {
		return f, nil
	}
	// Legacy sessions are one JSON object. Emit metadata and each message
	// separately, rather than buffering the entire conversation.
	r, w := io.Pipe()
	go func() { defer f.Close(); err := streamLegacyGemini(f, w); _ = w.CloseWithError(err) }()
	return geminiLegacyReader{r, f}, nil
}

type geminiLegacyReader struct {
	*io.PipeReader
	source *os.File
}

func (r geminiLegacyReader) Close() error { _ = r.source.Close(); return r.PipeReader.Close() }
func (h geminiHistory) Export(id, cwd string) ([]byte, error) {
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
		return nil, errors.New("gemini history exceeds federation.agent_transfer_max_bytes default (2 GiB)")
	}
	return raw, h.Validate(raw, id)
}
func (h geminiHistory) Validate(raw []byte, id string) error {
	return h.ValidateReader(bytes.NewReader(raw), id)
}
func (h geminiHistory) ValidateReader(r io.Reader, id string) error {
	return rewriteGeminiHistory(r, id, "", "", io.Discard)
}

func geminiHistoryRecordLimit() int {
	if c, err := config.Load(); err == nil && c.Federation != nil && c.Federation.AgentHistoryRecordMaxBytes > 0 {
		return c.Federation.AgentHistoryRecordMaxBytes
	}
	return MaxHistoryRecordBytes
}

// Reset at each legacy field/message, limiting allocation before JSON decode.
type geminiRecordReader struct {
	io.Reader
	remaining int64
}

func (r *geminiRecordReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("gemini legacy record exceeds federation.agent_history_record_max_bytes")
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.Reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}
func streamLegacyGemini(r io.Reader, out io.Writer) error {
	// Field order in legacy JSON is not significant. Spool messages until all
	// metadata has been read, so even messages-first records stay portable.
	spool, err := os.CreateTemp("", "tclaude-gemini-legacy-*.jsonl")
	if err != nil {
		return err
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	bounded := &geminiRecordReader{Reader: r, remaining: int64(geminiHistoryRecordLimit())}
	dec := json.NewDecoder(bounded)
	enc := json.NewEncoder(spool)
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("invalid legacy Gemini session")
	}
	metadata := map[string]json.RawMessage{}
	for dec.More() {
		bounded.remaining = int64(geminiHistoryRecordLimit())
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("invalid legacy Gemini field")
		}
		if name == "messages" {
			tok, err = dec.Token()
			if err != nil || tok != json.Delim('[') {
				return errors.New("invalid legacy Gemini messages")
			}
			for dec.More() {
				bounded.remaining = int64(geminiHistoryRecordLimit())
				var msg json.RawMessage
				if err := dec.Decode(&msg); err != nil {
					return err
				}
				if err := enc.Encode(msg); err != nil {
					return err
				}
			}
			if _, err = dec.Token(); err != nil {
				return err
			}
		} else {
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				return err
			}
			metadata[name] = value
		}
	}
	if _, err = dec.Token(); err != nil {
		return err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing legacy Gemini session data")
	}
	if err = json.NewEncoder(out).Encode(metadata); err != nil {
		return err
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(out, spool)
	return err
}
func geminiProjectHash(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:])
}
func rewriteGeminiHistory(r io.Reader, source, target, cwd string, out io.Writer) error {
	if _, err := uuid.Parse(source); err != nil {
		return errors.New("gemini history source identity must be a UUID")
	}
	limit := MaxHistoryRecordBytes
	if b, ok := r.(interface{ HistoryRecordLimit() int }); ok && b.HistoryRecordLimit() > 0 {
		limit = b.HistoryRecordLimit()
	}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, min(64<<10, limit)), limit)
	enc := json.NewEncoder(out)
	found := false
	ids := map[string]string{}
	remint := func(id string) string {
		if id == "" || target == "" {
			return id
		}
		if v, ok := ids[id]; ok {
			return v
		}
		v := uuid.NewString()
		ids[id] = v
		return v
	}
	rewriteMessage := func(msg map[string]json.RawMessage) error {
		if geminiHistoryRawString(msg, "id") == "" {
			return errors.New("gemini message has no identity")
		}
		if target != "" {
			geminiHistoryPutString(msg, "id", remint(geminiHistoryRawString(msg, "id")))
		}
		return nil
	}
	rewriteMeta := func(meta map[string]json.RawMessage) error {
		if id := geminiHistoryRawString(meta, "sessionId"); id != "" && id != source {
			return errors.New("gemini history session identity mismatch")
		}
		if kind := geminiHistoryRawString(meta, "kind"); kind != "" && kind != "main" {
			return errors.New("gemini subagent history is not a main session")
		}
		if target != "" {
			if _, ok := meta["sessionId"]; ok {
				geminiHistoryPutString(meta, "sessionId", target)
			}
			if _, ok := meta["projectHash"]; ok {
				geminiHistoryPutString(meta, "projectHash", geminiProjectHash(cwd))
			}
		}
		if raw, ok := meta["messages"]; ok {
			var msgs []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &msgs); err != nil {
				return errors.New("invalid Gemini message checkpoint")
			}
			for _, msg := range msgs {
				if err := rewriteMessage(msg); err != nil {
					return err
				}
			}
			meta["messages"], _ = json.Marshal(msgs)
		}
		return nil
	}
	for scan.Scan() {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil || rec == nil {
			return errors.New("invalid Gemini history record")
		}
		if !found {
			if geminiHistoryRawString(rec, "sessionId") != source || geminiHistoryRawString(rec, "projectHash") == "" {
				return errors.New("gemini history has no matching session metadata")
			}
			if err := rewriteMeta(rec); err != nil {
				return err
			}
			found = true
		} else if geminiHistoryRawString(rec, "id") != "" {
			if err := rewriteMessage(rec); err != nil {
				return err
			}
		} else if _, ok := rec["$set"]; ok {
			var meta map[string]json.RawMessage
			if err := json.Unmarshal(rec["$set"], &meta); err != nil || meta == nil {
				return errors.New("invalid Gemini metadata update")
			}
			if err := rewriteMeta(meta); err != nil {
				return err
			}
			rec["$set"], _ = json.Marshal(meta)
		} else if geminiHistoryRawString(rec, "sessionId") != "" && geminiHistoryRawString(rec, "projectHash") != "" {
			if err := rewriteMeta(rec); err != nil {
				return err
			}
		} else if id := geminiHistoryRawString(rec, "$rewindTo"); id != "" {
			geminiHistoryPutString(rec, "$rewindTo", remint(id))
		} else {
			return errors.New("unsupported Gemini history record")
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("gemini history record exceeds federation.agent_history_record_max_bytes=%d or cannot be read: %w", limit, err)
	}
	if !found {
		return errors.New("gemini history has no session metadata")
	}
	return nil
}
func (h geminiHistory) Import(raw []byte, source, cwd string) (string, func(), error) {
	return h.ImportReader(bytes.NewReader(raw), source, cwd)
}
func (h geminiHistory) ImportReader(r io.Reader, source, cwd string) (string, func(), error) {
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", nil, err
	}
	if !filepath.IsAbs(cwd) {
		return "", nil, errors.New("gemini history requires an absolute receiver cwd")
	}
	id := uuid.NewString()
	roots := []string{geminiDir()}
	if runtime.GOOS == "darwin" {
		roots = append(roots, geminiSeatbeltRuntimeDir())
	}
	if roots[0] == "" {
		return "", nil, errors.New("cannot determine Gemini home")
	}
	var paths []string
	cleanup := func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
		if p, found, _ := locateGeminiHistoryFile(id); found {
			_ = os.Remove(p)
		}
		_ = db.DeleteConvIndex(id)
		_ = db.DeleteConvBranchHistory(id)
	}
	fail := func(e error) (string, func(), error) { cleanup(); return "", nil, e }
	for _, root := range roots {
		slug := geminiProjectHash(cwd)
		for candidate, path := range readGeminiProjectRegistry(root) {
			if path == cwd {
				slug = candidate
				break
			}
		}
		project := filepath.Join(root, geminiTmpDirName, slug)
		if err = os.MkdirAll(project, 0700); err != nil {
			return fail(err)
		}
		marker := filepath.Join(project, geminiProjectRootFile)
		if raw, e := os.ReadFile(marker); e == nil {
			if strings.TrimSpace(string(raw)) != cwd {
				return fail(errors.New("gemini project directory belongs to another cwd"))
			}
		} else if os.IsNotExist(e) {
			if err = os.WriteFile(marker, []byte(cwd), 0600); err != nil {
				return fail(err)
			}
		} else {
			return fail(e)
		}
		chats := filepath.Join(project, geminiChatsDirName)
		if err = os.MkdirAll(chats, 0700); err != nil {
			return fail(err)
		}
		path := filepath.Join(chats, geminiSessionPrefix+time.Now().UTC().Format("2006-01-02T15-04")+"-"+id[:8]+".jsonl")
		if len(paths) > 0 {
			if err = os.Link(paths[0], path); err != nil {
				from, e := os.Open(paths[0])
				if e != nil {
					return fail(e)
				}
				to, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if e != nil {
					_ = from.Close()
					return fail(e)
				}
				paths = append(paths, path)
				_, err = io.Copy(to, from)
				_ = from.Close()
				closeErr := to.Close()
				if err != nil {
					return fail(err)
				}
				if closeErr != nil {
					return fail(closeErr)
				}
				continue
			}
			paths = append(paths, path)
			continue
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return fail(e)
		}
		paths = append(paths, path)
		err = rewriteGeminiHistory(r, source, id, cwd, f)
		ce := f.Close()
		if err != nil {
			return fail(err)
		}
		if ce != nil {
			return fail(ce)
		}
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		return fail(err)
	}
	if err = db.UpsertConvIndex(&db.ConvIndexRow{ConvID: id, ProjectDir: filepath.Dir(filepath.Dir(paths[0])), FullPath: paths[0], ProjectPath: cwd, Harness: GeminiName, FileSize: info.Size(), FileMtime: info.ModTime()}); err != nil {
		return fail(err)
	}
	return id, cleanup, nil
}

func geminiHistoryRawString(m map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}
func geminiHistoryPutString(m map[string]json.RawMessage, key, value string) {
	m[key], _ = json.Marshal(value)
}

// Find identity from metadata only. The ordinary conversation locator folds
// all turns for UI summaries, which is inappropriate before transfer limits.
func locateGeminiHistoryFile(id string) (string, bool, error) {
	var best string
	var newest time.Time
	for _, root := range geminiRuntimeDirs() {
		candidates, err := filepath.Glob(filepath.Join(root, geminiTmpDirName, "*", geminiChatsDirName, geminiSessionPrefix+"*-"+id[:8]+".json*"))
		if err != nil {
			return "", false, err
		}
		for _, path := range candidates {
			if !geminiIsSessionFile(filepath.Base(path)) {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			metadata, err := geminiHistoryMetadata(bufio.NewReader(f))
			info, statErr := f.Stat()
			_ = f.Close()
			if err != nil || statErr != nil || metadata["sessionId"] != id {
				continue
			}
			// A migrated JSONL is authoritative over its legacy JSON source.
			jsonl, bestJSONL := strings.HasSuffix(path, ".jsonl"), strings.HasSuffix(best, ".jsonl")
			if best == "" || (jsonl && !bestJSONL) || (jsonl == bestJSONL && info.ModTime().After(newest)) {
				best, newest = path, info.ModTime()
			}
		}
	}
	return best, best != "", nil
}

// Legacy JSON permits messages before metadata. Skip arbitrary JSON values
// byte by byte, without allocating their contents, then read identity fields.
func geminiHistoryMetadata(r *bufio.Reader) (map[string]string, error) {
	metadata := map[string]string{}
	next := func() (byte, error) {
		for {
			b, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			if !strings.ContainsRune(" \t\r\n", rune(b)) {
				return b, nil
			}
		}
	}
	b, err := next()
	if err != nil || b != '{' {
		return nil, errors.New("invalid gemini metadata")
	}
	for {
		b, err = next()
		if err != nil {
			return nil, err
		}
		if b == '}' {
			return metadata, nil
		}
		if b != '"' {
			return nil, errors.New("invalid gemini metadata key")
		}
		_ = r.UnreadByte()
		keyRaw, err := geminiHistoryJSONValue(r, true)
		if err != nil {
			return nil, err
		}
		var key string
		if err = json.Unmarshal(keyRaw, &key); err != nil {
			return nil, err
		}
		b, err = next()
		if err != nil || b != ':' {
			return nil, errors.New("invalid gemini metadata field")
		}
		keep := key == "sessionId" || key == "projectHash"
		raw, err := geminiHistoryJSONValue(r, keep)
		if err != nil {
			return nil, err
		}
		if keep {
			var value string
			if err = json.Unmarshal(raw, &value); err != nil {
				return nil, err
			}
			metadata[key] = value
		}
		b, err = next()
		if err != nil {
			return nil, err
		}
		if b == '}' {
			return metadata, nil
		}
		if b != ',' {
			return nil, errors.New("invalid gemini metadata separator")
		}
	}
}
func geminiHistoryJSONValue(r *bufio.Reader, keep bool) ([]byte, error) {
	var raw []byte
	depth, started, quoted, escaped := 0, false, false, false
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if !started && strings.ContainsRune(" \t\r\n", rune(b)) {
			continue
		}
		if started && !quoted && depth == 0 && (b == ',' || b == '}' || strings.ContainsRune(" \t\r\n", rune(b))) {
			_ = r.UnreadByte()
			return raw, nil
		}
		started = true
		if keep {
			if len(raw) >= 64<<10 {
				return nil, errors.New("gemini metadata field too large")
			}
			raw = append(raw, b)
		}
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
				if depth == 0 {
					return raw, nil
				}
			}
		} else {
			switch b {
			case '"':
				quoted = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return raw, nil
				}
				if depth < 0 {
					return nil, errors.New("invalid gemini JSON value")
				}
			}
		}
	}
}
