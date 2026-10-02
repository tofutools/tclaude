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
// the context occupancy is after every turn. The follower reads only the bytes
// appended since the previous refresh, and decodes only the usage fields, so a
// refresh during a tool-heavy turn stays cheap on this request path.

const geminiContextRefreshInterval = 2 * time.Second

type geminiContextRefreshState struct {
	// convID and createdAt identify the session generation (see the Copilot
	// state for why a pruned-and-recreated id must not inherit a fold).
	convID    string
	createdAt time.Time

	lastRefresh time.Time
	refreshing  bool

	follower harness.GeminiUsageFollower

	// persisted is the projection last written. It advances only on a
	// successful write, so a failed one is retried on the next refresh.
	persisted harness.GeminiUsage
	// persistedCost is the WHAT-IF total last written, and costWritten whether
	// any was; same retry rule.
	persistedCost float64
	costWritten   bool
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

	path := state.follower.Path()
	if migrated := harness.GeminiMigratedSessionFile(path); migrated != "" {
		// A resumed legacy session: Gemini migrated it to a sibling `.jsonl`
		// and writes only there from now on, leaving the `.json` in place.
		path = migrated
	}
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
		path = located
	}
	usage, found, err := state.follower.Read(path)
	if err != nil {
		slog.Warn("gemini-usage: cannot read the session file",
			"session_id", sess.ID, "path", path, "error", err, "module", "agentd")
		return
	}
	if !found {
		// Migrated or removed; the next refresh locates it afresh.
		return
	}
	persistGeminiContextSnapshot(sess, state, usage)
	persistGeminiVirtualCost(sess, state, usage)
}

// persistGeminiContextSnapshot writes the projection when it changed. A file
// with no usage yet (the first turn is still in flight) writes nothing, so an
// earlier reading is never blanked.
//
// The model column carries the latest call's model, so a switch made inside
// the pane with /model reaches the dashboard. Gemini records no reasoning
// effort anywhere (and tclaude refuses one at launch), so effort stays empty.
func persistGeminiContextSnapshot(sess *db.SessionRow, state *geminiContextRefreshState, usage harness.GeminiUsage) {
	if usage.Calls == 0 || usage == state.persisted {
		return
	}
	updated, err := db.UpdateContextSnapshotAndModelEffortForGeneration(sess.ID, sess.ConvID, sess.CreatedAt,
		usage.ContextPct(), usage.ContextTokens, usage.OutputTokens, usage.ContextWindow, usage.Model, "")
	if err != nil {
		slog.Warn("gemini-usage: failed to persist the context snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	if updated {
		state.persisted = usage
	}
}

// persistGeminiVirtualCost writes the conversation's WHAT-IF cost history
// when the total changed. The rows are replaced across the conversation (a
// resume keeps the conv id under a new session id), so a resumed generation
// never double-counts its predecessor's calls.
func persistGeminiVirtualCost(sess *db.SessionRow, state *geminiContextRefreshState, usage harness.GeminiUsage) {
	if usage.CostUSD <= 0 || (state.costWritten && usage.CostUSD == state.persistedCost) {
		return
	}
	history := state.follower.CostHistory(time.Now())
	daily := make([]db.VirtualCostDailySnapshot, 0, len(history))
	for _, day := range history {
		daily = append(daily, db.VirtualCostDailySnapshot{
			Day: day.Day, CostUSD: day.CostUSD, UpdatedAt: day.Observed, Model: day.Model,
		})
	}
	updated, err := db.ReplaceSessionVirtualCostHistoryForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt, usage.CostUSD, daily)
	if err != nil {
		slog.Warn("gemini-usage: failed to persist the what-if cost",
			"session_id", sess.ID, "model", usage.Model, "error", err, "module", "agentd")
		return
	}
	if updated {
		state.persistedCost, state.costWritten = usage.CostUSD, true
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
