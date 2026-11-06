package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

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

// persistOpenCodeRuntimeMetadata records the provider-qualified model identity
// ("openai/gpt-5.6-terra") and reasoning variant from the assistant message
// the context snapshot came from. OpenCode calls reasoning effort `variant`.
// Either value is independently optional so a partial event cannot erase good
// metadata already recorded on the session.
func persistOpenCodeRuntimeMetadata(runtime db.OpenCodeRuntime, usage openCodeContextUsage) {
	sessionID := openCodeTelemetrySessionID(runtime)
	if usage.ProviderID != "" && usage.ModelID != "" {
		if err := db.UpdateSessionModelSlug(sessionID, usage.ProviderID+"/"+usage.ModelID); err != nil {
			slog.Debug("OpenCode model slug could not be persisted",
				"session", sessionID, "error", err, "module", "agentd")
		}
	}
	if err := db.UpdateSessionEffort(sessionID, usage.Variant); err != nil {
		slog.Debug("OpenCode reasoning variant could not be persisted",
			"session", sessionID, "error", err, "module", "agentd")
	}
}

// openCodeTelemetrySessionID resolves the sessions row owned by a managed
// OpenCode runtime. Fresh daemon spawns supervise the server under a temporary
// label, while launch enrollment gives `session new` the already-minted
// OpenCode conversation ID as its row ID. Older/legacy launches used the label
// for both, so prefer that row when it exists and otherwise target the conv ID.
func openCodeTelemetrySessionID(runtime db.OpenCodeRuntime) string {
	if runtime.SessionID != "" {
		if exists, err := db.SessionExists(runtime.SessionID); err == nil && exists {
			return runtime.SessionID
		}
	}
	if runtime.ConvID != "" && runtime.ConvID != runtime.SessionID {
		if exists, err := db.SessionExists(runtime.ConvID); err == nil && exists {
			return runtime.ConvID
		}
	}
	return runtime.SessionID
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
	return db.UpdateSessionCost(openCodeTelemetrySessionID(runtime), cost)
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
		Variant    string   `json:"variant"`
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
