package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Codex stores each conversation as a date-indexed rollout `.jsonl` under
// ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl — and, once a
// session ages out of the hot set, the same file gzip-style compressed to
// `.jsonl.zst`. Unlike Claude Code (one cwd-indexed `.jsonl` carrying
// title/cwd/branch), Codex keeps the durable metadata in a sidecar state
// DB (see codex_state.go); the rollout is the per-turn event log. The
// read path treats the rollout as source-of-truth + fallback and the
// state DB as enrichment, so a session with no DB row still assembles
// fully from its rollout head.

// maxCodexRolloutLineBytes caps a single rollout line. Codex lines can be
// large (the base-instructions and sandbox-policy snapshots run to several
// KB), so the default 64 KiB scanner token size is too small; 10 MiB
// matches the Claude Code parser's ceiling.
const maxCodexRolloutLineBytes = 10 * 1024 * 1024

// codexEnvelope is one rollout line: every line is the same
// {timestamp,type,payload} shape, with `type` selecting how to read the
// raw payload (verified against Codex CLI v0.139 — see testharness.CodexSim).
type codexEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexSessionMeta is the `session_meta` payload, written once at the head
// of every rollout. It carries the session id and the cwd the read path
// keys on (Codex is date-indexed, so cwd lives inside the file).
type codexSessionMeta struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

// codexEventMsg is an `event_msg` payload; only `user_message` carries the
// first prompt the read path harvests (payload.type ∈ {task_started,
// user_message, agent_message, token_count, task_complete, …}).
type codexEventMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// codexTurnContext is a per-turn `turn_context` snapshot; the read path
// uses it as the model fallback (and cwd fallback) when there is no
// threads-DB row.
type codexTurnContext struct {
	Model string `json:"model"`
	Cwd   string `json:"cwd"`
}

// codexRollout is the subset of a rollout's head the read path harvests —
// enough to assemble a convops.SessionEntry without the threads DB.
type codexRollout struct {
	SessionID    string // session_meta.id (also the filename uuid)
	Cwd          string // session_meta.cwd, falling back to turn_context.cwd
	Created      string // session_meta.timestamp (UTC, RFC3339 with ms)
	FirstUserMsg string // first event_msg user_message .message
	Model        string // first turn_context .model
}

// parseCodexRolloutHead reads just enough of a rollout (transparently
// decompressing `.zst`) to fill a codexRollout: the session_meta line, the
// first user message, and the first model. It short-circuits once all the
// head fields are known, so a large rollout is not read to EOF. A
// malformed line is skipped rather than failing the whole parse; only an
// I/O / scanner error is returned.
func parseCodexRolloutHead(path string) (*codexRollout, error) {
	rc, err := openCodexRollout(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	out := &codexRollout{}
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCodexRolloutLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env codexEnvelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case "session_meta":
			var m codexSessionMeta
			if json.Unmarshal(env.Payload, &m) == nil {
				if out.SessionID == "" {
					out.SessionID = m.ID
				}
				if out.Cwd == "" {
					out.Cwd = m.Cwd
				}
				if out.Created == "" {
					out.Created = m.Timestamp
				}
			}
		case "event_msg":
			if out.FirstUserMsg != "" {
				continue
			}
			var e codexEventMsg
			if json.Unmarshal(env.Payload, &e) == nil && e.Type == "user_message" && e.Message != "" {
				out.FirstUserMsg = e.Message
			}
		case "turn_context":
			var tc codexTurnContext
			if json.Unmarshal(env.Payload, &tc) == nil {
				if out.Model == "" {
					out.Model = tc.Model
				}
				if out.Cwd == "" {
					out.Cwd = tc.Cwd
				}
			}
		}
		// The head is fully harvested once we have the id, a cwd, the
		// first prompt and a model — everything a SessionEntry needs.
		// Stop here so we don't stream a multi-MB rollout to EOF.
		if out.SessionID != "" && out.Cwd != "" && out.FirstUserMsg != "" && out.Model != "" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan codex rollout %s: %w", path, err)
	}
	return out, nil
}

