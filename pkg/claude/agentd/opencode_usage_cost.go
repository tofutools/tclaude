package agentd

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// OpenCode virtual cost: per-message step usage, removals and their retries,
// and the projection of a session's cost (TCL-673; see opencode_usage.go).

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
	bySession       map[string]map[string]openCodeProjectedMessageCost
	usageSession    map[string]map[string]openCodeMessageCostUsage
	hydratedSession map[string]bool
	pendingRemovals map[string]map[string]openCodePendingRemoval
	knownSteps      map[string]map[string]map[string]struct{}
	snapshotSteps   map[string]map[string]map[string]struct{}
	removalRetries  map[openCodeRemovalRetryKey]struct{}
	// trackedSessions is the authoritative OpenCode session tree for one
	// tclaude root session. nativeCosts stores each tree node's cumulative real
	// cost so a later root session.updated cannot overwrite child spend.
	trackedSessions map[string]map[string]string
	nativeCosts     map[string]map[string]float64
	// retiredNativeCost compacts deleted descendants into one cumulative
	// scalar. Backfill reconstructs it from the persisted session total.
	retiredNativeCost map[string]float64
	// inheritedUntil caches each root session's inherited-history boundary
	// (db.ConvCostInheritedUntil); see openCodeInheritedUntil.
	inheritedUntil map[string]time.Time
}

type openCodePendingRemoval struct {
	removal   openCodeCostRemoval
	removedAt time.Time
}

type openCodeRemovalRetryKey struct {
	convID     string
	sessionID  string
	generation *openCodeProcess
}

var (
	markOpenCodePricingStepsRemoved  = db.MarkOpenCodePricingStepsRemoved
	clearOpenCodePricingStepsRemoved = db.ClearOpenCodePricingStepsRemoved
	afterOpenCodeStepMarkerClearTest func()
	openCodeRemovalRetryDelay        = openCodeSSERetryDelay
)

func clearOpenCodeVirtualCostState(sessionID string) {
	openCodeVirtualCostState.Lock()
	delete(openCodeVirtualCostState.bySession, sessionID)
	delete(openCodeVirtualCostState.usageSession, sessionID)
	delete(openCodeVirtualCostState.hydratedSession, sessionID)
	delete(openCodeVirtualCostState.trackedSessions, sessionID)
	delete(openCodeVirtualCostState.nativeCosts, sessionID)
	delete(openCodeVirtualCostState.retiredNativeCost, sessionID)
	delete(openCodeVirtualCostState.inheritedUntil, sessionID)
	openCodeVirtualCostState.Unlock()
}

// openCodeInheritedUntil returns the root conversation's inherited-history
// boundary: messages created at or before it were imported from another
// conversation (a clone or a bundle import) and their cost is that
// conversation's spend. The boundary is written before the conversation first
// runs, so it is read once per runtime.
func openCodeInheritedUntil(runtime db.OpenCodeRuntime) (time.Time, error) {
	openCodeVirtualCostState.Lock()
	until, cached := openCodeVirtualCostState.inheritedUntil[runtime.SessionID]
	openCodeVirtualCostState.Unlock()
	if cached {
		return until, nil
	}
	until, err := db.ConvCostInheritedUntil(runtime.ConvID)
	if err != nil {
		return time.Time{}, err
	}
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.inheritedUntil == nil {
		openCodeVirtualCostState.inheritedUntil = map[string]time.Time{}
	}
	openCodeVirtualCostState.inheritedUntil[runtime.SessionID] = until
	openCodeVirtualCostState.Unlock()
	return until, nil
}

// openCodeMessageInherited reports whether a root-session message predates the
// inherited-history boundary. Child sessions are never imported, and a
// message without a creation time is counted as the conversation's own.
func openCodeMessageInherited(runtime db.OpenCodeRuntime, usage openCodeContextUsage, until time.Time) bool {
	return !until.IsZero() && usage.SessionID == runtime.ConvID &&
		!usage.CreatedAt.IsZero() && !usage.CreatedAt.After(until)
}

func clearOpenCodeConversationStepState(convID string) {
	if convID == "" {
		return
	}
	openCodeVirtualCostState.Lock()
	delete(openCodeVirtualCostState.knownSteps, convID)
	delete(openCodeVirtualCostState.snapshotSteps, convID)
	openCodeVirtualCostState.Unlock()
}

