package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/filefollow"
)

const (
	codexTelemetryCheckpointVersion  = 4
	codexTelemetryAnchorBytes        = 64
	maxCodexTelemetryCheckpointBytes = 1 << 20
)

var ErrCodexTelemetryCheckpointTooLarge = errors.New("codex telemetry checkpoint exceeds size limit")

// CodexTelemetryFollower incrementally follows one live Codex rollout. It
// memoizes the resolved path and accumulated parser state; callers may reuse a
// follower across concurrent dashboard polls.
type CodexTelemetryFollower struct {
	mu sync.Mutex

	home            string
	convID          string
	path            string
	stream          *filefollow.Follower[codexRuntimeScanState]
	archiveInfo     os.FileInfo
	state           codexRuntimeScanState
	snapshot        CodexRuntimeSnapshot
	children        map[string]*CodexTelemetryFollower
	preserveMissing bool

	checkpointTooLarge           bool
	checkpointTooLargeStateBytes int64
}

func (f *CodexTelemetryFollower) ensureStream() *filefollow.Follower[codexRuntimeScanState] {
	if f.stream == nil {
		f.stream = filefollow.New(filefollow.Config[codexRuntimeScanState]{
			NewState: func(string, int64) codexRuntimeScanState {
				if f.preserveMissing {
					return newOwnedCodexRuntimeScanState(f.convID)
				}
				return newCodexRuntimeScanState()
			},
			CloneState: func(state codexRuntimeScanState) codexRuntimeScanState { return state.clone() },
			Scan: func(r io.Reader, path string, state *codexRuntimeScanState, strict bool) (int64, bool, error) {
				if !strict {
					state.costAuthoritative = true
				}
				return scanCompleteCodexLines(r, path, state, strict)
			},
			AnchorBytes: codexTelemetryAnchorBytes,
		})
	}
	return f.stream
}

// codexTelemetryCheckpoint is the durable form of the follower's cursor and
// accumulated fold state. The state must travel with the offset: resuming from
// byte N with an empty interrupted-child/followup ledger would produce a
// different answer from scanning bytes [0,N) first.
type codexTelemetryCheckpoint struct {
	Version              int                           `json:"version"`
	Home                 string                        `json:"home"`
	ConvID               string                        `json:"conv_id"`
	Path                 string                        `json:"path"`
	OwnerBoundarySeen    bool                          `json:"owner_boundary_seen,omitempty"`
	Offset               int64                         `json:"offset"`
	FileSize             int64                         `json:"file_size"`
	ModTimeUnixNano      int64                         `json:"mod_time_unix_nano"`
	Device               uint64                        `json:"device,omitempty"`
	Inode                uint64                        `json:"inode,omitempty"`
	Anchor               []byte                        `json:"anchor"`
	Latest               *codexTokenCountInfo          `json:"latest,omitempty"`
	ContextReset         bool                          `json:"context_reset,omitempty"`
	Model                string                        `json:"model,omitempty"`
	Effort               string                        `json:"effort,omitempty"`
	Usage                *CodexUsage                   `json:"usage,omitempty"`
	CostUSD              float64                       `json:"cost_usd,omitempty"`
	CostPriced           bool                          `json:"cost_priced,omitempty"`
	CostModel            string                        `json:"cost_model,omitempty"`
	CostObserved         string                        `json:"cost_observed,omitempty"`
	CostAuthoritative    bool                          `json:"cost_authoritative,omitempty"`
	CostHistory          []CodexTokenCostDailySnapshot `json:"cost_history,omitempty"`
	InterruptedSubagents []string                      `json:"interrupted_subagents,omitempty"`
	FollowupCallIDs      []string                      `json:"followup_call_ids,omitempty"`
	DiscoveredSubagents  []string                      `json:"discovered_subagents,omitempty"`
	Children             map[string]json.RawMessage    `json:"children,omitempty"`
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
	// Older checkpoints have no discovered-child ledger. Restoring their EOF
	// cursor would permanently skip collaboration edges that occurred in the
	// already-consumed prefix, so upgrades deliberately rebuild once.
	if cp.Version != codexTelemetryCheckpointVersion {
		return fmt.Errorf("unsupported Codex telemetry checkpoint version %d", cp.Version)
	}
	if cp.Home == "" || cp.ConvID == "" || cp.Path == "" || cp.Offset <= 0 ||
		cp.FileSize < cp.Offset || len(cp.Anchor) == 0 || len(cp.Anchor) > codexTelemetryAnchorBytes {
		return fmt.Errorf("invalid Codex telemetry checkpoint cursor")
	}
	state := newCodexRuntimeScanState()
	if f.preserveMissing {
		state = newOwnedCodexRuntimeScanState(cp.ConvID)
		state.ownerBoundarySeen = cp.OwnerBoundarySeen
	}
	state.replaceCheckpointContext(cp.Latest, cp.ContextReset)
	state.model = cp.Model
	state.effort = cp.Effort
	state.usage = cp.Usage
	state.costUSD = cp.CostUSD
	state.costPriced = cp.CostPriced
	state.costModel = cp.CostModel
	state.costObserved = cp.CostObserved
	state.costAuthoritative = cp.CostAuthoritative
	state.costHistory = append([]CodexTokenCostDailySnapshot(nil), cp.CostHistory...)
	for _, id := range cp.InterruptedSubagents {
		if id != "" {
			state.addCheckpointSetValue(state.interruptedSubagents, checkpointInterruptedSubagentsPrefix, id)
		}
	}
	for _, id := range cp.FollowupCallIDs {
		if id != "" {
			state.addCheckpointSetValue(state.followupCallIDs, checkpointFollowupCallIDsPrefix, id)
		}
	}
	for _, id := range cp.DiscoveredSubagents {
		if id != "" {
			state.discoveredSubagents[id] = struct{}{}
		}
	}
	cursor := filefollow.Cursor{
		Path: cp.Path, Offset: cp.Offset, FileSize: cp.FileSize,
		ModTimeUnixNano: cp.ModTimeUnixNano, Device: cp.Device, Inode: cp.Inode,
		Anchor: append([]byte(nil), cp.Anchor...),
	}
	if err := f.ensureStream().Restore(cursor, state); err != nil {
		return fmt.Errorf("restore Codex telemetry cursor: %w", err)
	}
	f.home = cp.Home
	f.convID = cp.ConvID
	f.path = cp.Path
	f.state = state
	f.snapshot = state.snapshot()
	f.children = make(map[string]*CodexTelemetryFollower, len(cp.Children))
	for id, childData := range cp.Children {
		child := &CodexTelemetryFollower{preserveMissing: true}
		if err := child.RestoreCheckpoint(childData); err != nil {
			return fmt.Errorf("restore Codex child %s checkpoint: %w", id, err)
		}
		f.children[id] = child
		state.discoveredSubagents[id] = struct{}{}
	}
	f.checkpointTooLarge = false
	return nil
}

