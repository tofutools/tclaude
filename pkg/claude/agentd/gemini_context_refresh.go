package agentd

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
	// inheritanceResolved records that the follower was given the
	// conversation's inherited-history boundary (db.ConvCostInheritedUntil).
	inheritanceResolved bool

	// persisted is the projection last written. It advances only on a
	// successful write, so a failed one is retried on the next refresh.
	persisted harness.GeminiUsage
	// persistedCost is the cost total last written, persistedWhatIf whether
	// it went to the WHAT-IF column, and costWritten whether any was; same
	// retry rule.
	persistedCost   float64
	persistedWhatIf bool
	costWritten     bool

	// authType is the generation's Gemini auth type, read once (authResolved)
	// from its settings: it decides real versus WHAT-IF under billing "auto".
	authType     string
	authResolved bool
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
	if !state.inheritanceResolved {
		// A clone or imported history: its copied calls are the source
		// conversation's spend. Written before the conversation first runs.
		until, err := db.ConvCostInheritedUntil(sess.ConvID)
		if err != nil {
			slog.Warn("gemini-usage: cannot resolve the inherited-history boundary",
				"session_id", sess.ID, "conv_id", sess.ConvID, "error", err, "module", "agentd")
			return
		}
		state.follower.SetCostInheritedUntil(until)
		state.inheritanceResolved = true
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
	persistGeminiCost(sess, state, usage)
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

// persistGeminiCost writes the conversation's cost history when the total or
// its classification changed. The rows are replaced across the conversation
// (a resume keeps the conv id under a new session id), so a resumed
// generation never double-counts its predecessor's calls, and a
// re-classification moves the whole history between the real and WHAT-IF
// columns.
//
// Gemini records no dollar figure, so the amount is always tclaude's estimate
// at Gemini API rates; geminiCostIsWhatIf only decides whether that estimate
// stands for real per-token spend or a hypothetical one.
func persistGeminiCost(sess *db.SessionRow, state *geminiContextRefreshState, usage harness.GeminiUsage) {
	if usage.CostUSD <= 0 {
		return
	}
	whatIf := geminiCostIsWhatIf(sess, state)
	if state.costWritten && usage.CostUSD == state.persistedCost && whatIf == state.persistedWhatIf {
		return
	}
	history := state.follower.CostHistory(time.Now())
	daily := make([]db.VirtualCostDailySnapshot, 0, len(history))
	for _, day := range history {
		daily = append(daily, db.VirtualCostDailySnapshot{
			Day: day.Day, CostUSD: day.CostUSD, UpdatedAt: day.Observed, Model: day.Model,
		})
	}
	updated, err := db.ReplaceSessionCostHistoryForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt, usage.CostUSD, daily, whatIf)
	if err != nil {
		slog.Warn("gemini-usage: failed to persist the cost",
			"session_id", sess.ID, "model", usage.Model, "what_if", whatIf, "error", err, "module", "agentd")
		return
	}
	if updated {
		state.persistedCost, state.persistedWhatIf, state.costWritten = usage.CostUSD, whatIf, true
	}
}

// geminiCostIsWhatIf classifies a session's cost per cost.gemini_billing.
// Under "auto" an API-key or Vertex AI session is real spend; a Google
// sign-in, an unselected or unreadable auth type, or any other type stays
// WHAT-IF, so tclaude never presents an unconfirmed charge as real.
func geminiCostIsWhatIf(sess *db.SessionRow, state *geminiContextRefreshState) bool {
	cfg, err := config.Load()
	if err != nil {
		slog.Debug("gemini-usage: config load failed; classifying cost automatically", "error", err)
	}
	switch cfg.ResolvedGeminiBilling() {
	case config.GeminiBillingAPI:
		return false
	case config.GeminiBillingSubscription:
		return true
	}
	if !state.authResolved {
		authType, err := harness.GeminiSelectedAuthType(os.Getenv, sess.Cwd)
		if err != nil {
			slog.Debug("gemini-usage: cannot resolve the auth type; cost stays WHAT-IF",
				"session_id", sess.ID, "error", err, "module", "agentd")
		}
		state.authType, state.authResolved = authType, true
	}
	return !harness.GeminiAuthBilledPerToken(state.authType)
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
