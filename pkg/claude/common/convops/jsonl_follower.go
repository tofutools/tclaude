package convops

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/filefollow"
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
// # The append-only contract
//
// The optimization rests on a domain assumption: a Claude Code transcript
// .jsonl is APPEND-ONLY for the life of its path. The viability work for
// TCL-381 characterized this empirically — a live transcript keeps a stable
// inode with monotonically growing size; /clear rotates to a NEW conv-id /
// path (never an in-place rewrite); resume and compaction append; cleanup
// deletes the whole file (a Remove event). We do not re-validate the whole
// file on every tick — that would mean reading it end-to-end, exactly the
// cost this change removes. Instead the guards below are cheap TRIPWIRES
// for the rewrite shapes that DO occur, backed by the append-only contract
// for the rest.
//
// # What each guard catches
//
//   - Forward-fold correctness. Every field parseJSONLSession derives is
//     head-only (first-wins), last-seen, or additive, so accumulating
//     forward yields the same result as a full scan. The accumulator here
//     IS the body of parseJSONLSession's loop, so the two paths converge by
//     construction — up to the time-dependent cwd canonicalization noted on
//     canonCwd, where already-observed branch repo-dir keys intentionally do
//     NOT re-converge with a fresh reparse after a mid-conversation symlink
//     retarget (that is per-tick-for-new-records equivalence, by design).
//   - Identity change (os.SameFile / inode): catches rotation and
//     atomic replace-then-rename — a wholesale file swap.
//   - Size shrink below the cursor: catches truncation / a shorter rewrite.
//   - Tail-anchor: re-reads the committed last ~64 bytes before the cursor
//     and compares. Catches an in-place rewrite (same inode) that ends
//     LARGER than the cursor — the one shape size + inode miss — as long as
//     it disturbs the bytes just before the cursor (an append-then-rewrite,
//     or any rewrite that shifts the tail).
//   - Read/decode doubt on the appended bytes: falls back to a full reparse.
//
// Any of these discards the cursor and full-reparses — degrading to today's
// behavior (one full read), never to a corrupt index.
//
// # Accepted residual risk
//
// The anchor is a tripwire, not a proof that the folded prefix is unchanged:
// validating the whole prefix would require reading the whole file. The
// undetected shape is specific. scanAppend only runs when the file grew
// (size > offset, same inode); so the residual case is an interior
// modification of bytes strictly BEFORE the anchor window, FOLLOWED BY an
// append that grows the file past the cursor while leaving the last ~64
// bytes untouched. That admits scanAppend, the anchor still matches, and the
// stale prefix state is retained — not detected until the next full reparse
// (the next daemon restart, or the next size shrink / rotation / decode-doubt
// for that conv).
//
// A separate, coarser edge: an in-place SAME-LENGTH rewrite that lands within
// the same one-second mtime tick as the committed stat hits refresh's
// unchanged-file fast path (same inode, size and mtime) and is served from
// memory. This is the mtime-resolution limitation the size check normally
// backstops, but size is unchanged here too.
//
// Claude Code does not write transcripts either way (it O_APPENDs whole
// records and never rewrites earlier bytes), so both are accepted residual
// risk rather than defended cases. The invariant we DO guarantee: we never
// trust a cursor whose validating anchor we could not read (see scanAppend).

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
	// canonCwd memoises db.CanonicalizeRepoDir WITHIN a single scan pass.
	// Canonicalization is time-dependent external state — db.CanonicalizeRepoDir
	// calls filepath.EvalSymlinks, so the same cwd string can resolve to
	// different repo dirs if a symlink in the path is retargeted between
	// ticks. The memo is therefore deliberately NOT carried across ticks
	// (clone() gives each scan a fresh, empty map), so this tick's new
	// records canonicalize against the filesystem exactly as a fresh full
	// parse would. Already-accumulated branchAccum keys keep the repo dir
	// observed when their turns were first read — historically accurate, and
	// the reason follower/full-reparse equivalence is stated per-tick for
	// NEW records rather than as a global re-canonicalization.
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
// in place. canonCwd is intentionally reset to empty, NOT copied — the
// memo must not survive across ticks (see its field comment).
func (s *jsonlScanState) clone() jsonlScanState {
	out := *s
	out.branches = make(map[string]*branchAccum, len(s.branches))
	for k, acc := range s.branches {
		cp := *acc
		out.branches[k] = &cp
	}
	out.canonCwd = map[string]string{}
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

// scanJSONLLines consumes newline-terminated records from r into state and
// reports how many bytes of complete records were consumed. A writer may be
// mid-write(2) when the monitor polls: the unterminated tail stays at the
// current offset (not counted in consumed) and is retried on the next
// append. strict controls the decode-doubt contract — see consumeLine.
func scanJSONLLines(r io.Reader, path string, state *jsonlScanState, strict bool) (consumed int64, doubt bool, err error) {
	return filefollow.ScanLines(r, filefollow.LineConfig{MaxRecordBytes: maxJSONLLineBytes}, func(line filefollow.Line) bool {
		if line.Oversized {
			// An oversized record must not make every later append rebuild.
			// Its bounded prefix is deliberately ignored and the complete
			// physical record is committed past its newline.
			state.oversizedSeen = true
			slog.Warn("conv_index: skipping oversized .jsonl record",
				"path", path, "bytes", line.Bytes,
				"limit_bytes", maxJSONLLineBytes)
			return true
		}
		return state.consumeLine(line.Data)
	}, strict)
}

// convFollower incrementally follows ONE live transcript .jsonl. The
// daemon monitor holds one per watched path and drives it from a single
// goroutine. All cursor and file-change mechanics live in filefollow.
type convFollower struct {
	convID string
	stream *filefollow.Follower[jsonlScanState]

	entry    *SessionEntry
	complete bool
}

func newConvFollower(convID string) *convFollower {
	f := &convFollower{convID: convID}
	f.stream = filefollow.New(filefollow.Config[jsonlScanState]{
		NewState:   func(path string, _ int64) jsonlScanState { return newJSONLScanState(convID, path) },
		CloneState: func(state jsonlScanState) jsonlScanState { return state.clone() },
		Scan:       scanJSONLLines,
	})
	return f
}

// refresh returns convID's freshest scan result, reading only appended
// bytes when it safely can. scanComplete is false only when a read errored
// before EOF (an I/O failure) — an oversized record is skipped, not a
// failure. On any doubt it falls back to a full reparse, so the returned
// (entry, scanComplete) is always what a full parseJSONLSession of the same
// bytes would produce.
func (f *convFollower) refresh(path string, info os.FileInfo) (*SessionEntry, bool, error) {
	_ = info // filefollow captures identity and metadata from one descriptor.
	update, err := f.stream.Refresh(path)
	if err != nil {
		return nil, false, err
	}
	if update.Unchanged {
		return f.entry, f.complete, nil
	}
	f.complete = !update.State.oversizedSeen
	f.entry = update.State.finalize(update.Info)
	return f.entry, f.complete, nil
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
		// Keep the last committed fold on transient I/O/churn. Recurring paths
		// must never bypass the follower with a second byte-zero scan; the next
		// event retries against the still-validated cursor or rebuilds once.
		slog.Warn("conv_index: incremental transcript refresh failed",
			"path", filePath, "conv_id", c.convID, "error", err)
		return c.f.entry
	}
	return upsertScanResult(filePath, c.convID, c.projectDir, info, scanned, scanComplete)
}

