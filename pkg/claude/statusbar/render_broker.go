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

// renderRetryBackoff is how long a failed round trip suppresses the next
// attempt. Same order as the read TTL — long enough that a permanently
// refused agent costs a socket every few seconds rather than every frame,
// short enough that a daemon restart is picked up while the operator is
// still watching.
const renderRetryBackoff = 5 * time.Second

// renderBrokerTimeout bounds one brokered render. Far shorter than the
// hook broker's: a hook can afford to wait because the agent's turn is
// already paused for it, whereas a status line that blocks is a visibly
// frozen pane.
const renderBrokerTimeout = 3 * time.Second

// brokerRenders reports whether this process must hand its statusline
// writes to agentd instead of performing them itself.
//
// It asks the SAME predicate the hook broker asks — marker first, the
// database-absent-but-daemon-reachable probe behind it — rather than
// testing the marker alone. Testing only the marker would leave a layer
// launch that arrived without it brokering its hooks while writing its
// status line into a read-only mount, which is precisely the half-inside
// state this must not have.
func brokerRenders() bool {
	return session.BrokerHostWrites()
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

	// Applied reports that every write this render asked for actually
	// landed. It exists because the change gate removes the direct path's
	// automatic retry: a pane that believes its writes landed sends
	// nothing more until its payload changes, so a database failure the
	// daemon merely logged would cost a snapshot until the next token
	// tick — long enough for the pre-compact guard to judge from it.
	// False leaves the digest unadvanced, so the next render re-sends.
	//
	// A reads-only render sets it too: it asked for no writes, so all of
	// them landed.
	Applied bool `json:"applied,omitempty"`

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

	// A recent failure suppresses the ATTEMPT, never the obligation. The
	// digest is left unadvanced by a failure, so the change gate still
	// reads as "changed" and would otherwise retry on the very next
	// render — a socket connect and a refusal several times a second for
	// an agent the daemon cannot place. Once the backoff lapses the write
	// goes out, because the digest still does not match.
	if cache != nil && !cache.FailedAt.IsZero() && time.Since(cache.FailedAt) < renderRetryBackoff {
		return factsFromBroker(req, cache.Reads)
	}

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
		// pane; a status bar missing its cosmetic extras is not.
		//
		// The digest is deliberately NOT advanced, so the next attempt
		// retries the write rather than believing it landed. What IS
		// recorded is the attempt time, which backs the retry off to the
		// read TTL. Without that a permanently refused agent — an
		// ancestry the daemon can no longer place — would open a socket,
		// be refused, and log, several times a second for the rest of its
		// life: a per-render round trip in exactly the case where there
		// is nothing to deliver.
		logRenderBrokerFailure(req.EnvSessionID, err)
		backoff := renderCache{ReadsAt: time.Now(), FailedAt: time.Now()}
		if cache != nil {
			backoff.Digest = cache.Digest
			backoff.Reads = cache.Reads
		}
		saveRenderCache(req.EnvSessionID, backoff)
		if cache != nil {
			return factsFromBroker(req, cache.Reads)
		}
		return renderFacts{}
	}

	newCache := renderCache{Digest: digest, ReadsAt: time.Now(), Reads: resp}
	if !writesChanged && cache != nil {
		// A reads-only refresh must not claim the writes were re-sent.
		newCache.Digest = cache.Digest
	} else if !resp.Applied {
		// The daemon answered, but a write it attempted did not land.
		// Leaving the digest unadvanced is what restores the retry the
		// change gate would otherwise have removed.
		if cache != nil {
			newCache.Digest = cache.Digest
		} else {
			newCache.Digest = ""
		}
		slog.Warn("statusline broker: agentd could not record this render; will retry",
			"module", "hooks")
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
		// FetchedAt and PRFetchedAt tick on a cache refresh that found
		// everything unchanged, so both are excluded to keep the gate quiet —
		// as is PRVia, which is the snapshot's own bookkeeping.
		//
		// They do reach one recorded value between them: the workspace row's
		// UpdatedAt freshness clock. Leaving them out means a re-lookup that
		// confirmed the same PR does not, by itself, re-send the row, so that
		// clock can lag its true observation. It is the safe direction —
		// agentd weighs it against its own observation of the same pull
		// request and prefers the newer, so understating only ever defers to
		// the daemon, which has the credential anyway. Including them would
		// put a write on an idle pane's timer, which is precisely what the
		// change gate exists to avoid.
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

// renderRefusalLogInterval throttles the one loud failure log.
//
// A refusal never self-corrects, so it has to reach an operator — but the
// thing being refused runs several times a second forever, and an
// unthrottled ERROR would pour identical lines into the log the
// dashboard's Logs tab reads until the agent dies. Once a minute names
// the condition without becoming the condition.
const renderRefusalLogInterval = time.Minute

// logRenderBrokerFailure keeps the three failure modes distinguishable.
// Only the one that never self-corrects is loud, and even that one is
// rate-limited — the daemon's own excess log is throttled for exactly the
// same reason, and a client-side log has no better claim.
func logRenderBrokerFailure(sessionID string, err error) {
	switch {
	case errors.Is(err, errRenderBrokerRefused):
		if !shouldLogRefusal(sessionID) {
			return
		}
		slog.Error("statusline broker: agentd refused this session's renders; "+
			"the dashboard's context, model, cost and location for this agent will not update",
			"error", err, "module", "hooks")
	case errors.Is(err, errRenderBrokerThrottled):
		slog.Debug("statusline broker: throttled by agentd", "error", err, "module", "hooks")
	default:
		slog.Debug("statusline broker: agentd unreachable", "error", err, "module", "hooks")
	}
}

// shouldLogRefusal rate-limits the refusal log across render processes.
//
// Each render is a fresh process, so the throttle cannot live in memory;
// it rides the same pane-local file the render cache uses, as a stamp
// nothing else reads. A pane that cannot write its own /tmp logs every
// time, which is the safe direction: the message still gets out.
func shouldLogRefusal(sessionID string) bool {
	path := renderRefusalStampPath(sessionID)
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < renderRefusalLogInterval {
		return false
	}
	now := time.Now()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		return true
	}
	_ = os.Chtimes(path, now, now)
	return true
}

// daemonSocketClient builds a client bounded by timeout that dials agentd's
// unix socket, trying each candidate path in turn.
//
// It attaches no identity headers, and deliberately so: agentd resolves this
// pane from the socket's peer credentials and its harness ancestry, which is
// the only identity a status line could honestly assert. Every statusline
// caller shares that discipline, so the client is shared too.
func daemonSocketClient(timeout time.Duration) (*http.Client, error) {
	socks := agentipc.ClientSocketPaths()
	if len(socks) == 0 {
		return nil, fmt.Errorf("no agentd socket path resolved")
	}
	return &http.Client{
		Timeout: timeout,
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
	}, nil
}

func postRenderToDaemon(req BrokeredRenderRequest) (BrokeredRenderResponse, error) {
	var out BrokeredRenderResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	client, err := daemonSocketClient(renderBrokerTimeout)
	if err != nil {
		return out, err
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
