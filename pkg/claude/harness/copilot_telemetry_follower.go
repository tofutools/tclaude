package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/filefollow"
)

// CopilotTelemetryFollower incrementally follows one live Copilot session's
// events.jsonl and projects the durable usage/context fields described in
// copilot_telemetry.go.
//
// The shape mirrors CodexTelemetryFollower because the hazards are the same
// and were already solved once in pkg/claude/common/filefollow: an unchanged
// file is answered from memory, an append is scanned from the last complete
// newline, and a truncate/replace forces exactly one authoritative rebuild.
//
// Copilot's log differs from Codex's in one way that matters. A `--resume`
// does not start a new file; it APPENDS a `session.resume` record to the
// existing one. For a byte-offset follower that is the easy case — a resume is
// indistinguishable from any other append — but it is also why the fold state
// must survive a checkpoint: resuming the cursor at byte N with an empty state
// would lose every earlier lifetime's model, counts and totals.
type CopilotTelemetryFollower struct {
	mu sync.Mutex

	home     string
	convID   string
	path     string
	stream   *filefollow.Follower[copilotRuntimeScanState]
	state    copilotRuntimeScanState
	snapshot CopilotRuntimeSnapshot
	// hydrated records that at least one successful scan has happened, so a
	// caller can tell "no data yet" from "a session that genuinely reports
	// nothing".
	hydrated bool
}

const (
	copilotTelemetryCheckpointVersion = 2
	copilotTelemetryAnchorBytes       = 64
	// maxCopilotTelemetryCheckpointBytes bounds the durable blob. This
	// checkpoint holds no unbounded collections — only scalars, one usage
	// struct and one bounded error message — so the cap is a corruption guard
	// rather than a budget that real state can approach.
	maxCopilotTelemetryCheckpointBytes = 64 << 10
)

func (f *CopilotTelemetryFollower) ensureStream() *filefollow.Follower[copilotRuntimeScanState] {
	if f.stream == nil {
		f.stream = filefollow.New(filefollow.Config[copilotRuntimeScanState]{
			NewState:    func(string, int64) copilotRuntimeScanState { return newCopilotRuntimeScanState() },
			CloneState:  func(state copilotRuntimeScanState) copilotRuntimeScanState { return state.clone() },
			Scan:        scanCompleteCopilotLines,
			AnchorBytes: copilotTelemetryAnchorBytes,
		})
	}
	return f.stream
}