// openCodexRollout opens a rollout for reading, transparently wrapping a
// `.jsonl.zst` file in a streaming zstd decoder. The returned ReadCloser
// closes both the decoder and the underlying file.
func openCodexRollout(path string) (io.ReadCloser, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from our own ~/.codex scan
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".zst") {
		zr, err := zstd.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &zstdReadCloser{zr: zr, f: f}, nil
	}
	return f, nil
}

// zstdReadCloser couples a zstd decoder to its backing file so a single
// Close releases both.
type zstdReadCloser struct {
	zr *zstd.Decoder
	f  *os.File
}

func (z *zstdReadCloser) Read(p []byte) (int, error) { return z.zr.Read(p) }

func (z *zstdReadCloser) Close() error {
	z.zr.Close()
	return z.f.Close()
}

// scanCodexRollouts walks ~/.codex/sessions and returns every rollout file
// path (both `.jsonl` and cold `.jsonl.zst`). An absent sessions dir is
// not an error — it just means no Codex conversations exist yet. Per-entry
// walk errors are tolerated so one unreadable date dir doesn't hide the
// rest.
func scanCodexRollouts(home string) ([]string, error) {
	root := codexSessionsDir(home)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip an unreadable entry, keep walking siblings
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") {
			return nil
		}
		if strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.zst") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// findCodexRollout locates a rollout by session id (the resume model finds
// the file by id, not by date). Returns "" when no rollout matches. When a
// session is mid-compression (both .jsonl and .jsonl.zst on disk) it
// returns the preferred (uncompressed) one.
func findCodexRollout(home, convID string) (string, error) {
	paths, err := scanCodexRollouts(home)
	if err != nil {
		return "", err
	}
	return dedupCodexRollouts(paths)[convID], nil
}

// dedupCodexRollouts collapses rollout paths to one per session id. Codex
// ages a session by writing the `.jsonl.zst` and THEN deleting the
// `.jsonl`, so during that window both files exist for the SAME uuid —
// without dedup the conv would list twice and a prefix that uniquely names
// that uuid would resolve as "ambiguous". Paths with an unparseable name
// are dropped (they carry no id to key on). See preferCodexRollout for the
// tie-break.
func dedupCodexRollouts(paths []string) map[string]string {
	byID := make(map[string]string, len(paths))
	for _, p := range paths {
		id := codexIDFromRolloutName(filepath.Base(p))
		if id == "" {
			continue
		}
		if cur, ok := byID[id]; ok {
			byID[id] = preferCodexRollout(cur, p)
		} else {
			byID[id] = p
		}
	}
	return byID
}

// preferCodexRollout picks which of two rollout paths for the SAME session
// id to keep: the uncompressed `.jsonl` beats a transient `.jsonl.zst`
// (avoids a needless decompress and is the live file during the
// compression window); among two same-kind paths the lexically-greater
// wins, which orders the date-indexed tree newest-last.
func preferCodexRollout(a, b string) string {
	aZst := strings.HasSuffix(a, ".zst")
	bZst := strings.HasSuffix(b, ".zst")
	if aZst != bZst {
		if aZst {
			return b // b is the uncompressed one
		}
		return a
	}
	if a >= b {
		return a
	}
	return b
}

// codexSessionsDir is the root of Codex's date-indexed rollout tree.
func codexSessionsDir(home string) string {
	return filepath.Join(home, ".codex", "sessions")
}

// codexIDFromRolloutName extracts the session uuid from a rollout file
// name `rollout-<ts>-<uuid>.jsonl[.zst]`. Both the timestamp and the uuid
// contain '-', so the uuid is taken as the trailing 36 chars (8-4-4-4-12)
// of the base name rather than by splitting on '-'. Returns "" when the
// name doesn't carry a well-formed uuid.
func codexIDFromRolloutName(name string) string {
	base := strings.TrimSuffix(name, ".zst")
	base = strings.TrimSuffix(base, ".jsonl")
	if !strings.HasPrefix(base, "rollout-") {
		return ""
	}
	if len(base) < 36 {
		return ""
	}
	id := base[len(base)-36:]
	if !looksLikeUUID(id) {
		return ""
	}
	return id
}

// looksLikeUUID reports whether s is a canonical 8-4-4-4-12 hex uuid. Used
// to validate the id carved out of a rollout filename before trusting it.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
