package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TCL-673 layers three OpenCode usage signals on top of the context-window
// reporting from TCL-701 (opencode_context.go), which this file deliberately
// reuses rather than re-deriving:
//
//   - cumulative real cost, from the SSE `session.updated` event's own `cost`;
//   - the provider/model slug, so the dashboard model column and the cost
//     history's denormalised model are populated for OpenCode; and
//   - a reconnect/resume backfill that seeds the latest context snapshot from
//     the conversation's message history, so a resumed session or a daemon
//     restart is authoritative before its next live turn.
//
// The occupancy math, the per-server model-limit cache, and the persistence
// chokepoint all live in opencode_context.go; the reconnect backfill funnels
// through persistOpenCodeContextUsage so there is exactly one context write
// path.

// persistOpenCodeModelSlug records the provider-qualified model identity
// ("openai/gpt-5.6-terra") from the assistant message the context snapshot came
// from, feeding the dashboard model column and the session_cost_daily model
// denormalisation. A no-op when either half is missing.
func persistOpenCodeModelSlug(runtime db.OpenCodeRuntime, usage openCodeContextUsage) {
	if usage.ProviderID == "" || usage.ModelID == "" {
		return
	}
	if err := db.UpdateSessionModelSlug(runtime.SessionID, usage.ProviderID+"/"+usage.ModelID); err != nil {
		slog.Debug("OpenCode model slug could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
}

type openCodeSessionUpdatedEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID   string  `json:"id"`
			Cost float64 `json:"cost"`
		} `json:"info"`
	} `json:"properties"`
}

