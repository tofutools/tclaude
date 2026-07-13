package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

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
	if f.info != nil && os.SameFile(f.info, info) && f.info.Size() == info.Size() &&
		f.info.ModTime().Equal(info.ModTime()) {
		return f.snapshot, nil
	}

	unchangedFile := f.info != nil && os.SameFile(f.info, info)
	canIncrement := unchangedFile && !strings.HasSuffix(path, ".zst") && info.Size() >= f.offset
	if canIncrement && info.Size() > f.offset {
		if err := f.scanAppend(path); err == nil {
			f.info = info
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
		f.home = home
		f.convID = convID
		f.path = ""
		f.info = nil
		f.offset = 0
		f.state = newCodexRuntimeScanState()
		f.snapshot = CodexRuntimeSnapshot{}
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
	if err != nil || path == "" {
		return path, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if path != f.path {
		f.path = path
		f.info = nil
		f.offset = 0
		f.state = newCodexRuntimeScanState()
		f.snapshot = CodexRuntimeSnapshot{}
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

	file, err := os.Open(path)
	if err != nil {
		return CodexRuntimeSnapshot{}, err
	}
	defer func() { _ = file.Close() }()
	state := newCodexRuntimeScanState()
	offset, _, err := scanCompleteCodexLines(file, path, &state, false)
	if err != nil {
		return CodexRuntimeSnapshot{}, fmt.Errorf("scan codex rollout %s: %w", path, err)
	}
	f.state = state
	f.offset = offset
	f.info = info
	f.snapshot = state.snapshot()
	return f.snapshot, nil
}

func (f *CodexTelemetryFollower) scanAppend(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return err
	}
	// Parse into a copy so a read/decode failure cannot partially advance the
	// durable state before the caller's full-rescan fallback succeeds.
	state := f.state.clone()
	read, doubt, err := scanCompleteCodexLines(file, path, &state, true)
	if err != nil {
		return err
	}
	if doubt {
		return fmt.Errorf("decode newly appended rollout line")
	}
	f.state = state
	f.offset += read
	return nil
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
