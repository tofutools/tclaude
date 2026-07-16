package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	codexTelemetryCheckpointVersion  = 1
	codexTelemetryAnchorBytes        = 64
	maxCodexTelemetryCheckpointBytes = 1 << 20
)

var ErrCodexTelemetryCheckpointTooLarge = errors.New("codex telemetry checkpoint exceeds size limit")

// CodexTelemetryFollower incrementally follows one live Codex rollout. It
// memoizes the resolved path and accumulated parser state; callers may reuse a
// follower across concurrent dashboard polls.
type CodexTelemetryFollower struct {
	mu sync.Mutex

	home     string
	convID   string
	path     string
	info     os.FileInfo
	offset   int64
	state    codexRuntimeScanState
	snapshot CodexRuntimeSnapshot

	checkpointSize    int64
	checkpointModTime int64
	checkpointDevice  uint64
	checkpointInode   uint64
	checkpointAnchor  []byte
	restored          bool
}

// codexTelemetryCheckpoint is the durable form of the follower's cursor and
// accumulated fold state. The state must travel with the offset: resuming from
// byte N with an empty interrupted-child/followup ledger would produce a
// different answer from scanning bytes [0,N) first.
type codexTelemetryCheckpoint struct {
	Version              int                  `json:"version"`
	Home                 string               `json:"home"`
	ConvID               string               `json:"conv_id"`
	Path                 string               `json:"path"`
	Offset               int64                `json:"offset"`
	FileSize             int64                `json:"file_size"`
	ModTimeUnixNano      int64                `json:"mod_time_unix_nano"`
	Device               uint64               `json:"device,omitempty"`
	Inode                uint64               `json:"inode,omitempty"`
	Anchor               []byte               `json:"anchor"`
	Latest               *codexTokenCountInfo `json:"latest,omitempty"`
	ContextReset         bool                 `json:"context_reset,omitempty"`
	InterruptedSubagents []string             `json:"interrupted_subagents,omitempty"`
	FollowupCallIDs      []string             `json:"followup_call_ids,omitempty"`
}

// RestoreCheckpoint primes an empty follower from a durable checkpoint. The
// next RuntimeTelemetry call validates the path, size/mtime and bytes directly
// before Offset before trusting it. Invalid JSON or an unsupported version is
// rejected; a valid-but-stale file checkpoint simply falls back to fullScan.
func (f *CodexTelemetryFollower) RestoreCheckpoint(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(data) == 0 || len(data) > maxCodexTelemetryCheckpointBytes {
		return fmt.Errorf("invalid Codex telemetry checkpoint size %d", len(data))
	}
	var cp codexTelemetryCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("decode Codex telemetry checkpoint: %w", err)
	}
	if cp.Version != codexTelemetryCheckpointVersion {
		return fmt.Errorf("unsupported Codex telemetry checkpoint version %d", cp.Version)
	}
	if cp.Home == "" || cp.ConvID == "" || cp.Path == "" || cp.Offset <= 0 ||
		cp.FileSize < cp.Offset || len(cp.Anchor) == 0 || len(cp.Anchor) > codexTelemetryAnchorBytes {
		return fmt.Errorf("invalid Codex telemetry checkpoint cursor")
	}
	state := newCodexRuntimeScanState()
	state.latest = cp.Latest
	state.contextReset = cp.ContextReset
	for _, id := range cp.InterruptedSubagents {
		if id != "" {
			state.interruptedSubagents[id] = struct{}{}
		}
	}
	for _, id := range cp.FollowupCallIDs {
		if id != "" {
			state.followupCallIDs[id] = struct{}{}
		}
	}
	f.home = cp.Home
	f.convID = cp.ConvID
	f.path = cp.Path
	f.info = nil
	f.offset = cp.Offset
	f.state = state
	f.snapshot = state.snapshot()
	f.checkpointSize = cp.FileSize
	f.checkpointModTime = cp.ModTimeUnixNano
	f.checkpointDevice = cp.Device
	f.checkpointInode = cp.Inode
	f.checkpointAnchor = append([]byte(nil), cp.Anchor...)
	f.restored = true
	return nil
}

