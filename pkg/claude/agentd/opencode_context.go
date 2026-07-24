package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// OpenCode reports per-turn token usage on the assistant message delivered over
// the `/event` SSE stream (`message.updated`), and the active model's
// context-window limit through `/config/providers`. Neither the SSE lifecycle
// projector (opencode_events.go) nor the model catalog (opencode_models.go)
// retains those values, so an OpenCode agent shows a blank dashboard context
// meter where Claude Code and Codex show usage. This module closes that gap by
// projecting the same four `sessions` columns the other harnesses populate
// (context_pct, tokens_input, tokens_output, context_window_size) via
// db.UpdateContextSnapshot — after which the harness-agnostic dashboard render
// path surfaces OpenCode identically. See TCL-701.

const (
	openCodeLimitCacheTTL       = 5 * time.Minute
	openCodeLimitFailureBackoff = 30 * time.Second
	openCodeConfigTimeout       = 3 * time.Second
)

// openCodeContextUsage is the token accounting carried on one OpenCode
// assistant message. The bucket names mirror OpenCode's own message schema
// (`tokens: {input, output, reasoning, cache: {read, write}}`); ProviderID and
// ModelID key the context-window lookup.
type openCodeContextUsage struct {
	MessageID  string
	ProviderID string
	ModelID    string
	CreatedAt  time.Time
	// ReportedCost is OpenCode's real cost for this assistant message. A
	// present zero identifies the subscription-backed path; nil is ambiguous
	// and must not be treated as a subscription.
	ReportedCost *float64
	Input        int64
	Output       int64
	Reasoning    int64
	CacheRead    int64
	CacheWrite   int64
}

// total is the resident context occupancy OpenCode itself computes for its
// footer usage indicator: every token bucket summed. OpenCode re-sends the full
// conversation each turn, so the latest assistant message's total is the tokens
// currently occupying the window — the right numerator for occupancy, and the
// same last-turn semantics Codex uses.
func (u openCodeContextUsage) total() int64 {
	return u.Input + u.Output + u.Reasoning + u.CacheRead + u.CacheWrite
}

type openCodeMessageTokensPayload struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

type openCodeMessageUpdatedEvent struct {
	Type       string `json:"type"`
	Properties struct {
		Info struct {
			ID         string   `json:"id"`
			Role       string   `json:"role"`
			ProviderID string   `json:"providerID"`
			ModelID    string   `json:"modelID"`
			SessionID  string   `json:"sessionID"`
			Cost       *float64 `json:"cost"`
			Time       struct {
				Created int64 `json:"created"`
			} `json:"time"`
			Tokens openCodeMessageTokensPayload `json:"tokens"`
		} `json:"info"`
		SessionID string `json:"sessionID"`
	} `json:"properties"`
}

// parseOpenCodeContextUsage extracts token usage from a `message.updated` SSE
// event for the assistant of convID. ok is false for any event that is not an
// assistant message-update for this conversation, or whose token total is not
// yet positive — OpenCode streams several message.updated snapshots per turn,
// and the early ones carry an all-zero usage block (mirrors OpenCode's own
// `total <= 0 -> no usage` guard). Suppressing those keeps an empty mid-stream
// snapshot from clobbering a good one back toward 0% in the dashboard meter.
func parseOpenCodeContextUsage(event json.RawMessage, convID string) (openCodeContextUsage, bool) {
	if convID == "" {
		return openCodeContextUsage{}, false
	}
	// The SSE stream is dominated by message.part.updated / status events; skip
	// the full unmarshal (already paid once by the lifecycle projector) unless
	// the raw event could be a message.updated.
	if !bytes.Contains(event, []byte(`"message.updated"`)) {
		return openCodeContextUsage{}, false
	}
	var decoded openCodeMessageUpdatedEvent
	if err := json.Unmarshal(event, &decoded); err != nil {
		return openCodeContextUsage{}, false
	}
	if decoded.Type != "message.updated" {
		return openCodeContextUsage{}, false
	}
	info := decoded.Properties.Info
	// /event is directory-scoped, not session-scoped: one OpenCode store can
	// carry several conversations for the same worktree. The message info
	// carries its own sessionID; fall back to the envelope's for robustness.
	sessionID := info.SessionID
	if sessionID == "" {
		sessionID = decoded.Properties.SessionID
	}
	if sessionID != convID || info.Role != "assistant" {
		return openCodeContextUsage{}, false
	}
	usage := openCodeContextUsage{
		MessageID:    info.ID,
		ProviderID:   info.ProviderID,
		ModelID:      info.ModelID,
		ReportedCost: info.Cost,
		Input:        info.Tokens.Input,
		Output:       info.Tokens.Output,
		Reasoning:    info.Tokens.Reasoning,
		CacheRead:    info.Tokens.Cache.Read,
		CacheWrite:   info.Tokens.Cache.Write,
	}
	if info.Time.Created > 0 {
		usage.CreatedAt = time.UnixMilli(info.Time.Created)
	}
	if usage.total() <= 0 {
		return openCodeContextUsage{}, false
	}
	return usage, true
}

