package statusbar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- the statusline half of TCL-754's broker ---
//
// A statusline render is a very different customer from a hook event.
// Hooks arrive at conversational cadence; Claude Code re-renders the
// status line several times a second, forever, for every agent. Handing
// each render to the daemon unconditionally would put a socket round trip
// on a display refresh — so this path is explicitly NOT "the hook broker
// with a different URL".
//
// Two disciplines keep it cheap, both of them operator rulings:
//
//   - WRITES ARE CHANGE-GATED, not time-gated. A render whose payload is
//     byte-identical to the last one sent records nothing, because it
//     would record exactly what is already there. A render whose payload
//     differs goes out IMMEDIATELY, with no minimum interval in front of
//     it. That distinction is load-bearing: the pre-compact guard reads
//     the context snapshot this path writes, so a rate discipline on the
//     write side would hand the guard stale numbers and let a compaction
//     through on out-of-date evidence.
//   - READS ARE TTL-CACHED. The pin, the sandbox badge and the usage
//     buckets are cosmetic, and a status bar is a convenience surface: a
//     few seconds of staleness there is acceptable by ruling. Nothing
//     correctness-bearing reads from this cache.
//
// The one read that is NOT cosmetic — which session row this render may
// write — is never cached and never sent. The daemon resolves it from the
// caller's process ancestry and re-runs the attribution gate itself, so
// write authority comes from identity the caller cannot assert.

// renderReadTTL bounds how long the cosmetic reads may coast. Short
// enough that toggling the sandbox override or pinning a window shows up
// while the operator is still looking at the pane, long enough that an
// idle agent's bar costs no traffic at all.
const renderReadTTL = 5 * time.Second

// renderBrokerTimeout bounds one brokered render. Far shorter than the
// hook broker's: a hook can afford to wait because the agent's turn is
// already paused for it, whereas a status line that blocks is a visibly
// frozen pane.
const renderBrokerTimeout = 3 * time.Second

// brokerRenders reports whether this process must hand its statusline
// writes to agentd instead of performing them itself.
//
// It rides the same launch-seam marker the hook broker uses, because it
// answers the same question — "is the conversation database reachable
// from in here" — and a launch cannot be half inside the wall.
func brokerRenders() bool {
	return os.Getenv(session.HookBrokerEnvVar) == session.HookBrokerAgentd
}

// BrokeredRenderRequest is the body a sandboxed statusline render POSTs
// to agentd.
//
// It carries the payload verbatim rather than a pre-computed set of
// writes. That is deliberate: the daemon re-derives every recorded number
// with the same code the pane would have run, so there is one derivation
// rather than two that can disagree — and so a hostile pane cannot state
// a context percentage its payload does not support.
type BrokeredRenderRequest struct {
	// ClaimedSessionID is the caller's TCLAUDE_SESSION_ID, a CROSS-CHECK
	// ONLY. The daemon resolves the real row from process ancestry and
	// refuses the request when the two disagree.
	ClaimedSessionID string `json:"claimed_session_id,omitempty"`

	// RenderConvID is Claude Code's session_id from the payload: the
	// conversation this render describes. It is the input to the
	// attribution gate, which the DAEMON runs.
	RenderConvID string `json:"render_conv_id,omitempty"`

	// EnvPinnedWindow is the raw auto-compaction pin observed in the
	// pane's environment.
	EnvPinnedWindow string `json:"env_pinned_window,omitempty"`

	// Payload is the verbatim statusline stdin.
	Payload []byte `json:"payload,omitempty"`

	// Git is the pane's git/gh snapshot. It cannot be re-derived
	// daemon-side — the daemon is not in the agent's worktree — so this
	// is the one part of the workspace row the caller supplies.
	Git *GitSnapshot `json:"git,omitempty"`

	// ApplyWrites is false on a render that only needs its reads
	// refreshed, so a coasting bar costs no writes.
	ApplyWrites bool `json:"apply_writes,omitempty"`

	// WantUsage asks for the usage-API cache fallback, which the render
	// needs only when its own payload carried no rate-limit buckets.
	WantUsage bool `json:"want_usage,omitempty"`
}

// BrokeredRenderResponse carries back the facts the bar renders.
type BrokeredRenderResponse struct {
	// Owned reports whether the daemon's attribution gate accepted this
	// render as the resolved session's own.
	Owned bool `json:"owned,omitempty"`

	// PinnedWindow is the resolved auto-compaction pin in tokens.
	PinnedWindow int64 `json:"pinned_window,omitempty"`

	// SandboxOff drives the ⚠ SB-OFF badge.
	SandboxOff bool `json:"sandbox_off,omitempty"`

	// Usage is the usage-API cache, present only when WantUsage was set.
	Usage *usageapi.CachedUsage `json:"usage,omitempty"`

	// UsageStale marks a cache the daemon's refresh could not replace.
	UsageStale bool `json:"usage_stale,omitempty"`

	// UsagePresent distinguishes "asked and there is none" from "never
	// asked", so a cached response is not mistaken for an unanswered one.
	UsagePresent bool `json:"usage_present,omitempty"`
}