// Checkpoint returns a deterministic durable checkpoint after at least one
// successful scan. The byte slice is safe for the caller to retain.
func (f *CodexTelemetryFollower) Checkpoint() ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, cursorOK := f.ensureStream().Checkpoint()
	if !cursorOK || f.path == "" || cursor.Offset <= 0 {
		return nil, false, nil
	}
	// Avoid rebuilding, sorting, and marshaling a multi-MB checkpoint until its
	// mutable serialized state has become smaller than at the last failed try.
	// Growth, including add-then-remove churn back to the same size, cannot make
	// an append-only rollout's checkpoint fit under the cap.
	if f.checkpointTooLarge && f.state.checkpointStateBytes >= f.checkpointTooLargeStateBytes {
		return nil, false, nil
	}
	cp := codexTelemetryCheckpoint{
		Version:              codexTelemetryCheckpointVersion,
		Home:                 f.home,
		ConvID:               f.convID,
		Path:                 f.path,
		OwnerBoundarySeen:    f.state.ownerBoundarySeen,
		Offset:               cursor.Offset,
		FileSize:             cursor.FileSize,
		ModTimeUnixNano:      cursor.ModTimeUnixNano,
		Device:               cursor.Device,
		Inode:                cursor.Inode,
		Anchor:               append([]byte(nil), cursor.Anchor...),
		Latest:               f.state.latest,
		ContextReset:         f.state.contextReset,
		Model:                f.state.model,
		Effort:               f.state.effort,
		Usage:                f.state.usage,
		CostUSD:              f.state.costUSD,
		CostPriced:           f.state.costPriced,
		CostModel:            f.state.costModel,
		CostObserved:         f.state.costObserved,
		CostAuthoritative:    f.state.costAuthoritative,
		CostHistory:          append([]CodexTokenCostDailySnapshot(nil), f.state.costHistory...),
		InterruptedSubagents: sortedStringSet(f.state.interruptedSubagents),
		FollowupCallIDs:      sortedStringSet(f.state.followupCallIDs),
		DiscoveredSubagents:  sortedStringSet(f.state.discoveredSubagents),
	}
	if len(f.children) > 0 {
		cp.Children = make(map[string]json.RawMessage, len(f.children))
		for id := range f.state.discoveredSubagents {
			child := f.children[id]
			if child == nil {
				continue
			}
			data, ok, err := child.Checkpoint()
			if err != nil {
				return nil, false, fmt.Errorf("encode Codex child %s checkpoint: %w", id, err)
			}
			if ok {
				cp.Children[id] = data
			}
		}
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return nil, false, fmt.Errorf("encode Codex telemetry checkpoint: %w", err)
	}
	if len(data) > maxCodexTelemetryCheckpointBytes {
		f.checkpointTooLarge = true
		f.checkpointTooLargeStateBytes = f.state.checkpointStateBytes
		return nil, false, fmt.Errorf("%w: %d bytes", ErrCodexTelemetryCheckpointTooLarge, len(data))
	}
	f.checkpointTooLarge = false
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
		if f.preserveMissing {
			return f.aggregateChildrenLocked(home)
		}
		return CodexRuntimeSnapshot{}, nil
	}
	if strings.HasSuffix(path, ".zst") {
		if f.archiveInfo != nil && os.SameFile(f.archiveInfo, info) &&
			f.archiveInfo.Size() == info.Size() && f.archiveInfo.ModTime().Equal(info.ModTime()) {
			return f.aggregateChildrenLocked(home)
		}
		ownerID := ""
		if f.preserveMissing {
			ownerID = convID
		}
		state, err := codexRuntimeTelemetryStateFromRollout(path, ownerID)
		if err != nil {
			return CodexRuntimeSnapshot{}, err
		}
		f.ensureStream().Reset()
		f.archiveInfo = info
		f.state = state
		f.snapshot = state.snapshot()
		f.pruneChildrenLocked()
		return f.aggregateChildrenLocked(home)
	}
	update, err := f.ensureStream().RefreshWithInfo(path, info)
	if err != nil {
		return CodexRuntimeSnapshot{}, fmt.Errorf("follow Codex rollout %s: %w", path, err)
	}
	if update.Unchanged {
		return f.aggregateChildrenLocked(home)
	}
	f.state = update.State
	f.archiveInfo = nil
	f.snapshot = update.State.snapshot()
	f.pruneChildrenLocked()
	return f.aggregateChildrenLocked(home)
}

