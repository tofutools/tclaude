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
	"sort"
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
	Parts []struct {
		ID        string                       `json:"id"`
		MessageID string                       `json:"messageID"`
		SessionID string                       `json:"sessionID"`
		Type      string                       `json:"type"`
		Cost      *float64                     `json:"cost"`
		Tokens    openCodeMessageTokensPayload `json:"tokens"`
	} `json:"parts"`
}

type openCodeStepCostUsage struct {
	PartID string
	Usage  openCodeContextUsage
}

type openCodeStepUpdatedEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Part      struct {
			ID        string                       `json:"id"`
			MessageID string                       `json:"messageID"`
			SessionID string                       `json:"sessionID"`
			Type      string                       `json:"type"`
			Cost      *float64                     `json:"cost"`
			Tokens    openCodeMessageTokensPayload `json:"tokens"`
		} `json:"part"`
	} `json:"properties"`
}

type openCodeCostRemoval struct {
	MessageID string
	PartID    string
}

type openCodeRemovedEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		PartID    string `json:"partID"`
	} `json:"properties"`
}

func parseOpenCodeCostRemoval(event json.RawMessage, convID string) (openCodeCostRemoval, bool) {
	if convID == "" || !bytes.Contains(event, []byte(`.removed"`)) {
		return openCodeCostRemoval{}, false
	}
	var decoded openCodeRemovedEvent
	if json.Unmarshal(event, &decoded) != nil || decoded.Properties.SessionID != convID ||
		decoded.Properties.MessageID == "" {
		return openCodeCostRemoval{}, false
	}
	switch decoded.Type {
	case "message.removed":
		return openCodeCostRemoval{MessageID: decoded.Properties.MessageID}, true
	case "message.part.removed":
		if decoded.Properties.PartID != "" {
			return openCodeCostRemoval{
				MessageID: decoded.Properties.MessageID,
				PartID:    decoded.Properties.PartID,
			}, true
		}
	}
	return openCodeCostRemoval{}, false
}

// parseOpenCodeStepCostUsage extracts OpenCode's per-model-call usage. An
// AssistantMessage can contain several step-finish parts when a turn calls
// tools; its top-level tokens field contains only the latest step even though
// its cost is cumulative, so WHAT-IF pricing must aggregate these parts.
func parseOpenCodeStepCostUsage(event json.RawMessage, convID string) (openCodeStepCostUsage, bool) {
	if convID == "" || !bytes.Contains(event, []byte(`"message.part.updated"`)) ||
		!bytes.Contains(event, []byte(`"step-finish"`)) {
		return openCodeStepCostUsage{}, false
	}
	var decoded openCodeStepUpdatedEvent
	if json.Unmarshal(event, &decoded) != nil || decoded.Type != "message.part.updated" {
		return openCodeStepCostUsage{}, false
	}
	part := decoded.Properties.Part
	sessionID := part.SessionID
	if sessionID == "" {
		sessionID = decoded.Properties.SessionID
	}
	if sessionID != convID || part.Type != "step-finish" || part.ID == "" || part.MessageID == "" {
		return openCodeStepCostUsage{}, false
	}
	usage := openCodeContextUsage{
		MessageID: part.MessageID, ReportedCost: part.Cost,
		Input: part.Tokens.Input, Output: part.Tokens.Output, Reasoning: part.Tokens.Reasoning,
		CacheRead: part.Tokens.Cache.Read, CacheWrite: part.Tokens.Cache.Write,
	}
	if usage.total() <= 0 {
		return openCodeStepCostUsage{}, false
	}
	return openCodeStepCostUsage{PartID: part.ID, Usage: usage}, true
}

type openCodeProjectedMessageCost struct {
	usd      float64
	eligible bool
	real     bool
}

type openCodeMessageCostUsage struct {
	message  openCodeContextUsage
	steps    map[string]openCodeContextUsage
	hadSteps bool
}

var openCodeVirtualCostState struct {
	sync.Mutex
	bySession    map[string]map[string]openCodeProjectedMessageCost
	usageSession map[string]map[string]openCodeMessageCostUsage
}