// openCodeContextSnapshot converts token usage plus a resolved context-window
// limit into the four dashboard values. tokensInput folds the cache buckets
// into the input side and tokensOutput folds reasoning into the output side, so
// tokensInput+tokensOutput equals the occupancy total OpenCode displays and the
// dashboard tooltip's used/window figure stays consistent with pct. pct is 0
// when the window is unknown (limit lookup failed), matching Codex's
// window-absent behaviour; the meter then renders as "not reported".
func openCodeContextSnapshot(usage openCodeContextUsage, windowSize int64) (pct float64, tokensInput, tokensOutput int64) {
	tokensInput = usage.Input + usage.CacheRead + usage.CacheWrite
	tokensOutput = usage.Output + usage.Reasoning
	total := tokensInput + tokensOutput
	if windowSize > 0 && total > 0 {
		pct = float64(total) / float64(windowSize) * 100
	}
	return pct, tokensInput, tokensOutput
}

// persistOpenCodeContextUsage resolves the active model's context window and
// writes the occupancy snapshot to the session row. All-zero writes are a no-op
// at the DB chokepoint (db.UpdateContextSnapshot), so a usage that resolves to
// nothing meaningful degrades to leaving the prior snapshot untouched.
func persistOpenCodeContextUsage(ctx context.Context, runtime db.OpenCodeRuntime, usage openCodeContextUsage) {
	windowSize := openCodeContextWindow(ctx, runtime, usage.ProviderID, usage.ModelID)
	pct, tokensInput, tokensOutput := openCodeContextSnapshot(usage, windowSize)
	if err := db.UpdateContextSnapshot(runtime.SessionID, pct, tokensInput, tokensOutput, windowSize); err != nil {
		slog.Debug("OpenCode context snapshot could not be persisted",
			"session", runtime.SessionID, "error", err, "module", "agentd")
	}
}

// openCodeConfigHTTPClient is the shared client for /config/providers reads, so
// limit fetches reuse connections and idle transports rather than allocating a
// client per call.
var openCodeConfigHTTPClient = &http.Client{Timeout: openCodeConfigTimeout}

// openCodeLimitCache memoizes the per-model context-window limits fetched from
// each managed server's /config/providers, keyed by server URL. Each session
// owns a distinct server, and the model catalog is stable for a run, so a short
// TTL keeps the hot SSE path from issuing an HTTP round trip per assistant
// message while still picking up a provider/model change within minutes.
var openCodeLimitCache struct {
	sync.Mutex
	byServer map[string]openCodeLimitEntry
}

type openCodeLimitEntry struct {
	limits map[string]int64
	prices map[string]openCodeModelPrice
	// loaded distinguishes a successful (possibly empty) native catalog from
	// a transient fetch failure. Cost recovery must never erase retained
	// history merely because the local OpenCode server was temporarily
	// unavailable after restart.
	loaded bool
	at     time.Time
	// ttl bounds this entry's freshness. A successful fetch earns the full
	// cache TTL; a failed fetch with no prior good data earns a shorter backoff
	// so a persistently failing /config/providers cannot force an HTTP round
	// trip on every assistant message flowing through the hot SSE path.
	ttl time.Duration
}

// openCodeContextWindow returns the context-window token limit for
// providerID/modelID as reported by the managed server, or 0 when the limit is
// unavailable (unknown model, fetch failure, or missing provider metadata). A
// 0 return degrades gracefully: the snapshot persists token counts with pct 0.
func openCodeContextWindow(ctx context.Context, runtime db.OpenCodeRuntime, providerID, modelID string) int64 {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		return 0
	}
	limits := openCodeModelLimits(ctx, runtime)
	return limits[providerID+"/"+modelID]
}

func openCodeModelLimits(ctx context.Context, runtime db.OpenCodeRuntime) map[string]int64 {
	return openCodeModelCatalog(ctx, runtime).limits
}

