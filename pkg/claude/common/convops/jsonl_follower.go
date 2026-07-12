package convops

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// This file adds an incremental "follower" over a live Claude Code
// transcript .jsonl, ported from the Codex telemetry follower (TCL-371,
// PR #1029). The daemon's fsnotify monitor re-parses a conversation on
// every debounced write; for a busy streaming agent with a multi-MB
// transcript that is a full read + per-line JSON decode of the whole file
// every ~500ms. The follower remembers a byte offset and accumulator state
// per path and, when the file has only grown, decodes just the appended
// bytes.
//
// Correctness rests on two things:
//   - The projection is a forward fold — every field parseJSONLSession
//     derives is head-only (first-wins), last-seen, or additive, so
//     accumulating forward yields the same result as a full scan. The
//     accumulator here IS the body of parseJSONLSession's loop, so the two
//     paths converge by construction (see parseJSONLSession, which now
//     drives the same jsonlScanState).
//   - We never trust a cursor we can't validate. Identity (inode) change,
//     size shrink below the cursor, a tail-anchor mismatch just before the
//     cursor, or any read/decode doubt discards the cursor and falls back
//     to an authoritative full reparse — degrading to today's behavior, one
//     full read, never to a corrupt index.

// maxJSONLLineBytes caps a single .jsonl record. Lives in convops.go
// (shared with parseJSONLSession); referenced here.

// jsonlScanState is the forward-accumulator for a Claude Code transcript
// scan. It holds exactly the running state parseJSONLSession's loop
// maintained as locals — the partial SessionEntry plus the branch-history
// and interrupt folds — so a full scan and an incremental follower fold
// through identical code.
type jsonlScanState struct {
	sessionID string
	fullPath  string

	entry SessionEntry

	firstTimestamp      string
	lastTurnInterrupted bool

	// oversizedSeen is sticky: set once any record past maxJSONLLineBytes
	// is skipped, and never cleared. It makes the scan "incomplete for
	// rebuild purposes" — a skipped record might have carried a branch
	// stamp, so the accumulated branch set may be missing an entry, and
	// RebuildConvBranchHistoryScan (a replace-set that DELETES unobserved
	// rows) must not run against a possibly-incomplete set. Being sticky
	// and carried through clone() is what keeps an incremental scan's
	// completeness verdict identical to a full reparse of the same bytes.
	oversizedSeen bool

	// branches gathers, per (canonical repo dir, branch), the timestamps
	// bracketing its appearance — folded into entry.BranchHistory at
	// finalize. Keyed by repoDir+"\x00"+branch: one conversation can touch
	// the same branch name in two repos, and those are distinct entries.
	branches map[string]*branchAccum
	// canonCwd memoises db.CanonicalizeRepoDir. Claude Code stamps a fixed
	// cwd onto every turn, so this resolves to a single entry.
	canonCwd map[string]string
}

type branchAccum struct {
	repoDir   string
	branch    string
	firstSeen time.Time
	lastSeen  time.Time
}

func newJSONLScanState(sessionID, fullPath string) jsonlScanState {
	return jsonlScanState{
		sessionID: sessionID,
		fullPath:  fullPath,
		entry: SessionEntry{
			SessionID: sessionID,
			FullPath:  fullPath,
		},
		branches: map[string]*branchAccum{},
		canonCwd: map[string]string{},
	}
}

// clone deep-copies the accumulator so an incremental scan can advance a
// throwaway copy: a read/decode failure partway through the appended bytes
// must not leave the durable state half-advanced before the full-rescan
// fallback runs. The entry is copied by value (its only slice field,
// BranchHistory, is populated at finalize, not during accumulation); the
// branch fold is deep-copied because its *branchAccum values are mutated
// in place.
func (s *jsonlScanState) clone() jsonlScanState {
	out := *s
	out.branches = make(map[string]*branchAccum, len(s.branches))
	for k, acc := range s.branches {
		cp := *acc
		out.branches[k] = &cp
	}
	out.canonCwd = make(map[string]string, len(s.canonCwd))
	maps.Copy(out.canonCwd, s.canonCwd)
	return out
}