func clearOpenCodeVirtualCostState(sessionID string) {
	openCodeVirtualCostState.Lock()
	delete(openCodeVirtualCostState.bySession, sessionID)
	delete(openCodeVirtualCostState.usageSession, sessionID)
	openCodeVirtualCostState.Unlock()
}

func validOpenCodeRate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validOpenCodePrice(price openCodeModelPrice) bool {
	return validOpenCodeRate(price.Input) && validOpenCodeRate(price.Output) &&
		validOpenCodeRate(price.Cache.Read) && validOpenCodeRate(price.Cache.Write)
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
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
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

func aggregateOpenCodeMessageCostUsage(state openCodeMessageCostUsage) openCodeContextUsage {
	usage := state.message
	if !state.hadSteps {
		return usage
	}
	usage.Input, usage.Output, usage.Reasoning, usage.CacheRead, usage.CacheWrite = 0, 0, 0, 0, 0
	allCostsKnown := true
	reportedCost := 0.0
	for _, step := range state.steps {
		usage.Input += step.Input
		usage.Output += step.Output
		usage.Reasoning += step.Reasoning
		usage.CacheRead += step.CacheRead
		usage.CacheWrite += step.CacheWrite
		if step.ReportedCost == nil {
			allCostsKnown = false
		} else {
			reportedCost += *step.ReportedCost
		}
	}
	if allCostsKnown {
		usage.ReportedCost = &reportedCost
	}
	return usage
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

func ensureOpenCodeVirtualCostStateLocked(sessionID string) (
	map[string]openCodeProjectedMessageCost,
	map[string]openCodeMessageCostUsage,
) {
	if openCodeVirtualCostState.bySession == nil {
		openCodeVirtualCostState.bySession = map[string]map[string]openCodeProjectedMessageCost{}
	}
	if openCodeVirtualCostState.usageSession == nil {
		openCodeVirtualCostState.usageSession = map[string]map[string]openCodeMessageCostUsage{}
	}
	messages := openCodeVirtualCostState.bySession[sessionID]
	if messages == nil {
		messages = map[string]openCodeProjectedMessageCost{}
		openCodeVirtualCostState.bySession[sessionID] = messages
	}
	usages := openCodeVirtualCostState.usageSession[sessionID]
	if usages == nil {
		usages = map[string]openCodeMessageCostUsage{}
		openCodeVirtualCostState.usageSession[sessionID] = usages
	}
	return messages, usages
}

func waitForOpenCodeCostSessionRow(ctx context.Context, sessionID string) bool {
	deadline := time.Now().Add(openCodeHookRowWait)
	for {
		exists, err := db.SessionExists(sessionID)
		if err != nil {
			slog.Debug("OpenCode virtual cost session lookup failed",
				"session", sessionID, "error", err, "module", "agentd")
			return false
		}
		if exists {
			return true
		}
		if time.Now().After(deadline) {
			slog.Debug("OpenCode virtual cost session row did not appear before timeout",
				"session", sessionID, "module", "agentd")
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(openCodeHookRowRetryDelay):
		}
	}
}

func projectAndPersistOpenCodeCostState(ctx context.Context, runtime db.OpenCodeRuntime) {
	// Resume launches start the managed runtime and its SSE consumer before the
	// child `session new` process inserts the local session row. Keep the
	// recovered usage state in memory while waiting for that row, otherwise the
	// first authoritative backfill can become a silent UPDATE/INSERT no-op.
	if !waitForOpenCodeCostSessionRow(ctx, runtime.SessionID) {
		return
	}
	prices, loaded := openCodeModelPrices(ctx, runtime)
	if !loaded {
		// A transient catalog failure is not an authoritative statement that
		// pricing disappeared. Retain both the recovered usage state and the
		// last persisted history so a later successful fetch can recompute all
		// messages without moving old spend into today.
		return
	}
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	aggregated := make([]openCodeContextUsage, 0, len(usages))
	for _, state := range usages {
		aggregated = append(aggregated, aggregateOpenCodeMessageCostUsage(state))
	}
	openCodeVirtualCostState.Unlock()

	projections := make(map[string]openCodeProjectedMessageCost, len(aggregated))
	type dailyContribution struct {
		usd       float64
		updatedAt time.Time
		model     string
	}
	byDay := make(map[string]dailyContribution)
	total := 0.0
	haveReal := false
	haveIneligible := false
	for _, usage := range aggregated {
		projected := projectOpenCodeMessageCost(usage, prices)
		projections[usage.MessageID] = projected
		if projected.real {
			haveReal = true
			continue
		}
		if !projected.eligible {
			// A successfully loaded catalog can still omit one model. Do not
			// publish a deceptively partial total or erase the last complete
			// projection; wait for authoritative pricing or real cost.
			haveIneligible = true
			continue
		}
		observedAt := usage.CreatedAt
		if observedAt.IsZero() {
			observedAt = time.Now()
		}
		day := observedAt.In(time.Local).Format("2006-01-02")
		contribution := byDay[day]
		contribution.usd += projected.usd
		model := usage.ProviderID + "/" + usage.ModelID
		if observedAt.After(contribution.updatedAt) ||
			(observedAt.Equal(contribution.updatedAt) && model > contribution.model) {
			contribution.updatedAt = observedAt
			contribution.model = model
		}
		byDay[day] = contribution
		total += projected.usd
	}
	if haveReal {
		total = 0
		byDay = map[string]dailyContribution{}
	} else if haveIneligible {
		return
	}
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)
	snapshots := make([]db.VirtualCostDailySnapshot, 0, len(days))
	cumulative := 0.0
	for _, day := range days {
		contribution := byDay[day]
		cumulative += contribution.usd
		snapshots = append(snapshots, db.VirtualCostDailySnapshot{
			Day: day, CostUSD: cumulative, UpdatedAt: contribution.updatedAt, Model: contribution.model,
		})
	}
	if err := db.ReplaceSessionVirtualCostHistory(runtime.SessionID, total, snapshots); err != nil {
		slog.Warn("OpenCode virtual cost could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		return
	}
	openCodeVirtualCostState.Lock()
	openCodeVirtualCostState.bySession[runtime.SessionID] = projections
	openCodeVirtualCostState.Unlock()
}

// applyOpenCodeVirtualCostUsage replaces one message's metadata and projection.
// Replayed SSE updates therefore converge instead of incrementing, while model
// changes replace the old model's price contribution. When step-finish parts
// have arrived, their sum replaces the top-level latest-step token block.
func applyOpenCodeVirtualCostUsage(ctx context.Context, runtime db.OpenCodeRuntime, usage openCodeContextUsage) {
	if usage.MessageID == "" {
		return
	}
	if err := db.UpsertOpenCodeUsageActivity(openCodeActivityForUsage(runtime, usage)); err != nil {
		slog.Debug("OpenCode usage activity could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	state := usages[usage.MessageID]
	state.message = usage
	usages[usage.MessageID] = state
	openCodeVirtualCostState.Unlock()
	projectAndPersistOpenCodeCostState(ctx, runtime)
}

// applyOpenCodeVirtualCostStep replaces one model-call part by stable part ID.
// OpenCode emits the part before its corresponding message update; in that
// order it is retained until the message supplies provider/model metadata.
func applyOpenCodeVirtualCostStep(ctx context.Context, runtime db.OpenCodeRuntime, step openCodeStepCostUsage) {
	if step.PartID == "" || step.Usage.MessageID == "" {
		return
	}
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	state := usages[step.Usage.MessageID]
	if state.steps == nil {
		state.steps = map[string]openCodeContextUsage{}
	}
	state.hadSteps = true
	state.steps[step.PartID] = step.Usage
	usages[step.Usage.MessageID] = state
	haveMessage := state.message.MessageID != ""
	openCodeVirtualCostState.Unlock()
	if haveMessage {
		projectAndPersistOpenCodeCostState(ctx, runtime)
	}
}

func applyOpenCodeVirtualCostRemoval(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	removal openCodeCostRemoval,
) {
	if removal.MessageID == "" {
		return
	}
	messageRemoved := removal.PartID == ""
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	if messageRemoved {
		delete(usages, removal.MessageID)
	} else if state, ok := usages[removal.MessageID]; ok {
		delete(state.steps, removal.PartID)
		state.hadSteps = true
		usages[removal.MessageID] = state
	}
	openCodeVirtualCostState.Unlock()
	if messageRemoved {
		if err := db.DeleteOpenCodeUsageActivity(runtime.SessionID, removal.MessageID); err != nil {
			slog.Debug("OpenCode removed-message activity could not be deleted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
		}
	}
	projectAndPersistOpenCodeCostState(ctx, runtime)
}

func replaceOpenCodeVirtualCostUsage(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	usages []openCodeMessageCostUsage,
) {
	activity := make([]db.OpenCodeUsageActivity, 0, len(usages))
	usageState := make(map[string]openCodeMessageCostUsage, len(usages))
	for _, state := range usages {
		usage := aggregateOpenCodeMessageCostUsage(state)
		if usage.MessageID == "" {
			continue
		}
		activity = append(activity, openCodeActivityForUsage(runtime, state.message))
		usageState[usage.MessageID] = state
	}
	if err := db.ReplaceOpenCodeUsageActivity(runtime.SessionID, activity, time.Now()); err != nil {
		slog.Debug("OpenCode usage activity backfill could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	openCodeVirtualCostState.Lock()
	ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	openCodeVirtualCostState.bySession[runtime.SessionID] = map[string]openCodeProjectedMessageCost{}
	openCodeVirtualCostState.usageSession[runtime.SessionID] = usageState
	openCodeVirtualCostState.Unlock()
	projectAndPersistOpenCodeCostState(ctx, runtime)
}

// backfillOpenCodeContextUsage seeds the context snapshot from the
// conversation's message history on SSE (re)connect, so a resumed session or a
// daemon restart shows correct context immediately rather than only after its
// next live turn — the OpenCode analog of Codex's read-through refresh. The
// most-recent assistant turn is selected by `time.created` (not slice position,
// which the endpoint does not guarantee) and funnelled through the same
// persistOpenCodeContextUsage path the live stream uses. Historical assistant
// costs are summed as well: this recovers real spend when a session.updated
// event was missed during a disconnect. Best-effort — it never fails the
// stream.
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
		latest     openCodeContextUsage
		latestAt   int64
		haveAny    bool
		costUsages []openCodeMessageCostUsage
		realCost   float64
	)
	for _, m := range messages {
		if m.Info.Role != "assistant" {
			continue
		}
		if m.Info.Cost != nil && *m.Info.Cost > 0 {
			realCost += *m.Info.Cost
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
		costUsage := openCodeMessageCostUsage{
			message: usage,
			steps:   map[string]openCodeContextUsage{},
		}
		for _, part := range m.Parts {
			if part.Type != "step-finish" || part.ID == "" {
				continue
			}
			messageID := part.MessageID
			if messageID == "" {
				messageID = usage.MessageID
			}
			if messageID != usage.MessageID {
				continue
			}
			step := openCodeContextUsage{
				MessageID: messageID, ReportedCost: part.Cost,
				Input: part.Tokens.Input, Output: part.Tokens.Output, Reasoning: part.Tokens.Reasoning,
				CacheRead: part.Tokens.Cache.Read, CacheWrite: part.Tokens.Cache.Write,
			}
			if step.total() > 0 {
				costUsage.steps[part.ID] = step
				costUsage.hadSteps = true
			}
		}
		costUsages = append(costUsages, costUsage)
		if !haveAny || m.Info.Time.Created >= latestAt {
			latest = usage
			latestAt = m.Info.Time.Created
			haveAny = true
		}
	}
	replaceOpenCodeVirtualCostUsage(ctx, runtime, costUsages)
	if realCost > 0 && waitForOpenCodeCostSessionRow(ctx, runtime.SessionID) {
		if err := db.UpdateSessionCost(runtime.SessionID, realCost); err != nil {
			slog.Warn("OpenCode real cost backfill could not be persisted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
		}
	}
	if !haveAny {
		return
	}
	persistOpenCodeContextUsage(ctx, runtime, latest)
	persistOpenCodeModelSlug(runtime, latest)
}