// applyOpenCodeCost records OpenCode's own cumulative session cost as real spend
// from a `session.updated` event. The figure is OpenCode's, never a tclaude
// price table: on a ChatGPT/Codex subscription OpenCode reports 0 (no per-token
// bill), so nothing is written and the row honestly shows no cost; a
// pay-per-token key reports real spend, which lands in cost_usd. Zero (or
// absent) cost is left unwritten so a subscription session stays a clean N/A.
func applyOpenCodeCost(runtime db.OpenCodeRuntime, event json.RawMessage) {
	if runtime.ConvID == "" {
		return
	}
	// The stream is dominated by message/status events; skip the full unmarshal
	// unless this raw event could be a session.updated.
	if !bytes.Contains(event, []byte(`"session.updated"`)) {
		return
	}
	var decoded openCodeSessionUpdatedEvent
	if json.Unmarshal(event, &decoded) != nil || decoded.Type != "session.updated" {
		return
	}
	// /event is directory-scoped: match the conversation from the session's own
	// id, falling back to the envelope's sessionID, mirroring the context path's
	// robustness to either shape.
	sessionID := decoded.Properties.Info.ID
	if sessionID == "" {
		sessionID = decoded.Properties.SessionID
	}
	if sessionID != runtime.ConvID || decoded.Properties.Info.Cost <= 0 {
		return
	}
	if err := db.UpdateSessionCost(runtime.SessionID, decoded.Properties.Info.Cost); err != nil {
		slog.Warn("OpenCode session cost could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
}

// openCodeHistoryMessage is one entry of `GET /session/{id}/message`. Only the
// assistant `info` fields relevant to occupancy are decoded; the token shape
// reuses opencode_context.go's payload type.
type openCodeHistoryMessage struct {
	Info struct {
		ID         string   `json:"id"`
		Role       string   `json:"role"`
		ProviderID string   `json:"providerID"`
		ModelID    string   `json:"modelID"`
		Cost       *float64 `json:"cost"`
		Time       struct {
			Created int64 `json:"created"`
		} `json:"time"`
		Tokens openCodeMessageTokensPayload `json:"tokens"`
	} `json:"info"`
}

type openCodeProjectedMessageCost struct {
	usd      float64
	eligible bool
	real     bool
}

var openCodeVirtualCostState struct {
	sync.Mutex
	bySession map[string]map[string]openCodeProjectedMessageCost
}

func clearOpenCodeVirtualCostState(sessionID string) {
	openCodeVirtualCostState.Lock()
	delete(openCodeVirtualCostState.bySession, sessionID)
	openCodeVirtualCostState.Unlock()
}

func validOpenCodeRate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validOpenCodePrice(price openCodeModelPrice) bool {
	if !validOpenCodeRate(price.Input) || !validOpenCodeRate(price.Output) ||
		!validOpenCodeRate(price.Cache.Read) || !validOpenCodeRate(price.Cache.Write) {
		return false
	}
	return price.Input > 0 || price.Output > 0 || price.Cache.Read > 0 || price.Cache.Write > 0
}

// openCodeVirtualCostForUsage mirrors OpenCode's native cost calculation:
// rates are USD per million tokens; reasoning is charged as output; the
// highest matching context tier wins, with experimentalOver200K as the legacy
// fallback only when no explicit tier matches.
func openCodeVirtualCostForUsage(usage openCodeContextUsage, base openCodeModelPrice) (float64, bool) {
	price := base
	contextTokens := usage.Input + usage.CacheRead + usage.CacheWrite
	var (
		matched     bool
		matchedSize float64
	)
	for _, tier := range base.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size < 0 ||
			float64(contextTokens) <= tier.Tier.Size || (matched && tier.Tier.Size <= matchedSize) {
			continue
		}
		price.Input, price.Output, price.Cache = tier.Input, tier.Output, tier.Cache
		matched, matchedSize = true, tier.Tier.Size
	}
	if !matched && contextTokens > 200_000 && base.ExperimentalOver200K != nil {
		price.Input = base.ExperimentalOver200K.Input
		price.Output = base.ExperimentalOver200K.Output
		price.Cache = base.ExperimentalOver200K.Cache
	}
	if !validOpenCodePrice(price) {
		return 0, false
	}
	for _, tokens := range []int64{usage.Input, usage.Output, usage.Reasoning, usage.CacheRead, usage.CacheWrite} {
		if tokens < 0 {
			return 0, false
		}
	}
	const perMillion = 1_000_000
	usd := (float64(usage.Input)*price.Input +
		float64(usage.Output+usage.Reasoning)*price.Output +
		float64(usage.CacheRead)*price.Cache.Read +
		float64(usage.CacheWrite)*price.Cache.Write) / perMillion
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 {
		return 0, false
	}
	return usd, true
}

func projectOpenCodeMessageCost(usage openCodeContextUsage, prices map[string]openCodeModelPrice) openCodeProjectedMessageCost {
	if usage.ReportedCost == nil || usage.MessageID == "" {
		return openCodeProjectedMessageCost{}
	}
	if *usage.ReportedCost > 0 {
		return openCodeProjectedMessageCost{real: true}
	}
	if *usage.ReportedCost < 0 {
		return openCodeProjectedMessageCost{}
	}
	price, ok := prices[strings.TrimSpace(usage.ProviderID)+"/"+strings.TrimSpace(usage.ModelID)]
	if !ok {
		return openCodeProjectedMessageCost{}
	}
	usd, ok := openCodeVirtualCostForUsage(usage, price)
	return openCodeProjectedMessageCost{usd: usd, eligible: ok}
}

func persistOpenCodeVirtualCostState(runtime db.OpenCodeRuntime, messages map[string]openCodeProjectedMessageCost) {
	total := 0.0
	if len(messages) == 0 {
		return
	}
	for _, message := range messages {
		if message.real || !message.eligible {
			return
		}
		total += message.usd
	}
	if total <= 0 {
		return
	}
	if err := db.UpdateSessionVirtualCost(runtime.SessionID, total); err != nil {
		slog.Warn("OpenCode virtual cost could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
}

func openCodeActivityForUsage(runtime db.OpenCodeRuntime, usage openCodeContextUsage) db.OpenCodeUsageActivity {
	observedAt := usage.CreatedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	return db.OpenCodeUsageActivity{
		SessionID: runtime.SessionID, MessageID: usage.MessageID, ConvID: runtime.ConvID,
		ProviderID: usage.ProviderID, ModelID: usage.ModelID, ObservedAt: observedAt,
	}
}

// applyOpenCodeVirtualCostUsage replaces one message's projection. Replayed
// SSE updates therefore converge instead of incrementing, while model changes
// replace the old model's price contribution.
func applyOpenCodeVirtualCostUsage(ctx context.Context, runtime db.OpenCodeRuntime, usage openCodeContextUsage) {
	if usage.MessageID == "" {
		return
	}
	if err := db.UpsertOpenCodeUsageActivity(openCodeActivityForUsage(runtime, usage)); err != nil {
		slog.Debug("OpenCode usage activity could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	projected := projectOpenCodeMessageCost(usage, openCodeModelPrices(ctx, runtime))
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.bySession == nil {
		openCodeVirtualCostState.bySession = map[string]map[string]openCodeProjectedMessageCost{}
	}
	messages := openCodeVirtualCostState.bySession[runtime.SessionID]
	if messages == nil {
		messages = map[string]openCodeProjectedMessageCost{}
		openCodeVirtualCostState.bySession[runtime.SessionID] = messages
	}
	messages[usage.MessageID] = projected
	snapshot := make(map[string]openCodeProjectedMessageCost, len(messages))
	for id, message := range messages {
		snapshot[id] = message
	}
	openCodeVirtualCostState.Unlock()
	persistOpenCodeVirtualCostState(runtime, snapshot)
}

func replaceOpenCodeVirtualCostUsage(ctx context.Context, runtime db.OpenCodeRuntime, usages []openCodeContextUsage) {
	prices := openCodeModelPrices(ctx, runtime)
	messages := make(map[string]openCodeProjectedMessageCost, len(usages))
	activity := make([]db.OpenCodeUsageActivity, 0, len(usages))
	for _, usage := range usages {
		if usage.MessageID == "" {
			continue
		}
		messages[usage.MessageID] = projectOpenCodeMessageCost(usage, prices)
		activity = append(activity, openCodeActivityForUsage(runtime, usage))
	}
	if err := db.ReplaceOpenCodeUsageActivity(runtime.SessionID, activity, time.Now()); err != nil {
		slog.Debug("OpenCode usage activity backfill could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.bySession == nil {
		openCodeVirtualCostState.bySession = map[string]map[string]openCodeProjectedMessageCost{}
	}
	openCodeVirtualCostState.bySession[runtime.SessionID] = messages
	openCodeVirtualCostState.Unlock()
	persistOpenCodeVirtualCostState(runtime, messages)
}

// backfillOpenCodeContextUsage seeds the context snapshot from the
// conversation's message history on SSE (re)connect, so a resumed session or a
// daemon restart shows correct context immediately rather than only after its
// next live turn — the OpenCode analog of Codex's read-through refresh. The
// most-recent assistant turn is selected by `time.created` (not slice position,
// which the endpoint does not guarantee) and funnelled through the same
// persistOpenCodeContextUsage path the live stream uses. Cost is intentionally
// not backfilled: it rides session.updated and already persists in cost_usd
// across restarts. Best-effort — it never blocks or fails the stream.
func backfillOpenCodeContextUsage(ctx context.Context, runtime db.OpenCodeRuntime) {
	if runtime.ConvID == "" {
		return
	}
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/message?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		slog.Debug("OpenCode context backfill request could not be built",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		return
	}
	response, err := openCodeConfigHTTPClient.Do(request.WithContext(ctx))
	if err != nil {
		slog.Debug("OpenCode context backfill fetch failed",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		slog.Debug("OpenCode context backfill returned non-200",
			"session", runtime.SessionID, "status", response.StatusCode, "module", "agentd")
		return
	}
	var messages []openCodeHistoryMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(&messages); err != nil {
		slog.Debug("OpenCode context backfill decode failed",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		return
	}

	var (
		latest   openCodeContextUsage
		latestAt int64
		haveAny  bool
		usages   []openCodeContextUsage
	)
	for _, m := range messages {
		if m.Info.Role != "assistant" {
			continue
		}
		usage := openCodeContextUsage{
			MessageID:    m.Info.ID,
			ProviderID:   m.Info.ProviderID,
			ModelID:      m.Info.ModelID,
			ReportedCost: m.Info.Cost,
			Input:        m.Info.Tokens.Input,
			Output:       m.Info.Tokens.Output,
			Reasoning:    m.Info.Tokens.Reasoning,
			CacheRead:    m.Info.Tokens.Cache.Read,
			CacheWrite:   m.Info.Tokens.Cache.Write,
		}
		if m.Info.Time.Created > 0 {
			usage.CreatedAt = time.UnixMilli(m.Info.Time.Created)
		}
		if usage.total() <= 0 {
			continue
		}
		usages = append(usages, usage)
		if !haveAny || m.Info.Time.Created >= latestAt {
			latest = usage
			latestAt = m.Info.Time.Created
			haveAny = true
		}
	}
	replaceOpenCodeVirtualCostUsage(ctx, runtime, usages)
	if !haveAny {
		return
	}
	persistOpenCodeContextUsage(ctx, runtime, latest)
	persistOpenCodeModelSlug(runtime, latest)
}