// consumeLine folds one raw .jsonl line into the state. It returns false
// only when the line is non-empty but does not decode as JSON — "decode
// doubt". A full scan ignores the flag (skip-malformed, matching the
// historical parser); an incremental scan treats it as a signal to discard
// the cursor and full-reparse, because a torn/rewritten line read at a
// stale offset is exactly what must never be silently folded in.
func (s *jsonlScanState) consumeLine(line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	var msg jsonlMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return false
	}

	// First timestamp (first-wins).
	if s.firstTimestamp == "" && msg.Timestamp != "" {
		s.firstTimestamp = msg.Timestamp
	}

	// Project path from the first message that has it — cwd is fixed for
	// the life of a conversation, so first-wins.
	if s.entry.ProjectPath == "" && msg.Cwd != "" {
		s.entry.ProjectPath = msg.Cwd
	}

	// Git branch can change mid-conversation. Keep the LAST one in
	// GitBranch (where the agent is now) and the FIRST in
	// GitBranchStartup (the launch branch), and accumulate the history.
	if msg.GitBranch != "" {
		if s.entry.GitBranchStartup == "" {
			s.entry.GitBranchStartup = msg.GitBranch
		}
		s.entry.GitBranch = msg.GitBranch

		repoDir, ok := s.canonCwd[msg.Cwd]
		if !ok {
			repoDir = db.CanonicalizeRepoDir(msg.Cwd)
			s.canonCwd[msg.Cwd] = repoDir
		}
		accKey := repoDir + "\x00" + msg.GitBranch
		acc := s.branches[accKey]
		if acc == nil {
			acc = &branchAccum{repoDir: repoDir, branch: msg.GitBranch}
			s.branches[accKey] = acc
		}
		if ts := parseJSONLTimestamp(msg.Timestamp); !ts.IsZero() {
			if acc.firstSeen.IsZero() || ts.Before(acc.firstSeen) {
				acc.firstSeen = ts
			}
			if ts.After(acc.lastSeen) {
				acc.lastSeen = ts
			}
		}
	}

	// Custom title (last-wins) and summary (last-wins).
	if msg.Type == "custom-title" && msg.CustomTitle != "" {
		s.entry.CustomTitle = msg.CustomTitle
	}
	if msg.Type == "summary" && msg.Summary != "" {
		s.entry.Summary = msg.Summary
	}

	// First user message with actual text content as the prompt.
	if s.entry.FirstPrompt == "" && msg.Type == "user" && msg.Message.Role == "user" {
		text := extractMessageContent(msg.Message.Content)
		if text != "" && !strings.HasPrefix(text, "[Request interrupted") && !isSystemInjectedMessage(text) {
			s.entry.FirstPrompt = text
			if msg.Timestamp != "" {
				s.firstTimestamp = msg.Timestamp
			}
		}
	}

	// Track whether the most recent conversation turn is a user-interrupt
	// marker. Only user/assistant records are turns; a user record with no
	// extractable text is a tool_result carrier, not a turn, and must not
	// clear the flag. See parseJSONLSession's original comment for the full
	// rationale.
	switch msg.Type {
	case "user":
		if text := extractMessageContent(msg.Message.Content); text != "" {
			s.lastTurnInterrupted = msg.Message.Role == "user" &&
				interruptMarkers[strings.TrimSpace(text)]
		}
	case "assistant":
		s.lastTurnInterrupted = false
	}
	return true
}

// finalize folds the accumulated state into a SessionEntry, applying the
// same post-loop logic parseJSONLSession used: interrupt flag, branch-set
// fold, and the firstTimestamp / stub / Created fallback. Returns nil for a
// file with nothing indexable (no prompt, summary, or custom title and no
// timestamped line) — the stub case. info supplies mtime/size.
func (s *jsonlScanState) finalize(info os.FileInfo) *SessionEntry {
	entry := s.entry
	entry.FileMtime = info.ModTime().Unix()
	entry.FileSize = info.Size()
	entry.LastTurnInterrupted = s.lastTurnInterrupted

	// Rebuild BranchHistory fresh each finalize from the full accumulated
	// set — the follower carries the complete set across ticks, so this is
	// the whole history, which is what RebuildConvBranchHistoryScan (a
	// replace-set that deletes unobserved rows) needs.
	entry.BranchHistory = nil
	for _, acc := range s.branches {
		entry.BranchHistory = append(entry.BranchHistory, db.BranchObservation{
			Branch:    acc.branch,
			RepoDir:   acc.repoDir,
			FirstSeen: acc.firstSeen,
			LastSeen:  acc.lastSeen,
		})
	}

	if s.firstTimestamp == "" {
		// No timestamped line. A conversation can be NAMED before its
		// first turn (a spawned/reincarnated agent /rename'd at startup),
		// so a custom-title / summary alone still makes it indexable; only
		// a file with none of prompt/summary/title is a true empty stub.
		if entry.CustomTitle == "" && entry.Summary == "" && entry.FirstPrompt == "" {
			return nil
		}
		entry.Created = info.ModTime().UTC().Format(time.RFC3339)
	} else {
		entry.Created = s.firstTimestamp
	}
	entry.Modified = info.ModTime().UTC().Format(time.RFC3339)
	entry.MessageCount = 0
	return &entry
}