// Checkpoint returns a deterministic durable checkpoint after at least one
// successful scan. The byte slice is safe for the caller to retain.
func (f *CodexTelemetryFollower) Checkpoint() ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path == "" || f.offset <= 0 || f.checkpointSize < f.offset || len(f.checkpointAnchor) == 0 {
		return nil, false, nil
	}
	cp := codexTelemetryCheckpoint{
		Version:              codexTelemetryCheckpointVersion,
		Home:                 f.home,
		ConvID:               f.convID,
		Path:                 f.path,
		Offset:               f.offset,
		FileSize:             f.checkpointSize,
		ModTimeUnixNano:      f.checkpointModTime,
		Device:               f.checkpointDevice,
		Inode:                f.checkpointInode,
		Anchor:               append([]byte(nil), f.checkpointAnchor...),
		Latest:               f.state.latest,
		ContextReset:         f.state.contextReset,
		InterruptedSubagents: sortedStringSet(f.state.interruptedSubagents),
		FollowupCallIDs:      sortedStringSet(f.state.followupCallIDs),
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return nil, false, fmt.Errorf("encode Codex telemetry checkpoint: %w", err)
	}
	if len(data) > maxCodexTelemetryCheckpointBytes {
		return nil, false, fmt.Errorf("%w: %d bytes", ErrCodexTelemetryCheckpointTooLarge, len(data))
	}
	return data, true, nil
}

func sortedStringSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// RuntimeTelemetry returns convID's current rollout-derived state. An unchanged
// file is answered from memory without opening it. Live .jsonl files are read
// from the last complete newline; archived .zst files are immutable and only
// use the stat cache.
func (f *CodexTelemetryFollower) RuntimeTelemetry(home, convID string) (CodexRuntimeSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path, info, err := f.rollout(home, convID)
	if err != nil {
		return CodexRuntimeSnapshot{}, err
	}
	if path == "" {
		return CodexRuntimeSnapshot{}, nil
	}
	if f.restored {
		if !f.restoreMatches(path, info) {
			f.clearCursor()
		} else {
			f.restored = false
			f.info = info
			// An exact unchanged-file match needs no open/read at all. Growth
			// is handled below by scanAppend from the restored offset.
			if info.Size() == f.checkpointSize && info.ModTime().UnixNano() == f.checkpointModTime {
				f.info = info
				return f.snapshot, nil
			}
			if err := f.scanAppend(path); err == nil {
				f.snapshot = f.state.snapshot()
				return f.snapshot, nil
			}
			// A persisted cursor is only an optimization. Any validation or
			// incremental-decode doubt falls back to the authoritative rebuild.
			f.clearCursor()
		}
	}
	if f.info != nil && os.SameFile(f.info, info) && f.info.Size() == info.Size() &&
		f.info.ModTime().Equal(info.ModTime()) {
		return f.snapshot, nil
	}

	unchangedFile := f.info != nil && os.SameFile(f.info, info)
	canIncrement := unchangedFile && !strings.HasSuffix(path, ".zst") && info.Size() >= f.offset
	if canIncrement && info.Size() > f.offset {
		if err := f.scanAppend(path); err == nil {
			f.snapshot = f.state.snapshot()
			return f.snapshot, nil
		}
		// Any seek/read/decode doubt falls through to the authoritative rebuild.
	}
	return f.fullScan(path, info)
}