const maxSharedConvFollowers = 512

// customTitleTailBytes bounds the rare synchronous recovery used when a
// /clear hook outruns agentd's normal incremental transcript monitor. It is
// deliberately a tail window rather than a transcript rebuild: a direct
// /rename writes its custom-title record immediately before /clear.
const customTitleTailBytes = 4 * 1024 * 1024

type customTitleTailState struct {
	title string
}

func scanCustomTitleTail(r io.Reader, _ string, state *customTitleTailState, strict bool) (int64, bool, error) {
	return filefollow.ScanLines(r, filefollow.LineConfig{MaxRecordBytes: maxJSONLLineBytes}, func(line filefollow.Line) bool {
		if line.Oversized || len(bytes.TrimSpace(line.Data)) == 0 {
			return true
		}
		var msg struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if err := json.Unmarshal(line.Data, &msg); err != nil {
			return false
		}
		if msg.Type == "custom-title" && msg.CustomTitle != "" {
			state.title = msg.CustomTitle
		}
		return true
	}, strict)
}

// RefreshCustomTitleFromTail recovers the newest title from a bounded tail of
// one transcript. It exists for the /rename -> /clear ordering edge when no
// daemon monitor is following the file; normal recurring freshness continues
// through FollowAndUpsertFile. The title-only read intentionally does not
// stamp file metadata, because it has not rebuilt the full conversation row.
func RefreshCustomTitleFromTail(filePath string) (bool, error) {
	convID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	if len(convID) != 36 || filepath.Ext(filePath) != ".jsonl" {
		return false, nil
	}
	follower := filefollow.New(filefollow.Config[customTitleTailState]{
		NewState:      func(string, int64) customTitleTailState { return customTitleTailState{} },
		CloneState:    func(state customTitleTailState) customTitleTailState { return state },
		Scan:          scanCustomTitleTail,
		InitialOffset: filefollow.TailInitialOffset(customTitleTailBytes),
	})
	update, err := follower.Refresh(filePath)
	if err != nil {
		return false, err
	}
	if update.State.title == "" {
		return false, nil
	}
	if err := db.SetConvIndexCustomTitle(convID, update.State.title, db.DefaultHarness); err != nil {
		return false, err
	}
	if actor, err := db.GetAgentByConv(convID); err != nil {
		return false, err
	} else if actor != nil {
		if err := db.SetAgentPendingName(actor.AgentID, update.State.title); err != nil {
			return false, err
		}
	}
	return true, nil
}