// brokeredHostState is the sandboxed path's hostState.
func brokeredHostState(req renderRequest) renderFacts {
	cache := loadRenderCache(req.EnvSessionID)
	digest := renderDigest(req)

	writesChanged := cache == nil || cache.Digest != digest
	readsStale := cache == nil || time.Since(cache.ReadsAt) > renderReadTTL ||
		(req.WantUsage && !cache.Reads.UsagePresent)

	if !writesChanged && !readsStale {
		return factsFromBroker(req, cache.Reads)
	}

	resp, err := postRenderToDaemon(BrokeredRenderRequest{
		ClaimedSessionID: req.EnvSessionID,
		RenderConvID:     req.RenderConvID,
		EnvPinnedWindow:  req.EnvPinnedWindow,
		Payload:          req.Payload,
		Git:              req.Git,
		ApplyWrites:      writesChanged,
		WantUsage:        req.WantUsage,
	})
	if err != nil {
		// Fail soft, and keep coasting on whatever was last known. A
		// status bar that errors out is a visible defect in the agent's
		// pane; a status bar missing its cosmetic extras is not. The
		// digest is deliberately NOT advanced here, so the next render
		// retries the write rather than believing it landed.
		logRenderBrokerFailure(err)
		if cache != nil {
			return factsFromBroker(req, cache.Reads)
		}
		return renderFacts{}
	}

	newCache := renderCache{Digest: digest, ReadsAt: time.Now(), Reads: resp}
	if !writesChanged && cache != nil {
		// A reads-only refresh must not claim the writes were re-sent.
		newCache.Digest = cache.Digest
	}
	saveRenderCache(req.EnvSessionID, newCache)
	return factsFromBroker(req, resp)
}

// factsFromBroker maps a daemon answer onto the same renderFacts the
// direct path produces, so the rendering code below cannot tell which
// path it came from.
func factsFromBroker(req renderRequest, resp BrokeredRenderResponse) renderFacts {
	facts := renderFacts{
		PinnedWindow: resp.PinnedWindow,
		SandboxOff:   resp.SandboxOff,
		Usage:        resp.Usage,
		UsageStale:   resp.UsageStale,
	}
	if resp.Owned {
		// The identifiers are only ever used to decide whether a write
		// happens, and under brokering no write happens in this process.
		// Carrying the daemon's verdict rather than its row id keeps that
		// property obvious.
		facts.Owned = req.EnvSessionID
		facts.WorkspaceConv = req.RenderConvID
	}
	return facts
}

// renderDigest is the change gate. Any byte of the payload, the git
// snapshot or the pin differing means the writes this render implies
// differ, so it goes out. Digesting the raw payload rather than a
// hand-listed set of fields means a field Claude Code adds later cannot
// silently fall outside the gate and stop being recorded.
func renderDigest(req renderRequest) string {
	h := sha256.New()
	_, _ = h.Write([]byte(req.RenderConvID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.EnvPinnedWindow))
	_, _ = h.Write([]byte{0})
	if req.Git != nil {
		// FetchedAt ticks on every git-cache refresh without changing
		// anything recorded, so it is excluded to keep the gate quiet.
		gitKey := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s",
			req.Git.RepoURL, req.Git.Branch, req.Git.DefaultBranch,
			req.Git.PRNumber, req.Git.PRURL, req.Git.PRState)
		_, _ = h.Write([]byte(gitKey))
	}
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(req.Payload)
	return hex.EncodeToString(h.Sum(nil))
}

// errRenderBrokerRefused marks a daemon REFUSAL — the caller could not be
// placed, or claimed a session it does not own — as opposed to a
// transport failure or a rate-limit rejection. A refusal does not
// self-correct: the agent silently loses its whole status surface for the
// rest of its life, so it is logged where an operator will find it.
var errRenderBrokerRefused = errors.New("agentd refused the brokered statusline render")

// errRenderBrokerThrottled marks a rate-limit rejection, which is
// self-correcting and must not be logged at every render.
var errRenderBrokerThrottled = errors.New("agentd throttled the brokered statusline render")

// logRenderBrokerFailure keeps the three failure modes distinguishable.
// The statusline runs several times a second, so only the one that never
// self-corrects is allowed to be loud.
func logRenderBrokerFailure(err error) {
	switch {
	case errors.Is(err, errRenderBrokerRefused):
		slog.Error("statusline broker: agentd refused this session's renders; "+
			"the dashboard's context, model, cost and location for this agent will not update",
			"error", err, "module", "hooks")
	case errors.Is(err, errRenderBrokerThrottled):
		slog.Debug("statusline broker: throttled by agentd", "error", err, "module", "hooks")
	default:
		slog.Debug("statusline broker: agentd unreachable", "error", err, "module", "hooks")
	}
}

func postRenderToDaemon(req BrokeredRenderRequest) (BrokeredRenderResponse, error) {
	var out BrokeredRenderResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	socks := agentipc.ClientSocketPaths()
	if len(socks) == 0 {
		return out, fmt.Errorf("no agentd socket path resolved")
	}
	client := &http.Client{
		Timeout: renderBrokerTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var lastErr error
				for _, sock := range socks {
					conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		},
	}
	httpReq, err := http.NewRequest(http.MethodPost, "http://tclaude/v1/whoami/statusline", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		statusErr := fmt.Errorf("agentd returned %d: %s", resp.StatusCode, bytes.TrimSpace(preview))
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusUnauthorized:
			return out, fmt.Errorf("%w: %w", errRenderBrokerRefused, statusErr)
		case http.StatusTooManyRequests:
			return out, fmt.Errorf("%w: %w", errRenderBrokerThrottled, statusErr)
		}
		return out, statusErr
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return out, fmt.Errorf("decode broker response: %w", err)
	}
	return out, nil
}
