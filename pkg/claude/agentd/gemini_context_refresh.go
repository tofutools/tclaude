package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Read-through refresh of a Gemini session's context/usage columns, in the
// shape of copilot_context_refresh.go: the dashboard and the agent API read
// every session row anyway, so that read is where a grown session file is
// noticed.
//
// Gemini's session file carries per-call usage on every model message (see
// harness/gemini_usage.go), so unlike Copilot's durable log it can say what
// the context occupancy is after every turn. What it does not need is a byte-
// offset follower: one conversation's file is small and is re-folded whole,
// and only when its size or mtime moved.

const geminiContextRefreshInterval = 2 * time.Second

type geminiContextRefreshState struct {
	// convID and createdAt identify the session generation (see the Copilot
	// state for why a pruned-and-recreated id must not inherit a fold).
	convID    string
	createdAt time.Time

	lastRefresh time.Time
	refreshing  bool

	// file is the located session file and what it looked like at the last
	// fold; an unchanged stat skips the re-read.
	file harness.GeminiSessionFileStat

	persisted harness.GeminiUsage
}

var geminiContextRefreshMu struct {
	sync.Mutex
	states   map[string]*geminiContextRefreshState
	stopping bool
}

// refreshGeminiContextSnapshotOnRead brings one live Gemini session's
// context/usage columns up to date from its session file. A no-op for every
// other harness, for a dead session, and before the conversation id exists.
func refreshGeminiContextSnapshotOnRead(sess *db.SessionRow, alive bool) {
	if sess == nil || !alive || sess.Harness != harness.GeminiName ||
		sess.ID == "" || sess.ConvID == "" {
		return
	}
	state, ok := claimGeminiContextRefresh(sess, time.Now())
	if !ok {
		return
	}
	defer releaseGeminiContextRefresh(state)

	if state.file.Path != "" {
		stat, found := harness.StatGeminiSessionFile(state.file.Path)
		if found && stat.Size == state.file.Size && stat.ModTime.Equal(state.file.ModTime) {
			return
		}
		if !found {
			// Migrated (.json → .jsonl) or removed: locate it afresh.
			state.file = harness.GeminiSessionFileStat{}
		}
	}
	path := state.file.Path
	if path == "" {
		located, found, err := harness.LocateGeminiSessionFile(sess.ConvID)
		if err != nil {
			slog.Warn("gemini-usage: cannot locate the session file",
				"session_id", sess.ID, "conv_id", sess.ConvID, "error", err, "module", "agentd")
			return
		}
		if !found {
			// Gemini creates the file on the first recorded message.
			return
		}
		path = located.Path
	}
	usage, stat, ok := harness.GeminiUsageFromFile(path)
	if !ok {
		state.file = harness.GeminiSessionFileStat{}
		return
	}
	state.file = stat
	persistGeminiContextSnapshot(sess, state, usage)
}

// persistGeminiContextSnapshot writes the projection when it changed. A file
// with no usage yet (the first turn is still in flight) writes nothing, so an
// earlier reading is never blanked.
func persistGeminiContextSnapshot(sess *db.SessionRow, state *geminiContextRefreshState, usage harness.GeminiUsage) {
	if usage.Calls == 0 || usage == state.persisted {
		return
	}
	updated, err := db.UpdateContextSnapshotForGeneration(sess.ID, sess.ConvID, sess.CreatedAt,
		usage.ContextPct(), usage.ContextTokens, usage.OutputTokens, usage.ContextWindow)
	if err != nil {
		slog.Warn("gemini-usage: failed to persist the context snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	if updated {
		state.persisted = usage
	}
}

func claimGeminiContextRefresh(sess *db.SessionRow, now time.Time) (*geminiContextRefreshState, bool) {
	geminiContextRefreshMu.Lock()
	defer geminiContextRefreshMu.Unlock()
	if geminiContextRefreshMu.stopping {
		return nil, false
	}
	if geminiContextRefreshMu.states == nil {
		geminiContextRefreshMu.states = map[string]*geminiContextRefreshState{}
	}
	state := geminiContextRefreshMu.states[sess.ID]
	if state == nil || state.convID != sess.ConvID || !state.createdAt.Equal(sess.CreatedAt) {
		state = &geminiContextRefreshState{convID: sess.ConvID, createdAt: sess.CreatedAt}
		geminiContextRefreshMu.states[sess.ID] = state
	} else if state.refreshing || now.Sub(state.lastRefresh) < geminiContextRefreshInterval {
		return nil, false
	}
	state.refreshing = true
	state.lastRefresh = now
	return state, true
}

func releaseGeminiContextRefresh(state *geminiContextRefreshState) {
	geminiContextRefreshMu.Lock()
	defer geminiContextRefreshMu.Unlock()
	state.refreshing = false
}

// stopGeminiContextRefreshes blocks further refreshes during daemon shutdown.
func stopGeminiContextRefreshes() {
	geminiContextRefreshMu.Lock()
	defer geminiContextRefreshMu.Unlock()
	geminiContextRefreshMu.stopping = true
	geminiContextRefreshMu.states = nil
}