// copilotTelemetryCheckpoint is the durable form of the cursor plus the whole
// accumulated fold. Both halves travel together for the reason filefollow's
// own docs give: a cursor without its state is not resumable.
type copilotTelemetryCheckpoint struct {
	Version int    `json:"version"`
	Home    string `json:"home"`
	ConvID  string `json:"conv_id"`
	Path    string `json:"path"`

	Offset          int64  `json:"offset"`
	FileSize        int64  `json:"file_size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	Device          uint64 `json:"device,omitempty"`
	Inode           uint64 `json:"inode,omitempty"`
	Anchor          []byte `json:"anchor"`

	Model                 string                   `json:"model,omitempty"`
	Effort                string                   `json:"effort,omitempty"`
	ContextTier           string                   `json:"context_tier,omitempty"`
	CopilotVersion        string                   `json:"copilot_version,omitempty"`
	UserMessages          int                      `json:"user_messages,omitempty"`
	AssistantMessages     int                      `json:"assistant_messages,omitempty"`
	AssistantOutputTokens int64                    `json:"assistant_output_tokens,omitempty"`
	Lifetimes             int                      `json:"lifetimes,omitempty"`
	Context               *CopilotContextTelemetry `json:"context,omitempty"`
	Usage                 *CopilotUsage            `json:"usage,omitempty"`
	NanoAIU               float64                  `json:"nano_aiu,omitempty"`
	HasNanoAIU            bool                     `json:"has_nano_aiu,omitempty"`
	PremiumRequests       float64                  `json:"premium_requests,omitempty"`
	LastError             *CopilotErrorObservation `json:"last_error,omitempty"`
}

// RestoreCheckpoint primes an empty follower from a durable checkpoint. The
// next RuntimeTelemetry call re-validates the path, identity, size/mtime and
// the bytes immediately before Offset before trusting any of it, so a stale
// checkpoint costs one full rescan rather than a wrong answer.
func (f *CopilotTelemetryFollower) RestoreCheckpoint(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(data) == 0 || len(data) > maxCopilotTelemetryCheckpointBytes {
		return fmt.Errorf("invalid Copilot telemetry checkpoint size %d", len(data))
	}
	var cp copilotTelemetryCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("decode Copilot telemetry checkpoint: %w", err)
	}
	// A version bump means the fold changed shape, and an old EOF cursor would
	// permanently skip whatever the new fold wanted from the consumed prefix.
	// Rebuilding once is the only correct upgrade.
	if cp.Version != copilotTelemetryCheckpointVersion {
		return fmt.Errorf("unsupported Copilot telemetry checkpoint version %d", cp.Version)
	}
	if cp.Home == "" || cp.ConvID == "" || cp.Path == "" || cp.Offset <= 0 ||
		cp.FileSize < cp.Offset || len(cp.Anchor) == 0 || len(cp.Anchor) > copilotTelemetryAnchorBytes {
		return errors.New("invalid Copilot telemetry checkpoint cursor")
	}

	state := newCopilotRuntimeScanState()
	state.model = cp.Model
	state.effort = cp.Effort
	state.contextTier = cp.ContextTier
	state.copilotVersion = cp.CopilotVersion
	state.userMessages = cp.UserMessages
	state.assistantMessages = cp.AssistantMessages
	state.assistantOutputTokens = cp.AssistantOutputTokens
	state.lifetimes = cp.Lifetimes
	if cp.Context != nil {
		state.context = *cp.Context
		state.hasContext = true
	}
	if cp.Usage != nil {
		usage := *cp.Usage
		state.usage = &usage
	}
	state.nanoAIU = cp.NanoAIU
	state.hasNanoAIU = cp.HasNanoAIU
	state.premiumRequests = cp.PremiumRequests
	if cp.LastError != nil {
		lastError := *cp.LastError
		state.lastError = &lastError
	}

	cursor := filefollow.Cursor{
		Path: cp.Path, Offset: cp.Offset, FileSize: cp.FileSize,
		ModTimeUnixNano: cp.ModTimeUnixNano, Device: cp.Device, Inode: cp.Inode,
		Anchor: append([]byte(nil), cp.Anchor...),
	}
	if err := f.ensureStream().Restore(cursor, state); err != nil {
		return fmt.Errorf("restore Copilot telemetry cursor: %w", err)
	}
	f.home = cp.Home
	f.convID = cp.ConvID
	f.path = cp.Path
	f.state = state
	f.snapshot = state.snapshot()
	f.hydrated = true
	return nil
}

// Checkpoint returns a deterministic durable checkpoint once at least one
// successful scan has committed a cursor. (nil, false, nil) means "nothing
// worth persisting yet", which is not an error.
func (f *CopilotTelemetryFollower) Checkpoint() ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, ok := f.ensureStream().Checkpoint()
	if !ok || f.path == "" || cursor.Offset <= 0 {
		return nil, false, nil
	}
	cp := copilotTelemetryCheckpoint{
		Version:               copilotTelemetryCheckpointVersion,
		Home:                  f.home,
		ConvID:                f.convID,
		Path:                  f.path,
		Offset:                cursor.Offset,
		FileSize:              cursor.FileSize,
		ModTimeUnixNano:       cursor.ModTimeUnixNano,
		Device:                cursor.Device,
		Inode:                 cursor.Inode,
		Anchor:                append([]byte(nil), cursor.Anchor...),
		Model:                 f.state.model,
		Effort:                f.state.effort,
		ContextTier:           f.state.contextTier,
		CopilotVersion:        f.state.copilotVersion,
		UserMessages:          f.state.userMessages,
		AssistantMessages:     f.state.assistantMessages,
		AssistantOutputTokens: f.state.assistantOutputTokens,
		Lifetimes:             f.state.lifetimes,
		NanoAIU:               f.state.nanoAIU,
		HasNanoAIU:            f.state.hasNanoAIU,
		PremiumRequests:       f.state.premiumRequests,
	}
	if f.state.hasContext {
		context := f.state.context
		cp.Context = &context
	}
	if f.state.usage != nil {
		usage := *f.state.usage
		cp.Usage = &usage
	}
	if f.state.lastError != nil {
		lastError := *f.state.lastError
		cp.LastError = &lastError
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return nil, false, fmt.Errorf("encode Copilot telemetry checkpoint: %w", err)
	}
	if len(data) > maxCopilotTelemetryCheckpointBytes {
		return nil, false, fmt.Errorf("copilot telemetry checkpoint exceeds %d bytes: %d",
			maxCopilotTelemetryCheckpointBytes, len(data))
	}
	return data, true, nil
}

// RuntimeTelemetry returns convID's current durable-log projection.
//
// (zero, false, nil) means the session has no event log yet — a directory
// Copilot has created but not written to, or a conversation that has never
// received a prompt. That is not an error and callers must not treat it as
// one: writing a zeroed snapshot on it would blank a genuine earlier reading.
func (f *CopilotTelemetryFollower) RuntimeTelemetry(home, convID string) (CopilotRuntimeSnapshot, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path, info, err := f.eventLog(home, convID)
	if err != nil {
		return CopilotRuntimeSnapshot{}, false, err
	}
	if path == "" {
		return CopilotRuntimeSnapshot{}, false, nil
	}
	update, err := f.ensureStream().RefreshWithInfo(path, info)
	if err != nil {
		return CopilotRuntimeSnapshot{}, false, fmt.Errorf("follow Copilot event log %s: %w", path, err)
	}
	if update.Unchanged {
		return f.snapshot, f.hydrated, nil
	}
	f.state = update.State
	f.snapshot = update.State.snapshot()
	f.hydrated = true
	return f.snapshot, true, nil
}

// Stats exposes the underlying follower's cumulative work counters.
//
// It exists so "this reads appended bytes rather than rescanning the log" is
// an ASSERTABLE property rather than a claim in a comment: Rebuilds counts
// authoritative full scans, Appends counts incremental ones, and PayloadBytes
// counts what was actually handed to the decoder.
func (f *CopilotTelemetryFollower) Stats() filefollow.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensureStream().Stats()
}

// eventLog resolves and memoizes the log path, resetting the cursor whenever
// the follower is retargeted at a different home or conversation.
func (f *CopilotTelemetryFollower) eventLog(home, convID string) (string, os.FileInfo, error) {
	if home != f.home || convID != f.convID {
		f.clearCursor()
		f.home = home
		f.convID = convID
		f.path = ""
	}
	if home == "" || convID == "" || !copilotSafeConvID(convID) {
		// An id carrying a separator or a `..` is not a Copilot session id, and
		// joining it would stat outside the session-state tree.
		return "", nil, nil
	}
	path := filepath.Join(home, copilotSessionStateDirName, convID, copilotEventsFileName)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Copilot creates the session directory before the log. Keep any
			// state already accumulated: a vanished log is not evidence that
			// the earlier reading was wrong, and the next Refresh revalidates.
			return "", nil, nil
		}
		return "", nil, err
	}
	if path != f.path {
		f.clearCursor()
		f.path = path
	}
	return path, info, nil
}

func (f *CopilotTelemetryFollower) clearCursor() {
	if f.stream != nil {
		f.stream.Reset()
	}
	f.state = newCopilotRuntimeScanState()
	f.snapshot = CopilotRuntimeSnapshot{}
	f.hydrated = false
}

// scanCompleteCopilotLines consumes newline-terminated records only. Copilot
// may be between write(2)s when tclaude polls; an unterminated tail stays at
// the current offset and is retried on the next append rather than being
// decoded as a truncated record.
//
// An oversized line is skipped and the scan CONTINUES. Copilot's log is
// append-only, so aborting on one huge tool result would freeze this
// conversation's telemetry at whatever preceded it, permanently.
func scanCompleteCopilotLines(
	r io.Reader,
	path string,
	state *copilotRuntimeScanState,
	strict bool,
) (int64, bool, error) {
	return filefollow.ScanLines(r, filefollow.LineConfig{MaxRecordBytes: maxCopilotEventLineBytes},
		func(line filefollow.Line) bool {
			if line.Oversized {
				slog.Warn("copilot-telemetry: skipping oversized event record",
					"path", path, "bytes", line.Bytes,
					"limit_bytes", maxCopilotEventLineBytes, "module", "harness")
				return true
			}
			return state.consumeLine(line.Data)
		}, strict)
}