type sharedConvFollowerEntry struct {
	mu       sync.Mutex
	follower *ConvFollower
	usedAt   time.Time
	active   int
}

var sharedConvFollowers = struct {
	sync.Mutex
	entries map[string]*sharedConvFollowerEntry
}{entries: make(map[string]*sharedConvFollowerEntry)}

// FollowAndUpsertFile is the concurrency-safe recurring-reader counterpart to
// ScanAndUpsertFile. Dashboard/name freshness fallbacks share these bounded
// per-path cursors so a race with agentd's fsnotify debounce cannot replay a
// large transcript from byte zero on every request.
func FollowAndUpsertFile(filePath string) *SessionEntry {
	sharedConvFollowers.Lock()
	entry := sharedConvFollowers.entries[filePath]
	if entry == nil {
		entry = &sharedConvFollowerEntry{follower: NewConvFollower(filePath)}
		sharedConvFollowers.entries[filePath] = entry
	}
	entry.usedAt = time.Now()
	entry.active++
	pruneSharedConvFollowers()
	sharedConvFollowers.Unlock()

	entry.mu.Lock()
	result := entry.follower.ReindexFile(filePath)
	entry.mu.Unlock()
	_, statErr := os.Stat(filePath)
	sharedConvFollowers.Lock()
	entry.active--
	if os.IsNotExist(statErr) && sharedConvFollowers.entries[filePath] == entry {
		delete(sharedConvFollowers.entries, filePath)
	}
	pruneSharedConvFollowers()
	sharedConvFollowers.Unlock()
	return result
}

// pruneSharedConvFollowers never evicts an in-flight entry: doing so would let
// a second caller create another follower and lock for the same path while the
// older fold can still commit. Temporary growth above the cap is preferable to
// concurrent out-of-order conv_index writes and is pruned as calls finish.
func pruneSharedConvFollowers() {
	for len(sharedConvFollowers.entries) > maxSharedConvFollowers {
		var oldestPath string
		var oldest time.Time
		for path, candidate := range sharedConvFollowers.entries {
			if candidate.active > 0 {
				continue
			}
			if oldestPath == "" || candidate.usedAt.Before(oldest) {
				oldestPath, oldest = path, candidate.usedAt
			}
		}
		if oldestPath == "" {
			return
		}
		delete(sharedConvFollowers.entries, oldestPath)
	}
}
