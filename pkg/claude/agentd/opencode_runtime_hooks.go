package agentd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func consumeOpenCodeEvent(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	event json.RawMessage,
) bool {
	return withOpenCodeProjectorApplyLock(ctx, runtime, func() {
		consumeOpenCodeEventLocked(ctx, runtime, projector, event)
	})
}

func consumeOpenCodeEventLocked(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	event json.RawMessage,
) {
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	removedSessions := observeOpenCodeSessionTree(runtime, event)
	applyOpenCodeSessionDeletion(ctx, runtime, removedSessions)
	eventSessionID := openCodeRawEventSessionID(event)
	costSessionTracked := openCodeSessionTracked(runtime, eventSessionID)
	observeOpenCodeStandingOrderAssistant(runtime, projector, event)
	projected, err := projector.project(event)
	if native, ok := projector.takeNativeHook(); ok {
		applyOpenCodeHooks(ctx, runtime, projector,
			[]session.HookCallbackInput{native})
	}
	if err != nil {
		slog.Debug("OpenCode SSE event could not be projected",
			"session", runtime.SessionID, "error", err)
	} else {
		applyOpenCodeHooks(ctx, runtime, projector, projected)
	}
	// A tool-using assistant message can contain several model calls. OpenCode
	// publishes each call's authoritative token block as a step-finish part;
	// retain it before the following message.updated event supplies model
	// metadata and triggers the aggregate WHAT-IF projection.
	if costSessionTracked {
		if step, ok := parseOpenCodeStepCostUsage(event, eventSessionID); ok {
			applyOpenCodeVirtualCostStep(ctx, runtime, step)
		}
	}
	if costSessionTracked {
		if removal, ok := parseOpenCodeCostRemoval(event, eventSessionID); ok {
			applyOpenCodeVirtualCostRemoval(ctx, runtime, removal)
		}
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// Context-window usage rides on the same directory-wide SSE stream as the
	// lifecycle hooks but is a session-row side effect, not a hook event, so it
	// is projected independently of the lifecycle projector, and after it so the
	// current event's status transition is applied before any model-limit fetch.
	// A cold-cache fetch (bounded to one per cache TTL and cancelled with ctx)
	// can briefly delay subsequent buffered events; in practice the local
	// managed server resolves the limit in milliseconds. See TCL-701.
	if costSessionTracked {
		if usage, ok := parseOpenCodeContextUsage(event, eventSessionID); ok {
			if eventSessionID == runtime.ConvID {
				persistOpenCodeContextUsage(ctx, runtime, usage)
				// TCL-673: record the provider/model slug from the root message so
				// child model calls do not replace the parent's dashboard model.
				persistOpenCodeRuntimeMetadata(runtime, usage)
			}
			// TCL-708: the same authoritative per-message usage drives the native
			// catalog what-if projection and provider-aware Usage coverage index.
			applyOpenCodeVirtualCostUsage(ctx, runtime, usage)
		}
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// TCL-673: OpenCode's own cumulative session cost rides session.updated.
	// $0/N-A on a subscription; real spend on a pay-per-token key.
	applyOpenCodeCost(runtime, event)
}

func observeOpenCodeStandingOrderAssistant(
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	event json.RawMessage,
) {
	if projector == nil || !bytes.Contains(event, []byte(`"message.updated"`)) {
		return
	}
	var envelope openCodeEventEnvelope
	if err := json.Unmarshal(event, &envelope); err != nil ||
		envelope.Type != "message.updated" ||
		envelope.Properties.Info.SessionID != runtime.ConvID ||
		envelope.Properties.Info.Role != "assistant" ||
		envelope.Properties.Info.ParentID == "" {
		return
	}
	agentID, err := db.AgentIDForConv(runtime.ConvID)
	if err != nil || agentID == "" {
		if err != nil {
			slog.Warn("OpenCode standing-order origin: resolve assistant owner failed",
				"conv", runtime.ConvID, "error", err)
		}
		return
	}
	activated, err := db.ActivateStandingOrderTurnOrigin(
		agentID, runtime.ConvID, envelope.Properties.Info.ParentID,
		time.Now(), standingOrderOriginActiveTTL)
	if err != nil {
		slog.Warn("OpenCode standing-order origin: correlate assistant turn failed",
			"agent", agentID, "conv", runtime.ConvID, "error", err)
		return
	}
	if activated {
		projector.standingOrderTurn = true
	}
}

type openCodePromptTurnState int

const (
	openCodePromptTurnMissing openCodePromptTurnState = iota
	openCodePromptTurnPending
	openCodePromptTurnActive
	openCodePromptTurnCompleted
)

func openCodePromptTurnHistory(
	runtime db.OpenCodeRuntime,
	messageID string,
) (openCodePromptTurnState, error) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/message?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return openCodePromptTurnMissing, err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return openCodePromptTurnMissing,
			fmt.Errorf("reconcile OpenCode prompt history: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return openCodePromptTurnMissing, fmt.Errorf(
			"reconcile OpenCode prompt history: HTTP %d", response.StatusCode)
	}
	var messages []struct {
		Info struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			ParentID string `json:"parentID"`
			Time     struct {
				Completed *int64 `json:"completed"`
			} `json:"time"`
		} `json:"info"`
	}
	if err := json.NewDecoder(io.LimitReader(
		response.Body, 64<<20)).Decode(&messages); err != nil {
		return openCodePromptTurnMissing,
			fmt.Errorf("decode OpenCode prompt history: %w", err)
	}
	state := openCodePromptTurnMissing
	for _, message := range messages {
		switch {
		case message.Info.Role == "user" && message.Info.ID == messageID:
			if state == openCodePromptTurnMissing {
				state = openCodePromptTurnPending
			}
		case message.Info.Role == "assistant" &&
			message.Info.ParentID == messageID:
			if message.Info.Time.Completed != nil {
				return openCodePromptTurnCompleted, nil
			}
			state = openCodePromptTurnActive
		}
	}
	return state, nil
}

