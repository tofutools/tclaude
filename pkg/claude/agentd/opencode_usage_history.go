package agentd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

// OpenCode session history: the message and child-session fetches and the
// reconnect backfill (TCL-673; see opencode_usage.go).

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
			if len(tracked) >= maxOpenCodeTrackedSessions {
				return nil, nil, nil, false
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
	inheritedUntil, err := openCodeInheritedUntil(runtime)
	if err != nil {
		slog.Debug("OpenCode inherited-history boundary could not be loaded",
			"session", runtime.SessionID, "error", err, "module", "agentd")
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
			Variant:      m.Info.Variant,
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
		// An imported message still describes the context, but its cost is
		// the spend of the conversation it was copied from.
		inherited := openCodeMessageInherited(runtime, usage, inheritedUntil)
		if !inherited {
			messageRealCost := openCodeMessageUsageRealCost(costUsage)
			realCost += messageRealCost
			if messageRealCost > 0 {
				historyCosts[m.Info.SessionID] += messageRealCost
			}
		}
		effectiveUsage := aggregateOpenCodeMessageCostUsage(costUsage)
		if effectiveUsage.total() <= 0 {
			if costUsage.hadSteps && !inherited {
				costUsages = append(costUsages, costUsage)
			}
			continue
		}
		if !inherited {
			costUsages = append(costUsages, costUsage)
		}
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
	if realCost > 0 {
		if sessionID, found := waitForOpenCodeCostSessionRow(ctx, runtime); found {
			if err := persistOpenCodeRealCost(runtime, realCost); err != nil {
				slog.Warn("OpenCode real cost backfill could not be persisted",
					"session", sessionID, "error", err, "module", "agentd")
			}
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
	persistOpenCodeRuntimeMetadata(runtime, latest)
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return false
	}
	forgetAllOpenCodePendingRemovals(runtime.ConvID)
	return true
}
