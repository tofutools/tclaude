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

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
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

type openCodeSessionTreeEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		} `json:"info"`
	} `json:"properties"`
}

func ensureOpenCodeTrackedSessionsLocked(runtime db.OpenCodeRuntime) map[string]string {
	if openCodeVirtualCostState.trackedSessions == nil {
		openCodeVirtualCostState.trackedSessions = map[string]map[string]string{}
	}
	tracked := openCodeVirtualCostState.trackedSessions[runtime.SessionID]
	if tracked == nil {
		tracked = map[string]string{}
		openCodeVirtualCostState.trackedSessions[runtime.SessionID] = tracked
	}
	if runtime.ConvID != "" {
		tracked[runtime.ConvID] = ""
	}
	return tracked
}

func openCodeSessionTracked(runtime db.OpenCodeRuntime, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	openCodeVirtualCostState.Lock()
	tracked := ensureOpenCodeTrackedSessionsLocked(runtime)
	_, ok := tracked[sessionID]
	openCodeVirtualCostState.Unlock()
	return ok
}

func rememberOpenCodeTrackedSession(runtime db.OpenCodeRuntime, sessionID string) {
	if sessionID == "" {
		return
	}
	openCodeVirtualCostState.Lock()
	tracked := ensureOpenCodeTrackedSessionsLocked(runtime)
	if _, ok := tracked[sessionID]; !ok {
		tracked[sessionID] = runtime.ConvID
	}
	openCodeVirtualCostState.Unlock()
}

// observeOpenCodeSessionTree learns newly created descendants before the same
// directory-wide SSE event is routed through the cost parsers. OpenCode emits
// parentID on session.created/session.updated; reconnect hydration replaces
// this opportunistic tree from the authoritative children endpoint.
func observeOpenCodeSessionTree(runtime db.OpenCodeRuntime, event json.RawMessage) []string {
	if !bytes.Contains(event, []byte(`"session.`)) {
		return nil
	}
	var decoded openCodeSessionTreeEvent
	if json.Unmarshal(event, &decoded) != nil {
		return nil
	}
	id := decoded.Properties.Info.ID
	if id == "" {
		id = decoded.Properties.SessionID
	}
	if id == "" {
		return nil
	}
	var removedIDs []string
	openCodeVirtualCostState.Lock()
	tracked := ensureOpenCodeTrackedSessionsLocked(runtime)
	switch decoded.Type {
	case "session.created", "session.updated":
		if _, parentTracked := tracked[decoded.Properties.Info.ParentID]; decoded.Properties.Info.ParentID != "" && parentTracked {
			if _, exists := tracked[id]; exists || len(tracked) < maxOpenCodeTrackedSessions {
				tracked[id] = decoded.Properties.Info.ParentID
			}
		}
	case "session.deleted":
		if _, belongs := tracked[id]; belongs && id != runtime.ConvID {
			delete(tracked, id)
			removed := map[string]struct{}{id: {}}
			for changed := true; changed; {
				changed = false
				for childID, parentID := range tracked {
					if _, parentRemoved := removed[parentID]; parentRemoved {
						delete(tracked, childID)
						removed[childID] = struct{}{}
						changed = true
					}
				}
			}
			removedIDs = make([]string, 0, len(removed))
			for removedID := range removed {
				removedIDs = append(removedIDs, removedID)
			}
			if costs := openCodeVirtualCostState.nativeCosts[runtime.SessionID]; costs != nil {
				if openCodeVirtualCostState.retiredNativeCost == nil {
					openCodeVirtualCostState.retiredNativeCost = map[string]float64{}
				}
				for _, removedID := range removedIDs {
					openCodeVirtualCostState.retiredNativeCost[runtime.SessionID] += costs[removedID]
					delete(costs, removedID)
				}
			}
		}
	}
	openCodeVirtualCostState.Unlock()
	return removedIDs
}

func persistOpenCodeRealCost(runtime db.OpenCodeRuntime, cost float64) error {
	if retained, err := db.MaxRealCostForConv(runtime.ConvID); err == nil && retained > cost {
		cost = retained
	}
	return db.UpdateSessionCost(runtime.SessionID, cost)
}