func (f *CodexTelemetryFollower) pruneChildrenLocked() {
	for id := range f.children {
		if _, discovered := f.state.discoveredSubagents[id]; !discovered {
			delete(f.children, id)
		}
	}
}

func (f *CodexTelemetryFollower) aggregateChildrenLocked(home string) (CodexRuntimeSnapshot, error) {
	if f.children == nil {
		f.children = map[string]*CodexTelemetryFollower{}
	}
	parts := []CodexRuntimeSnapshot{f.snapshot}
	for id := range f.state.discoveredSubagents {
		child := f.children[id]
		if child == nil {
			child = &CodexTelemetryFollower{preserveMissing: true}
			f.children[id] = child
		}
		snap, err := child.RuntimeTelemetry(home, id)
		if err != nil {
			return CodexRuntimeSnapshot{}, fmt.Errorf("follow Codex child %s: %w", id, err)
		}
		parts = append(parts, snap)
	}
	return aggregateCodexRuntimeCosts(parts), nil
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
		if !f.preserveMissing {
			f.clearCursor()
		}
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

func (f *CodexTelemetryFollower) clearCursor() {
	if f.stream != nil {
		f.stream.Reset()
	}
	f.archiveInfo = nil
	f.state = newCodexRuntimeScanState()
	f.snapshot = CodexRuntimeSnapshot{}
	f.children = nil
	f.checkpointTooLarge = false
}

// scanCompleteCodexLines consumes newline-terminated records only. A writer may
// be between write(2)s when the dashboard polls; the unterminated tail stays at
// the current offset and is retried with the next append.
func scanCompleteCodexLines(r io.Reader, rolloutPath string, state *codexRuntimeScanState, strict bool) (consumed int64, doubt bool, err error) {
	return filefollow.ScanLines(r, filefollow.LineConfig{MaxRecordBytes: maxCodexRolloutLineBytes}, func(line filefollow.Line) bool {
		if line.Oversized {
			slog.Warn("codex-telemetry: skipping oversized rollout record",
				"path", rolloutPath, "bytes", line.Bytes,
				"limit_bytes", maxCodexRolloutLineBytes, "module", "harness")
			if isCodexCompactedRecordPrefix(line.Data) {
				state.invalidateContext()
			}
			return true
		}
		return state.consumeLine(line.Data)
	}, strict)
}