// readJSONLLine reads and drains one record in bounded chunks, mirroring
// readCodexRolloutLine (TCL-379). The returned slice holds at most
// maxJSONLLineBytes; lineBytes always reports the full drained size so the
// caller can advance its offset past an oversized record without retaining
// the whole payload. A record longer than the cap sets oversized=true —
// the scan skips it (warn once) rather than treating it as a hard failure.
func readJSONLLine(reader *bufio.Reader, line []byte) ([]byte, int64, bool, error) {
	var lineBytes int64
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		lineBytes += int64(len(fragment))
		if !oversized {
			remaining := maxJSONLLineBytes - len(line)
			if len(fragment) <= remaining {
				line = append(line, fragment...)
			} else {
				line = append(line, fragment[:remaining]...)
				oversized = true
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, lineBytes, oversized, err
	}
}

// scanJSONLLines consumes newline-terminated records from r into state and
// reports how many bytes of complete records were consumed. A writer may be
// mid-write(2) when the monitor polls: the unterminated tail stays at the
// current offset (not counted in consumed) and is retried on the next
// append. strict controls the decode-doubt contract — see consumeLine.
func scanJSONLLines(r io.Reader, path string, state *jsonlScanState, strict bool) (consumed int64, doubt bool, err error) {
	reader := bufio.NewReaderSize(r, 64*1024)
	line := make([]byte, 0, 64*1024)
	for {
		var lineBytes int64
		var oversized bool
		var readErr error
		line, lineBytes, oversized, readErr = readJSONLLine(reader, line[:0])
		switch readErr {
		case nil:
			if oversized {
				// An oversized record (a turn carrying a huge tool result
				// or pasted blob) must never be a hard scan failure. Match
				// the Codex follower: discard the record, advance past its
				// newline, continue. Do NOT flag doubt — a rebuild would
				// hit the same record and loop forever. Mark the scan
				// oversized-incomplete so the destructive branch-history
				// rebuild is skipped (the skipped record may have carried a
				// branch stamp); the row itself is still upserted.
				state.oversizedSeen = true
				slog.Warn("conv_index: skipping oversized .jsonl record",
					"path", path, "bytes", lineBytes,
					"limit_bytes", maxJSONLLineBytes)
			} else if ok := state.consumeLine(line); strict && !ok {
				doubt = true
			}
			consumed += lineBytes
		case io.EOF:
			if lineBytes == 0 {
				return consumed, doubt, nil
			}
			// A syntactically complete record can arrive without a trailing
			// newline at true EOF. Consume it only if it is valid, non-empty
			// and not oversized; a torn mid-write tail is left un-consumed
			// and retried from the same offset after the next append.
			trimmed := bytes.TrimSpace(line)
			if !oversized && len(trimmed) > 0 && json.Valid(trimmed) {
				if ok := state.consumeLine(line); strict && !ok {
					doubt = true
				}
				consumed += lineBytes
			}
			return consumed, doubt, nil
		default:
			return consumed, doubt, readErr
		}
	}
}

// convFollowerAnchorBytes is how many bytes just before the cursor the
// follower re-reads and compares before trusting appended content. It
// closes the one hole the size/inode guards miss: an in-place rewrite that
// ends LARGER than the cursor keeps the same inode and passes the shrink
// check, and decode-doubt only catches it if the new bytes at the old
// offset happen to be misaligned. Re-reading the committed tail detects any
// change to the bytes preceding the cursor directly. 64 bytes is ample —
// any real rewrite changes far more than the last line's tail — and the
// read is a single pread of a fixed small buffer.
const convFollowerAnchorBytes = 64

// convFollower incrementally follows ONE live transcript .jsonl. The
// daemon monitor holds one per watched path and drives it from a single
// goroutine, so it carries no lock (unlike the Codex follower, which is
// shared across concurrent dashboard polls). The cursor is in-memory only:
// a daemon restart starts from a clean full reparse, exactly like the Codex
// follower.
type convFollower struct {
	convID string

	info   os.FileInfo
	offset int64
	anchor []byte
	state  jsonlScanState

	entry    *SessionEntry
	complete bool
	primed   bool
}

func newConvFollower(convID string) *convFollower {
	return &convFollower{convID: convID}
}

// refresh returns convID's freshest scan result, reading only appended
// bytes when it safely can. scanComplete is false only when a read errored
// before EOF (an I/O failure) — an oversized record is skipped, not a
// failure. On any doubt it falls back to a full reparse, so the returned
// (entry, scanComplete) is always what a full parseJSONLSession of the same
// bytes would produce.
func (f *convFollower) refresh(path string, info os.FileInfo) (*SessionEntry, bool, error) {
	// Fully unchanged — same identity, size AND mtime — is answered from
	// memory without reopening. Requiring mtime equality (not just size) is
	// deliberate: a same-length in-place rewrite keeps size == offset but
	// bumps mtime, and must NOT be served from the stale cursor.
	if f.primed && f.info != nil && os.SameFile(f.info, info) &&
		f.info.Size() == info.Size() && f.info.ModTime().Equal(info.ModTime()) {
		return f.entry, f.complete, nil
	}

	// An identity change (rotation, replace-then-rename) or a shrink below
	// the cursor invalidates the offset outright, as does a not-yet-primed
	// follower. Only a same-inode file that has grown past the cursor is a
	// candidate for reading just the appended bytes — and even then the
	// tail-anchor check inside scanAppend must pass.
	sameFile := f.primed && f.info != nil && os.SameFile(f.info, info)
	if sameFile && info.Size() > f.offset {
		if entry, complete, ok := f.scanAppend(path, info); ok {
			return entry, complete, nil
		}
		// Any seek/read/anchor/decode doubt falls through to the
		// authoritative rebuild.
	}
	// size == offset with a bumped mtime (same-length rewrite), a shrink,
	// an identity change, or append-doubt all land here.
	return f.fullScan(path, info)
}

func (f *convFollower) fullScan(path string, info os.FileInfo) (*SessionEntry, bool, error) {
	file, err := os.Open(path) //nolint:gosec // path is a ~/.claude/projects .jsonl from our own monitor
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()

	state := newJSONLScanState(f.convID, path)
	consumed, _, scanErr := scanJSONLLines(file, path, &state, false)
	complete := scanErr == nil && !state.oversizedSeen
	entry := state.finalize(info)

	f.commit(info, consumed, state, entry, complete)
	if scanErr != nil {
		// Match parseJSONLSession: a scan that stopped before EOF is a
		// truncated view; report it so the caller skips the branch-history
		// rebuild. The state is still committed so the next tick can try to
		// increment from where we stopped.
		slog.Warn("conv_index: .jsonl scan stopped before EOF; branch history not rebuilt",
			"conv_id", f.convID, "error", scanErr)
	}
	return entry, complete, nil
}

func (f *convFollower) scanAppend(path string, info os.FileInfo) (*SessionEntry, bool, bool) {
	file, err := os.Open(path) //nolint:gosec // path is a ~/.claude/projects .jsonl from our own monitor
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = file.Close() }()

	// Tail-anchor check: re-read the committed bytes just before the cursor
	// and compare. A mismatch means the file was rewritten under the same
	// inode (not a pure append), so the offset is meaningless — bail to a
	// full reparse.
	if len(f.anchor) > 0 {
		buf := make([]byte, len(f.anchor))
		anchorStart := f.offset - int64(len(f.anchor))
		if _, err := file.ReadAt(buf, anchorStart); err != nil {
			return nil, false, false
		}
		if !bytes.Equal(buf, f.anchor) {
			slog.Debug("conv_index: .jsonl tail anchor mismatch; full reparse",
				"conv_id", f.convID, "path", path)
			return nil, false, false
		}
	}

	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return nil, false, false
	}
	// Advance a throwaway copy so a mid-append read/decode failure cannot
	// half-advance the durable state before the full-rescan fallback.
	state := f.state.clone()
	consumed, doubt, scanErr := scanJSONLLines(file, path, &state, true)
	if scanErr != nil || doubt {
		return nil, false, false
	}
	newOffset := f.offset + consumed
	// oversizedSeen is sticky in the accumulator, so it reflects any
	// oversized record from offset 0 — the completeness verdict matches a
	// full reparse of the same bytes.
	complete := !state.oversizedSeen
	entry := state.finalize(info)
	f.commitAt(info, newOffset, state, entry, complete)
	return entry, complete, true
}