func applyOpenCodeSessionDeletion(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	removedIDs []string,
) {
	if len(removedIDs) == 0 || !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	removed := make(map[string]struct{}, len(removedIDs))
	for _, id := range removedIDs {
		removed[id] = struct{}{}
	}
	var messageIDs []string
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	for messageID, state := range usages {
		sessionID := state.message.SessionID
		if sessionID == "" {
			for _, step := range state.steps {
				sessionID = step.SessionID
				if sessionID != "" {
					break
				}
			}
		}
		if _, deleted := removed[sessionID]; !deleted {
			continue
		}
		delete(usages, messageID)
		delete(openCodeVirtualCostState.bySession[runtime.SessionID], messageID)
		forgetOpenCodeKnownMessageLocked(runtime.ConvID, messageID)
		forgetOpenCodeSnapshotMessageLocked(runtime.ConvID, messageID)
		messageIDs = append(messageIDs, messageID)
	}
	openCodeVirtualCostState.Unlock()
	for _, messageID := range messageIDs {
		_ = db.DeleteOpenCodeUsageActivity(runtime.ConvID, runtime.SessionID, messageID)
		_ = clearOpenCodePricingStepsRemoved(runtime.ConvID, messageID)
		takeOpenCodePendingRemoval(runtime.ConvID, messageID)
	}
	projectAndPersistOpenCodeCostState(ctx, runtime)
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
	if !openCodeSessionTracked(runtime, sessionID) || decoded.Properties.Info.Cost < 0 {
		return
	}
	openCodeVirtualCostState.Lock()
	if openCodeVirtualCostState.nativeCosts == nil {
		openCodeVirtualCostState.nativeCosts = map[string]map[string]float64{}
	}
	costs := openCodeVirtualCostState.nativeCosts[runtime.SessionID]
	if costs == nil {
		costs = map[string]float64{}
		openCodeVirtualCostState.nativeCosts[runtime.SessionID] = costs
	}
	costs[sessionID] = decoded.Properties.Info.Cost
	total := 0.0
	for _, cost := range costs {
		total += cost
	}
	total += openCodeVirtualCostState.retiredNativeCost[runtime.SessionID]
	openCodeVirtualCostState.Unlock()
	if total <= 0 {
		return
	}
	if err := persistOpenCodeRealCost(runtime, total); err != nil {
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
		SessionID  string   `json:"sessionID"`
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

func openCodeRawEventSessionID(event json.RawMessage) string {
	var envelope openCodeEventEnvelope
	if json.Unmarshal(event, &envelope) != nil {
		return ""
	}
	return openCodeEventSessionID(envelope)
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
		SessionID: sessionID, MessageID: part.MessageID, ReportedCost: part.Cost,
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
	openCodeVirtualCostState.Unlock()
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
	for _, step := range state.steps {
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
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// Resume launches start the managed runtime and its SSE consumer before the
	// child `session new` process inserts the local session row. Keep the
	// recovered usage state in memory while waiting for that row, otherwise the
	// first authoritative backfill can become a silent UPDATE/INSERT no-op.
	if !waitForOpenCodeCostSessionRow(ctx, runtime.SessionID) {
		return
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	openCodeVirtualCostState.Lock()
	_, usages := ensureOpenCodeVirtualCostStateLocked(runtime.SessionID)
	states := make([]openCodeMessageCostUsage, 0, len(usages))
	realCost := 0.0
	for _, state := range usages {
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
	if err := db.ReplaceSessionVirtualCostHistory(runtime.SessionID, total, snapshots); err != nil {
		slog.Warn("OpenCode virtual cost could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
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
		runtime.SessionID, runtime.ConvID, activity, time.Now(),
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
const maxOpenCodeTrackedSessions = 4096

type openCodeChildSession struct {
	ID       string  `json:"id"`
	ParentID string  `json:"parentID"`
	Cost     float64 `json:"cost"`
}

func fetchOpenCodeSessionMessages(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	sessionID string,
) ([]openCodeHistoryMessage, bool) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(sessionID) +
		"/message?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return nil, false
	}
	response, err := opencodeapi.Do(openCodeConfigHTTPClient, request.WithContext(ctx), runtime)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	var messages []openCodeHistoryMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(&messages); err != nil {
		return nil, false
	}
	for i := range messages {
		if messages[i].Info.SessionID == "" {
			messages[i].Info.SessionID = sessionID
		}
	}
	return messages, true
}

func fetchOpenCodeSessionChildren(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	sessionID string,
) ([]openCodeChildSession, bool) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(sessionID) +
		"/children?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return nil, false
	}
	response, err := opencodeapi.Do(openCodeConfigHTTPClient, request.WithContext(ctx), runtime)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	// Older OpenCode servers did not expose session.children. Treating 404 as
	// a leaf preserves root-only telemetry while supported versions recurse.
	if response.StatusCode == http.StatusNotFound {
		return nil, true
	}
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	var children []openCodeChildSession
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&children); err != nil {
		return nil, false
	}
	return children, true
}

func fetchOpenCodeSessionTreeHistory(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
) ([]openCodeHistoryMessage, map[string]string, map[string]float64, bool) {
	tracked := map[string]string{runtime.ConvID: ""}
	nativeCosts := map[string]float64{}
	queue := []string{runtime.ConvID}
	allMessages := []openCodeHistoryMessage{}
	for len(queue) > 0 {
		if len(tracked) > maxOpenCodeTrackedSessions {
			return nil, nil, nil, false
		}
		sessionID := queue[0]
		queue = queue[1:]
		messages, ok := fetchOpenCodeSessionMessages(ctx, runtime, sessionID)
		if !ok {
			return nil, nil, nil, false
		}
		allMessages = append(allMessages, messages...)
		children, ok := fetchOpenCodeSessionChildren(ctx, runtime, sessionID)
		if !ok {
			return nil, nil, nil, false
		}
		for _, child := range children {
			if child.ID == "" {
				continue
			}
			if _, seen := tracked[child.ID]; seen {
				continue
			}
			tracked[child.ID] = sessionID
			nativeCosts[child.ID] = child.Cost
			queue = append(queue, child.ID)
		}
	}
	return allMessages, tracked, nativeCosts, true
}

func backfillOpenCodeContextUsage(ctx context.Context, runtime db.OpenCodeRuntime) bool {
	if runtime.ConvID == "" || !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}
	if !retryOpenCodePendingRemovals(ctx, runtime) {
		// Stale history is unsafe until every locally observed final removal is
		// durable. Leave the previous authoritative state intact and retry on
		// this projector's deduplicated batch path or the next reconnect.
		scheduleOpenCodeRemovalRetry(ctx, runtime)
		return false
	}
	messages, trackedSessions, nativeCosts, ok := fetchOpenCodeSessionTreeHistory(ctx, runtime)
	if !ok {
		slog.Debug("OpenCode session-tree backfill failed",
			"session", runtime.SessionID, "module", "agentd")
		return false
	}
	removedPricingSteps, err := db.OpenCodePricingStepsRemoved(runtime.ConvID, time.Now())
	if err != nil {
		// Missing durable removal state is not safe to interpret as "no
		// removals": stale top-level tokens could otherwise resurrect a cleared
		// projection. Keep the prior authoritative state and retry later.
		slog.Debug("OpenCode pricing-step removals could not be loaded",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		return false
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}

	var (
		latest       openCodeContextUsage
		latestAt     int64
		haveAny      bool
		costUsages   []openCodeMessageCostUsage
		realCost     float64
		historyCosts = map[string]float64{}
		snapshot     = map[string]map[string]struct{}{}
	)
	for _, m := range messages {
		if m.Info.Role != "assistant" {
			continue
		}
		usage := openCodeContextUsage{
			SessionID:    m.Info.SessionID,
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
				SessionID: m.Info.SessionID, MessageID: messageID, ReportedCost: part.Cost,
				Input: part.Tokens.Input, Output: part.Tokens.Output, Reasoning: part.Tokens.Reasoning,
				CacheRead: part.Tokens.Cache.Read, CacheWrite: part.Tokens.Cache.Write,
			}
			if step.total() > 0 {
				costUsage.steps[part.ID] = step
				costUsage.hadSteps = true
			}
		}
		snapshotParts := make(map[string]struct{}, len(costUsage.steps))
		for partID := range costUsage.steps {
			snapshotParts[partID] = struct{}{}
		}
		snapshot[usage.MessageID] = snapshotParts
		if len(costUsage.steps) == 0 && removedPricingSteps[usage.MessageID] {
			// OpenCode can retain stale top-level message tokens after the last
			// step-finish part is removed. Keep aggregate mode active with an
			// empty step set so those tokens remain ineligible after reconnect.
			costUsage.hadSteps = true
		} else if len(costUsage.steps) > 0 && removedPricingSteps[usage.MessageID] {
			// History is authoritative that a later eligible step exists.
			if err := clearOpenCodePricingStepsRemoved(runtime.ConvID, usage.MessageID); err != nil {
				slog.Debug("OpenCode recovered pricing-step marker could not be cleared",
					"session", runtime.SessionID, "error", err, "module", "agentd")
			}
			if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
				return false
			}
		}
		if len(costUsage.steps) > 0 {
			openCodeVirtualCostState.Lock()
			if removedPricingSteps[usage.MessageID] {
				// A durable final-removal marker followed by historical steps
				// means a later model call superseded the removed set.
				forgetOpenCodeKnownMessageLocked(runtime.ConvID, usage.MessageID)
			}
			for partID := range costUsage.steps {
				rememberOpenCodeKnownStepLocked(runtime.ConvID, usage.MessageID, partID)
			}
			openCodeVirtualCostState.Unlock()
		}
		messageRealCost := openCodeMessageUsageRealCost(costUsage)
		realCost += messageRealCost
		if messageRealCost > 0 {
			historyCosts[m.Info.SessionID] += messageRealCost
		}
		effectiveUsage := aggregateOpenCodeMessageCostUsage(costUsage)
		if effectiveUsage.total() <= 0 {
			if costUsage.hadSteps {
				costUsages = append(costUsages, costUsage)
			}
			continue
		}
		costUsages = append(costUsages, costUsage)
		if m.Info.SessionID == runtime.ConvID && (!haveAny || m.Info.Time.Created >= latestAt) {
			// Context occupancy is the latest model call, represented by the
			// top-level token block. Only fall back to aggregated parts when
			// that top-level update was interrupted and remains empty.
			latest = usage
			if latest.total() <= 0 {
				latest = effectiveUsage
			}
			latestAt = m.Info.Time.Created
			haveAny = true
		}
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}
	retainedRealCost, _ := db.MaxRealCostForConv(runtime.ConvID)
	openCodeVirtualCostState.Lock()
	for sessionID, cost := range historyCosts {
		nativeCosts[sessionID] = cost
	}
	realCost = 0
	for _, cost := range nativeCosts {
		if cost > 0 {
			realCost += cost
		}
	}
	if openCodeVirtualCostState.trackedSessions == nil {
		openCodeVirtualCostState.trackedSessions = map[string]map[string]string{}
	}
	openCodeVirtualCostState.trackedSessions[runtime.SessionID] = trackedSessions
	if openCodeVirtualCostState.nativeCosts == nil {
		openCodeVirtualCostState.nativeCosts = map[string]map[string]float64{}
	}
	openCodeVirtualCostState.nativeCosts[runtime.SessionID] = nativeCosts
	if openCodeVirtualCostState.retiredNativeCost == nil {
		openCodeVirtualCostState.retiredNativeCost = map[string]float64{}
	}
	retiredRealCost := math.Max(retainedRealCost-realCost, 0)
	openCodeVirtualCostState.retiredNativeCost[runtime.SessionID] = retiredRealCost
	realCost += retiredRealCost
	if openCodeVirtualCostState.snapshotSteps == nil {
		openCodeVirtualCostState.snapshotSteps =
			map[string]map[string]map[string]struct{}{}
	}
	openCodeVirtualCostState.snapshotSteps[runtime.ConvID] = snapshot
	openCodeVirtualCostState.Unlock()
	replaceOpenCodeVirtualCostUsage(ctx, runtime, costUsages)
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}
	if realCost > 0 && waitForOpenCodeCostSessionRow(ctx, runtime.SessionID) {
		if err := persistOpenCodeRealCost(runtime, realCost); err != nil {
			slog.Warn("OpenCode real cost backfill could not be persisted",
				"session", runtime.SessionID, "error", err, "module", "agentd")
		}
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}
	if !haveAny {
		forgetAllOpenCodePendingRemovals(runtime.ConvID)
		return true
	}
	persistOpenCodeContextUsage(ctx, runtime, latest)
	persistOpenCodeModelSlug(runtime, latest)
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}
	forgetAllOpenCodePendingRemovals(runtime.ConvID)
	return true
}