func rememberOpenCodePendingRemoval(
	convID string,
	removal openCodeCostRemoval,
	removedAt time.Time,
) {
	if convID == "" || removal.MessageID == "" {
		return
	}
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.pendingRemovals == nil {
		openCodeVirtualCostState.pendingRemovals =
			map[string]map[string]openCodePendingRemoval{}
	}
	pending := openCodeVirtualCostState.pendingRemovals[convID]
	if pending == nil {
		pending = map[string]openCodePendingRemoval{}
		openCodeVirtualCostState.pendingRemovals[convID] = pending
	}
	pending[removal.MessageID] = openCodePendingRemoval{
		removal: removal, removedAt: removedAt,
	}
	openCodeVirtualCostState.Unlock()
}

func takeOpenCodePendingRemoval(convID, messageID string) bool {
	openCodeVirtualCostState.Lock()
	found := false
	if pending := openCodeVirtualCostState.pendingRemovals[convID]; pending != nil {
		if _, found = pending[messageID]; found {
			delete(pending, messageID)
		}
		if len(pending) == 0 {
			delete(openCodeVirtualCostState.pendingRemovals, convID)
		}
	}
	openCodeVirtualCostState.Unlock()
	return found
}

func hasOpenCodePendingRemoval(convID, messageID string) bool {
	openCodeVirtualCostState.Lock()
	_, found := openCodeVirtualCostState.pendingRemovals[convID][messageID]
	openCodeVirtualCostState.Unlock()
	return found
}

func hasAnyOpenCodePendingRemoval(convID string) bool {
	openCodeVirtualCostState.Lock()
	found := len(openCodeVirtualCostState.pendingRemovals[convID]) > 0
	openCodeVirtualCostState.Unlock()
	return found
}

func forgetAllOpenCodePendingRemovals(convID string) {
	openCodeVirtualCostState.Lock()
	delete(openCodeVirtualCostState.pendingRemovals, convID)
	openCodeVirtualCostState.Unlock()
}

func rememberOpenCodeKnownStepLocked(convID, messageID, partID string) {
	if convID == "" || messageID == "" || partID == "" {
		return
	}
	if openCodeVirtualCostState.knownSteps == nil {
		openCodeVirtualCostState.knownSteps =
			map[string]map[string]map[string]struct{}{}
	}
	messages := openCodeVirtualCostState.knownSteps[convID]
	if messages == nil {
		messages = map[string]map[string]struct{}{}
		openCodeVirtualCostState.knownSteps[convID] = messages
	}
	parts := messages[messageID]
	if parts == nil {
		parts = map[string]struct{}{}
		messages[messageID] = parts
	}
	parts[partID] = struct{}{}
}

func forgetOpenCodeKnownStepLocked(convID, messageID, partID string) {
	messages := openCodeVirtualCostState.knownSteps[convID]
	parts := messages[messageID]
	delete(parts, partID)
	if len(parts) == 0 {
		delete(messages, messageID)
	}
	if len(messages) == 0 {
		delete(openCodeVirtualCostState.knownSteps, convID)
	}
}

func forgetOpenCodeKnownMessageLocked(convID, messageID string) {
	messages := openCodeVirtualCostState.knownSteps[convID]
	delete(messages, messageID)
	if len(messages) == 0 {
		delete(openCodeVirtualCostState.knownSteps, convID)
	}
}

func rememberOpenCodeSnapshotStepLocked(convID, messageID, partID string) {
	if openCodeVirtualCostState.snapshotSteps == nil {
		openCodeVirtualCostState.snapshotSteps =
			map[string]map[string]map[string]struct{}{}
	}
	messages := openCodeVirtualCostState.snapshotSteps[convID]
	if messages == nil {
		messages = map[string]map[string]struct{}{}
		openCodeVirtualCostState.snapshotSteps[convID] = messages
	}
	parts := messages[messageID]
	if parts == nil {
		parts = map[string]struct{}{}
		messages[messageID] = parts
	}
	parts[partID] = struct{}{}
}

func forgetOpenCodeSnapshotStepLocked(convID, messageID, partID string) {
	messages := openCodeVirtualCostState.snapshotSteps[convID]
	parts := messages[messageID]
	delete(parts, partID)
	if len(parts) == 0 {
		delete(messages, messageID)
	}
	if len(messages) == 0 {
		delete(openCodeVirtualCostState.snapshotSteps, convID)
	}
}