// commit records a full-scan result: consumed is the absolute byte count of
// complete records from offset 0.
func (f *convFollower) commit(info os.FileInfo, consumed int64, state jsonlScanState, entry *SessionEntry, complete bool) {
	f.commitAt(info, consumed, state, entry, complete)
}

// commitAt durably advances the cursor to newOffset and snapshots the tail
// anchor from disk. Anchoring reads the last convFollowerAnchorBytes before
// newOffset so the next tick can prove the file was only appended to.
func (f *convFollower) commitAt(info os.FileInfo, newOffset int64, state jsonlScanState, entry *SessionEntry, complete bool) {
	f.info = info
	f.offset = newOffset
	f.state = state
	f.entry = entry
	f.complete = complete
	f.primed = true
	f.captureAnchor(newOffset)
}

// captureAnchor reads the committed tail from f's path so scanAppend can
// verify it next tick. Best-effort: on any read failure the anchor is
// cleared, which just forces the next tick's guard to skip the anchor check
// (the size/inode guards still apply, and a cleared anchor never
// false-positives).
func (f *convFollower) captureAnchor(offset int64) {
	if f.entry == nil {
		// A stub finalize (nothing indexable) — nothing committed worth
		// anchoring; the size/inode guards still cover the next tick.
		f.anchor = nil
		return
	}
	n := min(int64(convFollowerAnchorBytes), offset)
	if n <= 0 {
		f.anchor = nil
		return
	}
	file, err := os.Open(f.entry.FullPath) //nolint:gosec // our own monitored .jsonl
	if err != nil {
		f.anchor = nil
		return
	}
	defer func() { _ = file.Close() }()
	buf := make([]byte, n)
	if _, err := file.ReadAt(buf, offset-n); err != nil {
		f.anchor = nil
		return
	}
	f.anchor = buf
}