func reconcileOpenCodeStandingOrderOrigin(
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	agentID string,
	origin *db.StandingOrderTurnOrigin,
) (bool, bool) {
	if projector == nil || origin == nil ||
		origin.State != db.StandingOrderTurnOriginPending ||
		runtime.ServerURL == "" {
		return false, true
	}
	state, err := openCodePromptTurnHistory(runtime, origin.OpenCodeMessageID)
	if err != nil {
		slog.Warn("OpenCode standing-order origin: history reconciliation failed",
			"agent", agentID, "conv", runtime.ConvID, "error", err)
		return false, true
	}
	switch state {
	case openCodePromptTurnActive:
		activated, activateErr := db.ActivateStandingOrderTurnOrigin(
			agentID, runtime.ConvID, origin.OpenCodeMessageID,
			time.Now(), standingOrderOriginActiveTTL)
		if activateErr != nil {
			slog.Warn("OpenCode standing-order origin: history activation failed",
				"agent", agentID, "conv", runtime.ConvID, "error", activateErr)
			return false, true
		}
		projector.standingOrderTurn = activated
		return activated, !activated
	case openCodePromptTurnCompleted:
		if cancelErr := db.CancelPendingStandingOrderTurnOrigin(
			agentID, runtime.ConvID, origin.MessageID,
			origin.OpenCodeMessageID); cancelErr != nil {
			slog.Warn("OpenCode standing-order origin: completed history cleanup failed",
				"agent", agentID, "conv", runtime.ConvID, "error", cancelErr)
			return false, true
		}
		return false, false
	default:
		return false, true
	}
}

func applyOpenCodeHooks(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	projected []session.HookCallbackInput,
) {
	agentID, agentErr := db.AgentIDForConv(runtime.ConvID)
	originUncertain := agentErr != nil
	if agentErr != nil {
		slog.Warn("OpenCode standing-order origin: resolve stable agent failed",
			"conv", runtime.ConvID, "error", agentErr)
	}
	if projector != nil && agentID != "" {
		origin, err := db.GetStandingOrderTurnOrigin(
			agentID, runtime.ConvID, time.Now())
		if err != nil {
			slog.Warn("OpenCode standing-order origin: restore turn failed",
				"agent", agentID, "error", err)
			originUncertain = true
		} else {
			projector.standingOrderTurn =
				origin != nil && origin.State == db.StandingOrderTurnOriginActive
			// Pending means an accepted prompt may be queued behind another
			// turn. Suppress all boundaries fail-closed until its exact
			// assistant parent arrives; unrelated Stop events cannot clear it.
			originUncertain =
				origin != nil && origin.State == db.StandingOrderTurnOriginPending
			if len(projected) > 0 && origin != nil &&
				origin.State == db.StandingOrderTurnOriginPending {
				_, uncertain := reconcileOpenCodeStandingOrderOrigin(
					runtime, projector, agentID, origin)
				originUncertain = uncertain
			}
		}
	}
	for _, input := range projected {
		if projector != nil {
			input.StandingOrderOrigin =
				projector.standingOrderTurn || originUncertain
		} else if originUncertain {
			input.StandingOrderOrigin = true
		}
		if input.StandingOrderOnly {
			// session.compacted arrives here (projected as
			// SessionStart(source=compact) without a status change), so the
			// startup-context re-injection must be queued on this branch too.
			session.QueueContextReinjection(input, runtime.SessionID)
			deliverOpenCodeStandingOrders(input, runtime.SessionID)
			continue
		}
		deadline := time.Now().Add(openCodeHookRowWait)
		applied := false
		for {
			if ctx.Err() != nil {
				return
			}
			err := session.ApplyHook(input, runtime.SessionID)
			if err == nil {
				applied = true
				session.QueueContextReinjection(input, runtime.SessionID)
				deliverOpenCodeStandingOrders(input, runtime.SessionID)
				break
			}
			if !errors.Is(err, sql.ErrNoRows) || time.Now().After(deadline) {
				slog.Debug("OpenCode status event could not be applied",
					"session", runtime.SessionID, "event", input.HookEventName, "error", err)
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(openCodeHookRowRetryDelay):
			}
		}
		if applied && projector != nil && projector.standingOrderTurn &&
			openCodeTurnEnd(input.HookEventName) {
			if err := db.CompleteStandingOrderTurnOrigin(
				agentID, runtime.ConvID); err != nil {
				slog.Warn("OpenCode standing-order origin: clear turn failed",
					"agent", agentID, "event", input.HookEventName, "error", err)
			} else {
				projector.standingOrderTurn = false
			}
		}
	}
}

func openCodeTurnEnd(event string) bool {
	return event == "Stop" || event == "StopFailure"
}