func forgetOpenCodeSnapshotMessageLocked(convID, messageID string) {
	messages := openCodeVirtualCostState.snapshotSteps[convID]
	delete(messages, messageID)
	if len(messages) == 0 {
		delete(openCodeVirtualCostState.snapshotSteps, convID)
	}
}

func retryOpenCodePendingRemovals(ctx context.Context, runtime db.OpenCodeRuntime) bool {
	openCodeVirtualCostState.Lock()
	pending := make(
		map[string]openCodePendingRemoval,
		len(openCodeVirtualCostState.pendingRemovals[runtime.ConvID]),
	)
	for messageID, removal := range openCodeVirtualCostState.pendingRemovals[runtime.ConvID] {
		pending[messageID] = removal
	}
	openCodeVirtualCostState.Unlock()
	allPersisted := true
	for messageID, pendingRemoval := range pending {
		if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
			return false
		}
		if err := markOpenCodePricingStepsRemoved(
			runtime.ConvID, runtime.SessionID, messageID, pendingRemoval.removedAt,
		); err != nil {
			allPersisted = false
		}
	}
	return allPersisted
}

func scheduleOpenCodeRemovalRetry(ctx context.Context, runtime db.OpenCodeRuntime) {
	key := openCodeRemovalRetryKey{
		convID: runtime.ConvID, sessionID: runtime.SessionID,
	}
	key.generation, _ = ctx.Value(openCodeSSEGenerationKey{}).(*openCodeProcess)
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.removalRetries == nil {
		openCodeVirtualCostState.removalRetries =
			map[openCodeRemovalRetryKey]struct{}{}
	}
	if _, scheduled := openCodeVirtualCostState.removalRetries[key]; scheduled {
		openCodeVirtualCostState.Unlock()
		return
	}
	openCodeVirtualCostState.removalRetries[key] = struct{}{}
	openCodeVirtualCostState.Unlock()
	go func() {
		defer func() {
			openCodeVirtualCostState.Lock()
			delete(openCodeVirtualCostState.removalRetries, key)
			openCodeVirtualCostState.Unlock()
		}()
		for hasAnyOpenCodePendingRemoval(runtime.ConvID) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(openCodeRemovalRetryDelay):
			}
			applied := false
			if !withOpenCodeProjectorApplyLock(ctx, runtime, func() {
				if openCodeProjectorCurrent(ctx, runtime.SessionID) {
					applied = backfillOpenCodeContextUsage(ctx, runtime)
				}
			}) && ctx.Err() != nil {
				return
			}
			if applied {
				return
			}
		}
	}()
}