// ConvFollower is the exported per-path handle the daemon's fsnotify
// monitor holds to incrementally re-index one live transcript .jsonl. It is
// NOT safe for concurrent use: the monitor drives each follower from its
// single event-loop goroutine (the same goroutine that owns every
// conv_index write), so no lock is needed. The cursor is in-memory only —
// a daemon restart starts each follower cold with one full reparse.
type ConvFollower struct {
	convID     string
	projectDir string
	f          *convFollower
}

// NewConvFollower builds a follower for filePath. convID and the project
// dir are derived once from the path (a follower is 1:1 with a path, which
// is 1:1 with a conv). The follower is unprimed until its first ReindexFile.
func NewConvFollower(filePath string) *ConvFollower {
	convID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	return &ConvFollower{
		convID:     convID,
		projectDir: filepath.Dir(filePath),
		f:          newConvFollower(convID),
	}
}

// ReindexFile re-indexes filePath and writes the result into the DB cache,
// reading only appended bytes when the cursor is valid and full-reparsing
// otherwise. It is a drop-in for ScanAndUpsertFile on the monitor's live
// path: same DB side effects (conv_index upsert, branch-history rebuild,
// interrupted-session recovery), same self-cleaning delete when the file is
// gone. Returns the entry, or nil for a stub / deleted / non-conv file.
func (c *ConvFollower) ReindexFile(filePath string) *SessionEntry {
	if len(c.convID) != 36 { // not a conv .jsonl (UUID length)
		return nil
	}
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			_ = db.DeleteConvIndex(c.convID)
			_ = db.DeleteConvBranchHistory(c.convID)
		}
		return nil
	}
	scanned, scanComplete, err := c.f.refresh(filePath, info)
	if err != nil {
		// A hard I/O error opening the file (rare: stat succeeded, open
		// failed). Fall back to the full path so behavior is identical to
		// pre-follower on such errors; the cursor stays unprimed and retries
		// fresh next tick.
		return ScanAndUpsertFile(filePath)
	}
	return upsertScanResult(filePath, c.convID, c.projectDir, info, scanned, scanComplete)
}