// rollout reuses the memoized path while it still exists. Codex archives by
// replacing .jsonl with .jsonl.zst, so a missing cached path is the signal to
// walk the date tree again.
func (f *CodexTelemetryFollower) rollout(home, convID string) (string, os.FileInfo, error) {
	if home != f.home || convID != f.convID {
		f.clearCursor()
		f.home = home
		f.convID = convID
		f.path = ""
	}
	if f.path != "" {
		info, err := os.Stat(f.path)
		if err == nil {
			return f.path, info, nil
		}
		if !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	path, err := findCodexRollout(home, convID)
	if err != nil {
		return path, nil, err
	}
	if path == "" {
		f.clearCursor()
		f.path = ""
		return "", nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if path != f.path {
		f.path = path
		f.clearCursor()
		f.path = path
	}
	return path, info, nil
}

func (f *CodexTelemetryFollower) fullScan(path string, info os.FileInfo) (CodexRuntimeSnapshot, error) {
	if strings.HasSuffix(path, ".zst") {
		snap, err := CodexRuntimeTelemetryFromRollout(path)
		if err != nil {
			return CodexRuntimeSnapshot{}, err
		}
		f.state = newCodexRuntimeScanState()
		f.offset = 0
		f.info = info
		f.snapshot = snap
		return snap, nil
	}

	// Open/scan/capture from one descriptor. If the pathname is replaced while
	// it is being scanned, retry against the replacement rather than combining
	// file A's fold state with file B's identity and anchor.
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.Open(path)
		if err != nil {
			return CodexRuntimeSnapshot{}, err
		}
		state := newCodexRuntimeScanState()
		offset, metadata, _, scanErr := scanCodexTelemetryToStable(file, path, &state, 0, false)
		_ = file.Close()
		if scanErr != nil {
			return CodexRuntimeSnapshot{}, fmt.Errorf("scan codex rollout %s: %w", path, scanErr)
		}
		pathInfo, statErr := os.Stat(path)
		if statErr != nil {
			return CodexRuntimeSnapshot{}, statErr
		}
		if !os.SameFile(metadata.info, pathInfo) {
			continue
		}
		f.state = state
		f.offset = offset
		f.info = metadata.info
		f.snapshot = state.snapshot()
		f.applyCheckpoint(metadata)
		return f.snapshot, nil
	}
	return CodexRuntimeSnapshot{}, fmt.Errorf("codex rollout %s changed repeatedly while scanning", path)
}

func (f *CodexTelemetryFollower) scanAppend(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if f.info != nil && !os.SameFile(f.info, openedInfo) {
		return fmt.Errorf("codex rollout replaced before incremental scan")
	}
	// Parse into a copy so a read/decode failure cannot partially advance the
	// durable state before the caller's full-rescan fallback succeeds.
	state := f.state.clone()
	newOffset, metadata, doubt, err := scanCodexTelemetryToStable(file, path, &state, f.offset, true)
	if err != nil {
		return err
	}
	if doubt {
		return fmt.Errorf("decode newly appended rollout line")
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(metadata.info, pathInfo) {
		return fmt.Errorf("codex rollout replaced during incremental scan")
	}
	f.state = state
	f.offset = newOffset
	f.info = metadata.info
	f.applyCheckpoint(metadata)
	return nil
}

func (f *CodexTelemetryFollower) clearCursor() {
	f.info = nil
	f.offset = 0
	f.state = newCodexRuntimeScanState()
	f.snapshot = CodexRuntimeSnapshot{}
	f.checkpointSize = 0
	f.checkpointModTime = 0
	f.checkpointDevice = 0
	f.checkpointInode = 0
	f.checkpointAnchor = nil
	f.restored = false
}

func (f *CodexTelemetryFollower) restoreMatches(path string, info os.FileInfo) bool {
	if path != f.path || strings.HasSuffix(path, ".zst") || info.Size() < f.offset ||
		info.Size() < f.checkpointSize {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	metadata, metadataErr := captureCodexTelemetryCheckpoint(file, f.offset)
	_ = file.Close()
	if metadataErr != nil || !os.SameFile(info, metadata.info) {
		return false
	}
	if metadata.size < f.checkpointSize {
		return false
	}
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(metadata.info, pathInfo) {
		return false
	}
	// Same-size rewrites are not append-only and must rebuild even if their
	// final anchor happens to match. Growth is allowed to change mtime.
	if metadata.size == f.checkpointSize && metadata.modTime != f.checkpointModTime {
		return false
	}
	if metadata.device != 0 && metadata.inode != 0 &&
		(f.checkpointDevice == 0 || f.checkpointInode == 0 ||
			metadata.device != f.checkpointDevice || metadata.inode != f.checkpointInode) {
		return false
	}
	return bytes.Equal(metadata.anchor, f.checkpointAnchor)
}

type codexTelemetryCheckpointMetadata struct {
	info          os.FileInfo
	size          int64
	modTime       int64
	device, inode uint64
	anchor        []byte
}

// scanCodexTelemetryToStable closes the EOF/stat race: a writer may append a
// complete record after scanCompleteCodexLines observes EOF but before we stat
// the descriptor for the checkpoint. Keep consuming from the committed offset
// until size and mtime stay unchanged across both EOF and metadata capture.
// A stable incomplete tail is intentionally accepted with size > offset; its
// bytes are retried when the file next changes.
func scanCodexTelemetryToStable(
	file *os.File,
	path string,
	state *codexRuntimeScanState,
	offset int64,
	strict bool,
) (int64, codexTelemetryCheckpointMetadata, bool, error) {
	return scanCodexTelemetryToStableWithScanner(file, path, state, offset, strict, scanCompleteCodexLines)
}

type codexTelemetryLineScanner func(
	io.Reader,
	string,
	*codexRuntimeScanState,
	bool,
) (int64, bool, error)

func scanCodexTelemetryToStableWithScanner(
	file *os.File,
	path string,
	state *codexRuntimeScanState,
	offset int64,
	strict bool,
	scan codexTelemetryLineScanner,
) (int64, codexTelemetryCheckpointMetadata, bool, error) {
	const maxStabilityAttempts = 8
	var doubt bool
	for range maxStabilityAttempts {
		before, err := file.Stat()
		if err != nil {
			return offset, codexTelemetryCheckpointMetadata{}, doubt, err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return offset, codexTelemetryCheckpointMetadata{}, doubt, err
		}
		read, scanDoubt, err := scan(file, path, state, strict)
		if err != nil {
			return offset, codexTelemetryCheckpointMetadata{}, doubt, err
		}
		doubt = doubt || scanDoubt
		offset += read
		after, err := file.Stat()
		if err != nil {
			return offset, codexTelemetryCheckpointMetadata{}, doubt, err
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			continue
		}
		metadata, err := captureCodexTelemetryCheckpoint(file, offset)
		if err != nil {
			return offset, codexTelemetryCheckpointMetadata{}, doubt, err
		}
		if metadata.size != after.Size() || metadata.modTime != after.ModTime().UnixNano() {
			continue
		}
		return offset, metadata, doubt, nil
	}
	return offset, codexTelemetryCheckpointMetadata{}, doubt,
		fmt.Errorf("codex rollout %s did not stabilize while scanning", path)
}

func captureCodexTelemetryCheckpoint(file *os.File, offset int64) (codexTelemetryCheckpointMetadata, error) {
	if offset < 0 {
		return codexTelemetryCheckpointMetadata{}, fmt.Errorf("invalid Codex telemetry checkpoint offset %d", offset)
	}
	info, err := file.Stat()
	if err != nil {
		return codexTelemetryCheckpointMetadata{}, err
	}
	if info.Size() < offset {
		return codexTelemetryCheckpointMetadata{}, fmt.Errorf("codex rollout shrank while scanning")
	}
	start := max(offset-codexTelemetryAnchorBytes, 0)
	anchor := make([]byte, offset-start)
	if len(anchor) > 0 {
		if _, err := file.ReadAt(anchor, start); err != nil {
			return codexTelemetryCheckpointMetadata{}, err
		}
	}
	device, inode, _ := codexTelemetryFileIdentity(info)
	return codexTelemetryCheckpointMetadata{
		info: info, size: info.Size(), modTime: info.ModTime().UnixNano(),
		device: device, inode: inode, anchor: anchor,
	}, nil
}

func (f *CodexTelemetryFollower) applyCheckpoint(metadata codexTelemetryCheckpointMetadata) {
	f.checkpointSize = metadata.size
	f.checkpointModTime = metadata.modTime
	f.checkpointDevice = metadata.device
	f.checkpointInode = metadata.inode
	f.checkpointAnchor = metadata.anchor
}

// scanCompleteCodexLines consumes newline-terminated records only. A writer may
// be between write(2)s when the dashboard polls; the unterminated tail stays at
// the current offset and is retried with the next append.
func scanCompleteCodexLines(r io.Reader, rolloutPath string, state *codexRuntimeScanState, strict bool) (consumed int64, doubt bool, err error) {
	reader := bufio.NewReaderSize(r, 64*1024)
	line := make([]byte, 0, 64*1024)
	for {
		var lineBytes int64
		var oversized bool
		var readErr error
		line, lineBytes, oversized, readErr = readCodexRolloutLine(reader, line[:0])
		switch readErr {
		case nil:
			if oversized {
				// Compaction replacement-history and image payloads can be tens
				// of MiB. Match the telemetry parser's best-effort malformed-line
				// contract: discard the completed record, advance past its
				// newline, and continue to later telemetry without retaining the
				// whole payload in memory. Do not flag decode doubt: rebuilding
				// would encounter the same record and loop forever.
				slog.Warn("codex-telemetry: skipping oversized rollout record",
					"path", rolloutPath, "bytes", lineBytes,
					"limit_bytes", maxCodexRolloutLineBytes, "module", "harness")
				if isCodexCompactedRecordPrefix(line) {
					state.invalidateContext()
				}
			} else if ok := state.consumeLine(line); strict && !ok {
				doubt = true
			}
			consumed += lineBytes
		case io.EOF:
			if lineBytes == 0 {
				return consumed, doubt, nil
			}
			// Match the established parser for a syntactically complete EOF
			// record. An oversized or invalid mid-write tail is not consumed;
			// the follower retries it from the same offset after the next append.
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