func openCodeVirtualCostHydrated(sessionID string) bool {
	openCodeVirtualCostState.Lock()
	defer openCodeVirtualCostState.Unlock()
	return openCodeVirtualCostState.hydratedSession[sessionID]
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
// fallback only when no explicit tier matches and the per-call context exceeds
// legacyLongContextCutoff.
func openCodeVirtualCostForUsage(
	usage openCodeContextUsage,
	base openCodeModelPrice,
	legacyLongContextCutoff int64,
) (float64, bool) {
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
	if !matched && contextTokens > legacyLongContextCutoff && base.ExperimentalOver200K != nil {
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

func projectOpenCodeMessageCost(
	usage openCodeContextUsage,
	prices map[string]openCodeModelPrice,
	legacyLongContextCutoff int64,
) openCodeProjectedMessageCost {
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
	usd, ok := openCodeVirtualCostForUsage(usage, price, legacyLongContextCutoff)
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

func openCodeMessageUsageRealCost(state openCodeMessageCostUsage) float64 {
	messageCost := 0.0
	if state.message.ReportedCost != nil && *state.message.ReportedCost > 0 {
		messageCost = *state.message.ReportedCost
	}
	stepCost := 0.0
	for _, step := range state.steps {
		if step.ReportedCost != nil && *step.ReportedCost > 0 {
			stepCost += *step.ReportedCost
		}
	}
	return math.Max(messageCost, stepCost)
}

func projectOpenCodeMessageCostUsage(
	state openCodeMessageCostUsage,
	prices map[string]openCodeModelPrice,
	legacyLongContextCutoff int64,
) openCodeProjectedMessageCost {
	usage := aggregateOpenCodeMessageCostUsage(state)
	if !state.hadSteps {
		return projectOpenCodeMessageCost(usage, prices, legacyLongContextCutoff)
	}
	if usage.ReportedCost == nil || *usage.ReportedCost != 0 || usage.MessageID == "" {
		return projectOpenCodeMessageCost(usage, prices, legacyLongContextCutoff)
	}
	total := 0.0
	// Sorted for the same reason as the messages in the projection: the sum must
	// not change with map order.
	for _, key := range slices.Sorted(maps.Keys(state.steps)) {
		step := state.steps[key]
		step.ProviderID = usage.ProviderID
		step.ModelID = usage.ModelID
		projected := projectOpenCodeMessageCost(step, prices, legacyLongContextCutoff)
		if !projected.eligible {
			return projected
		}
		total += projected.usd
	}
	return openCodeProjectedMessageCost{usd: total, eligible: true}
}

func openCodeLegacyLongContextPricingCutoff() int64 {
	cfg, _ := config.Load()
	return cfg.ResolvedOpenCodeLegacyLongContextPricingCutoff()
}

func openCodeMessageUsageIsSubscription(state openCodeMessageCostUsage) bool {
	usage := aggregateOpenCodeMessageCostUsage(state)
	return usage.total() > 0 && usage.ReportedCost != nil && *usage.ReportedCost == 0
}

func openCodeActivityForUsage(runtime db.OpenCodeRuntime, usage openCodeContextUsage) db.OpenCodeUsageActivity {
	observedAt := usage.CreatedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	return db.OpenCodeUsageActivity{
		SessionID: openCodeTelemetrySessionID(runtime), MessageID: usage.MessageID, ConvID: runtime.ConvID,
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

func waitForOpenCodeCostSessionRow(ctx context.Context, runtime db.OpenCodeRuntime) (string, bool) {
	deadline := time.Now().Add(openCodeHookRowWait)
	for {
		sessionID := openCodeTelemetrySessionID(runtime)
		exists, err := db.SessionExists(sessionID)
		if err != nil {
			slog.Debug("OpenCode virtual cost session lookup failed",
				"session", sessionID, "error", err, "module", "agentd")
			return "", false
		}
		if exists {
			return sessionID, true
		}
		if time.Now().After(deadline) {
			slog.Debug("OpenCode virtual cost session row did not appear before timeout",
				"session", runtime.SessionID, "conv_id", runtime.ConvID, "module", "agentd")
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(openCodeHookRowRetryDelay):
		}
	}
}

func projectAndPersistOpenCodeCostState(ctx context.Context, runtime db.OpenCodeRuntime) {
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// Resume launches start the managed runtime and its SSE consumer before the
	// child `session new` process inserts the local session row. Keep the
	// recovered usage state in memory while waiting for that row, otherwise the
	// first authoritative backfill can become a silent UPDATE/INSERT no-op.
	sessionID, found := waitForOpenCodeCostSessionRow(ctx, runtime)
	if !found {
		return
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	inheritedUntil, err := openCodeInheritedUntil(runtime)
	if err != nil {
		// Without the boundary an imported history would be counted as this
		// conversation's spend, and real cost never decreases once written.
		slog.Warn("OpenCode inherited-history boundary could not be read",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		return
	}
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	states := make([]openCodeMessageCostUsage, 0, len(usages))
	realCost := 0.0
	for _, state := range usages {
		if openCodeMessageInherited(runtime, state.message, inheritedUntil) {
			continue
		}
		realCost += openCodeMessageUsageRealCost(state)
		states = append(states, state)
	}
	openCodeVirtualCostState.Unlock()
	if realCost > 0 {
		if err := persistOpenCodeRealCost(runtime, realCost); err != nil {
			slog.Warn("OpenCode native message cost could not be persisted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
			return
		}
		if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
			return
		}
		openCodeVirtualCostState.Lock()
		openCodeVirtualCostState.bySession[runtime.SessionID] = map[string]openCodeProjectedMessageCost{}
		openCodeVirtualCostState.Unlock()
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
	legacyLongContextCutoff := openCodeLegacyLongContextPricingCutoff()
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}

	// usages is a map, and a float sum depends on its order. A fixed order keeps
	// an old day's cumulative identical across flushes, so rewriting it changes
	// nothing and the stored cost opening survives (see db.CostOpening).
	sort.Slice(states, func(i, j int) bool {
		a, b := states[i].message, states[j].message
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.MessageID < b.MessageID
	})
	projections := make(map[string]openCodeProjectedMessageCost, len(states))
	type dailyContribution struct {
		usd       float64
		updatedAt time.Time
		model     string
	}
	byDay := make(map[string]dailyContribution)
	total := 0.0
	haveIneligible := false
	for _, state := range states {
		usage := aggregateOpenCodeMessageCostUsage(state)
		projected := projectOpenCodeMessageCostUsage(state, prices, legacyLongContextCutoff)
		projections[usage.MessageID] = projected
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
	if haveIneligible {
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
	if err := db.ReplaceSessionVirtualCostHistory(sessionID, total, snapshots); err != nil {
		slog.Warn("OpenCode virtual cost could not be persisted",
			"session", sessionID, "error", err, "module", "agentd")
		return
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
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
	if usage.MessageID == "" || !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	if !openCodeVirtualCostHydrated(runtime.SessionID) &&
		!backfillOpenCodeContextUsage(ctx, runtime) {
		// A partial in-memory state must never replace retained authoritative
		// history. Retry hydration on the next live event or SSE reconnect.
		return
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	rememberOpenCodeTrackedSession(runtime, usage.SessionID)
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	state := usages[usage.MessageID]
	state.message = usage
	usages[usage.MessageID] = state
	openCodeVirtualCostState.Unlock()
	if openCodeMessageUsageIsSubscription(state) {
		if err := db.UpsertOpenCodeUsageActivity(openCodeActivityForUsage(runtime, usage)); err != nil {
			slog.Debug("OpenCode usage activity could not be persisted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
		}
	} else if err := db.DeleteOpenCodeUsageActivity(
		runtime.ConvID, runtime.SessionID, usage.MessageID,
	); err != nil {
		slog.Debug("OpenCode non-subscription activity could not be cleared",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	projectAndPersistOpenCodeCostState(ctx, runtime)
}

// applyOpenCodeVirtualCostStep replaces one model-call part by stable part ID.
// OpenCode emits the part before its corresponding message update; in that
// order it is retained until the message supplies provider/model metadata.
func applyOpenCodeVirtualCostStep(ctx context.Context, runtime db.OpenCodeRuntime, step openCodeStepCostUsage) {
	if step.PartID == "" || step.Usage.MessageID == "" ||
		!openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	if !openCodeVirtualCostHydrated(runtime.SessionID) &&
		!backfillOpenCodeContextUsage(ctx, runtime) {
		return
	}
	if err := clearOpenCodePricingStepsRemoved(runtime.ConvID, step.Usage.MessageID); err != nil {
		slog.Debug("OpenCode pricing-step removal marker could not be cleared",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	replacedPendingRemoval := takeOpenCodePendingRemoval(runtime.ConvID, step.Usage.MessageID)
	if afterOpenCodeStepMarkerClearTest != nil {
		afterOpenCodeStepMarkerClearTest()
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	rememberOpenCodeTrackedSession(runtime, step.Usage.SessionID)
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	state := usages[step.Usage.MessageID]
	if replacedPendingRemoval {
		// A later eligible step supersedes a pending final-step tombstone. Drop
		// the preserved removed step before adding the new authoritative one.
		state.steps = nil
		forgetOpenCodeKnownMessageLocked(runtime.ConvID, step.Usage.MessageID)
		forgetOpenCodeSnapshotMessageLocked(runtime.ConvID, step.Usage.MessageID)
	}
	if state.steps == nil {
		state.steps = map[string]openCodeContextUsage{}
	}
	state.hadSteps = true
	state.steps[step.PartID] = step.Usage
	rememberOpenCodeKnownStepLocked(
		runtime.ConvID, step.Usage.MessageID, step.PartID,
	)
	rememberOpenCodeSnapshotStepLocked(
		runtime.ConvID, step.Usage.MessageID, step.PartID,
	)
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
	if removal.MessageID == "" || !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	if !openCodeVirtualCostHydrated(runtime.SessionID) &&
		!backfillOpenCodeContextUsage(ctx, runtime) {
		return
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	messageRemoved := removal.PartID == ""
	finalStepRemoved := false
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	if messageRemoved {
		delete(usages, removal.MessageID)
		forgetOpenCodeKnownMessageLocked(runtime.ConvID, removal.MessageID)
		forgetOpenCodeSnapshotMessageLocked(runtime.ConvID, removal.MessageID)
	} else {
		state, haveState := usages[removal.MessageID]
		knownParts := openCodeVirtualCostState.knownSteps[runtime.ConvID][removal.MessageID]
		if len(knownParts) == 0 {
			for partID := range state.steps {
				rememberOpenCodeKnownStepLocked(runtime.ConvID, removal.MessageID, partID)
				rememberOpenCodeSnapshotStepLocked(runtime.ConvID, removal.MessageID, partID)
			}
		}
		knownParts = openCodeVirtualCostState.knownSteps[runtime.ConvID][removal.MessageID]
		snapshotParts := openCodeVirtualCostState.snapshotSteps[runtime.ConvID][removal.MessageID]
		for partID := range knownParts {
			if partID != removal.PartID {
				if _, presentInSnapshot := snapshotParts[partID]; !presentInSnapshot {
					// Parts absent before this stream opened cannot have a
					// buffered removal. Prune them before classifying the
					// current event; retain the current part itself because its
					// removal may have raced the snapshot fetch.
					forgetOpenCodeKnownStepLocked(runtime.ConvID, removal.MessageID, partID)
				}
			}
		}
		knownParts = openCodeVirtualCostState.knownSteps[runtime.ConvID][removal.MessageID]
		if _, knownStep := knownParts[removal.PartID]; !knownStep {
			openCodeVirtualCostState.Unlock()
			return
		}
		finalStepRemoved = len(knownParts) == 1
		if !finalStepRemoved {
			forgetOpenCodeKnownStepLocked(runtime.ConvID, removal.MessageID, removal.PartID)
			forgetOpenCodeSnapshotStepLocked(runtime.ConvID, removal.MessageID, removal.PartID)
			if haveState {
				delete(state.steps, removal.PartID)
				state.hadSteps = true
				usages[removal.MessageID] = state
			}
		}
	}
	openCodeVirtualCostState.Unlock()
	if finalStepRemoved {
		// The history API cannot reconstruct a final-step removal. Persist that
		// fact before changing memory; a transient write failure leaves the old
		// projection visible and allows a replay to retry instead of clearing it
		// only to resurrect it later.
		removedAt := time.Now()
		if err := markOpenCodePricingStepsRemoved(
			runtime.ConvID, runtime.SessionID, removal.MessageID, removedAt,
		); err != nil {
			slog.Debug("OpenCode final-step removal could not be persisted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
			rememberOpenCodePendingRemoval(runtime.ConvID, removal, removedAt)
			scheduleOpenCodeRemovalRetry(ctx, runtime)
			return
		}
		takeOpenCodePendingRemoval(runtime.ConvID, removal.MessageID)
		if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
			return
		}
		openCodeVirtualCostState.Lock()
		_, usages = ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
		state, ok := usages[removal.MessageID]
		forgetOpenCodeKnownStepLocked(runtime.ConvID, removal.MessageID, removal.PartID)
		forgetOpenCodeSnapshotStepLocked(runtime.ConvID, removal.MessageID, removal.PartID)
		if ok {
			delete(state.steps, removal.PartID)
			state.hadSteps = true
			usages[removal.MessageID] = state
		}
		openCodeVirtualCostState.Unlock()
	}
	if messageRemoved {
		if err := db.DeleteOpenCodeUsageActivity(
			runtime.ConvID, runtime.SessionID, removal.MessageID,
		); err != nil {
			slog.Debug("OpenCode removed-message activity could not be deleted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
		}
		if err := clearOpenCodePricingStepsRemoved(runtime.ConvID, removal.MessageID); err != nil {
			slog.Debug("OpenCode removed-message pricing marker could not be deleted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
		}
		takeOpenCodePendingRemoval(runtime.ConvID, removal.MessageID)
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
		if openCodeMessageUsageIsSubscription(state) {
			activity = append(activity, openCodeActivityForUsage(runtime, state.message))
		}
		usageState[usage.MessageID] = state
	}
	if err := db.ReplaceOpenCodeUsageActivity(
		openCodeTelemetrySessionID(runtime), runtime.ConvID, activity, time.Now(),
	); err != nil {
		slog.Debug("OpenCode usage activity backfill could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	openCodeVirtualCostState.Lock()
	ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	openCodeVirtualCostState.bySession[runtime.SessionID] = map[string]openCodeProjectedMessageCost{}
	openCodeVirtualCostState.usageSession[runtime.SessionID] = usageState
	if openCodeVirtualCostState.hydratedSession == nil {
		openCodeVirtualCostState.hydratedSession = map[string]bool{}
	}
	openCodeVirtualCostState.hydratedSession[runtime.SessionID] = true
	openCodeVirtualCostState.Unlock()
	projectAndPersistOpenCodeCostState(ctx, runtime)
}