func openCodeModelPrices(ctx context.Context, runtime db.OpenCodeRuntime) (map[string]openCodeModelPrice, bool) {
	entry := openCodeModelCatalog(ctx, runtime)
	return entry.prices, entry.loaded
}

func openCodeModelCatalog(ctx context.Context, runtime db.OpenCodeRuntime) openCodeLimitEntry {
	openCodeLimitCache.Lock()
	if openCodeLimitCache.byServer == nil {
		openCodeLimitCache.byServer = map[string]openCodeLimitEntry{}
	}
	entry, ok := openCodeLimitCache.byServer[runtime.ServerURL]
	if ok && time.Since(entry.at) < entry.ttl {
		openCodeLimitCache.Unlock()
		return entry
	}
	openCodeLimitCache.Unlock()

	limits, prices, err := fetchOpenCodeModelCatalog(ctx, runtime)

	openCodeLimitCache.Lock()
	defer openCodeLimitCache.Unlock()
	// Each spawned session mints a fresh random server URL, so evict entries
	// whose servers are long gone to keep this map from growing unbounded over a
	// long-lived daemon's lifetime.
	evictStaleOpenCodeLimitEntries()
	if err != nil {
		slog.Debug("OpenCode provider limits could not be fetched",
			"session", runtime.SessionID, "error", err, "module", "agentd")
		// Keep serving prior good data rather than dropping a working window
		// size on a transient failure; only when there is none do we install a
		// short negative-cache backoff to avoid refetching on every event.
		if prior := openCodeLimitCache.byServer[runtime.ServerURL]; len(prior.limits) > 0 || len(prior.prices) > 0 {
			return prior
		}
		empty := openCodeLimitEntry{at: time.Now(), ttl: openCodeLimitFailureBackoff}
		openCodeLimitCache.byServer[runtime.ServerURL] = empty
		return empty
	}

	entry = openCodeLimitEntry{limits: limits, prices: prices, loaded: true, at: time.Now(), ttl: openCodeLimitCacheTTL}
	openCodeLimitCache.byServer[runtime.ServerURL] = entry
	return entry
}

// evictStaleOpenCodeLimitEntries drops cache entries not refreshed within a
// generous multiple of the success TTL. Callers must hold openCodeLimitCache.
func evictStaleOpenCodeLimitEntries() {
	const staleAfter = 4 * openCodeLimitCacheTTL
	for server, entry := range openCodeLimitCache.byServer {
		if time.Since(entry.at) > staleAfter {
			delete(openCodeLimitCache.byServer, server)
		}
	}
}

type openCodeCachePrice struct {
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type openCodePriceTier struct {
	Input  float64            `json:"input"`
	Output float64            `json:"output"`
	Cache  openCodeCachePrice `json:"cache"`
	Tier   struct {
		Type string  `json:"type"`
		Size float64 `json:"size"`
	} `json:"tier"`
}

type openCodeModelPrice struct {
	Input                float64             `json:"input"`
	Output               float64             `json:"output"`
	Cache                openCodeCachePrice  `json:"cache"`
	Tiers                []openCodePriceTier `json:"tiers"`
	ExperimentalOver200K *struct {
		Input  float64            `json:"input"`
		Output float64            `json:"output"`
		Cache  openCodeCachePrice `json:"cache"`
	} `json:"experimentalOver200K"`
}

// fetchOpenCodeModelCatalog reads /config/providers and retains both the
// context limit and native price table for each model. OpenCode's catalog
// rates are USD per million tokens; the pricing projection below mirrors
// OpenCode's own session cost selection exactly.
func fetchOpenCodeModelCatalog(ctx context.Context, runtime db.OpenCodeRuntime) (map[string]int64, map[string]openCodeModelPrice, error) {
	endpoint := runtime.ServerURL + "/config/providers?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := openCodeConfigHTTPClient.Do(request.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("OpenCode /config/providers returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Providers []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				Cost  *openCodeModelPrice `json:"cost"`
				Limit struct {
					Context int64 `json:"context"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, nil, err
	}
	limits := make(map[string]int64)
	prices := make(map[string]openCodeModelPrice)
	for _, provider := range payload.Providers {
		if provider.ID == "" {
			continue
		}
		for modelID, model := range provider.Models {
			key := provider.ID + "/" + modelID
			if model.Limit.Context > 0 {
				limits[key] = model.Limit.Context
			}
			if model.Cost != nil {
				prices[key] = *model.Cost
			}
		}
	}
	return limits, prices, nil
}
