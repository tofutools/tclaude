package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	offset, _, err := scanCompleteCodexLines(file, &state, false)
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
	read, doubt, err := scanCompleteCodexLines(file, &state, true)
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
func scanCompleteCodexLines(r io.Reader, state *codexRuntimeScanState, strict bool) (consumed int64, doubt bool, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCodexRolloutLineBytes)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if atEOF && len(bytes.TrimSpace(data)) > 0 && json.Valid(bytes.TrimSpace(data)) {
			// Match the established full parser for a complete final JSON value
			// even if the writer has not appended its newline yet. An invalid
			// (mid-write) tail remains unconsumed and is retried from its start.
			return len(data), data, nil
		}
		if atEOF {
			return 0, nil, nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		line := scanner.Bytes()
		consumed += int64(len(line))
		if ok := state.consumeLine(line); strict && !ok {
			doubt = true
		}
	}
	return consumed, doubt, scanner.Err()
}
