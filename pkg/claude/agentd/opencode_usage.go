package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"

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
		Role       string `json:"role"`
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
		Time       struct {
			Created int64 `json:"created"`
		} `json:"time"`
		Tokens openCodeMessageTokensPayload `json:"tokens"`
	} `json:"info"`
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
	// Use the general 5 s API client, not the 3 s /config/providers-tuned one:
	// a message history can be far larger than a provider catalog, and the
	// timeout covers the whole body read. A backfill that times out degrades
	// gracefully — the next live turn re-populates the snapshot.
	response, err := openCodeHTTPClient.Do(request.WithContext(ctx))
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
	)
	for _, m := range messages {
		if m.Info.Role != "assistant" {
			continue
		}
		usage := openCodeContextUsage{
			ProviderID: m.Info.ProviderID,
			ModelID:    m.Info.ModelID,
			Input:      m.Info.Tokens.Input,
			Output:     m.Info.Tokens.Output,
			Reasoning:  m.Info.Tokens.Reasoning,
			CacheRead:  m.Info.Tokens.Cache.Read,
			CacheWrite: m.Info.Tokens.Cache.Write,
		}
		if usage.total() <= 0 {
			continue
		}
		if !haveAny || m.Info.Time.Created >= latestAt {
			latest = usage
			latestAt = m.Info.Time.Created
			haveAny = true
		}
	}
	if !haveAny {
		return
	}
	persistOpenCodeContextUsage(ctx, runtime, latest)
	persistOpenCodeModelSlug(runtime, latest)
}
