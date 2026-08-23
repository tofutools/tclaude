package statusbar

import (
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// renderRequest is everything one render knows before it touches the
// host: the payload, the pane's own environment, and the git snapshot it
// gathered by shelling out (git and gh both work inside the sandbox, so
// that half never needs brokering).
type renderRequest struct {
	// EnvSessionID is TCLAUDE_SESSION_ID. On the direct path it is the
	// write target, subject to the attribution gate. On the brokered path
	// it is a CROSS-CHECK ONLY — the daemon resolves the real row from
	// the caller's process ancestry.
	EnvSessionID string

	// RenderConvID is Claude Code's own session_id from the payload: the
	// conversation this render actually describes.
	RenderConvID string

	// EnvPinnedWindow is the raw auto-compaction pin from this pane's
	// environment.
	EnvPinnedWindow string

	// Payload is the verbatim stdin bytes, stored as the statusline
	// snapshot so buckets StatusLineInput does not name yet survive.
	Payload []byte

	// Input is Payload, parsed.
	Input StatusLineInput

	// Git is this pane's git/gh snapshot, or nil outside a repo.
	Git *GitSnapshot

	// WantUsage records whether the render will need the usage-API cache
	// fallback. False whenever the payload carried its own buckets.
	WantUsage bool
}

// renderFacts is everything one render needs FROM the host. Nothing else
// about the database reaches the rendered bar.
type renderFacts struct {
	// Owned is the session row this render may write; empty when the
	// attribution gate refused it.
	Owned string

	// WorkspaceConv is the conversation id the workspace snapshot is
	// keyed by; empty when there is nothing safe to key it on.
	WorkspaceConv string

	// PinnedWindow is the resolved auto-compaction pin in tokens, with
	// the pane's own environment already taking precedence over the row.
	// Zero when neither pinned one.
	PinnedWindow int64

	// SandboxOff drives the ⚠ SB-OFF badge.
	SandboxOff bool

	// Usage is the usage-API cache, populated only when WantUsage.
	Usage *usageapi.CachedUsage

	// UsageStale marks a cache the fetch could not refresh, which the bar
	// renders with a "~" prefix.
	UsageStale bool
}

// hostState resolves the write-authority gate, performs every
// host-touching write this render implies, and returns the facts the
// render needs — either directly against the database or, inside a
// `tclaude-layer` sandbox where the database is not reachable, through
// agentd.
//
// Both paths run the same derivation and the same write set; only the
// transport differs. See render_shared.go.
func hostState(req renderRequest) renderFacts {
	if brokerRenders() {
		return brokeredHostState(req)
	}
	return directHostState(req)
}

// directHostState is the unbrokered path: exactly the reads and writes
// the statusbar has always performed, in the order it performed them.
func directHostState(req renderRequest) renderFacts {
	owned := ownedSessionID(req.EnvSessionID, req.RenderConvID)

	// The workspace row is keyed by CONV id and is therefore
	// self-attributing: a foreign child writes only its own row, never
	// the parent's. That is why it is not behind the ownership gate.
	workspaceConv := req.RenderConvID
	if workspaceConv == "" {
		workspaceConv = req.EnvSessionID
	}

	observed, resolved := resolvePinnedWindow(req.EnvPinnedWindow, owned)
	facts := renderFacts{
		Owned:         owned,
		WorkspaceConv: workspaceConv,
		PinnedWindow:  resolved,
		SandboxOff:    temporarySandboxOff(workspaceConv),
	}

	writes := renderWrites{
		Input:         req.Input,
		Payload:       req.Payload,
		Git:           req.Git,
		Derived:       deriveRender(req.Input, observed, resolved),
		Owned:         owned,
		WorkspaceConv: workspaceConv,
	}

	// The order below is deliberate rather than incidental. The usage-cache
	// PUBLISH precedes the cache-only usage READ so a render carrying buckets
	// is not answered from a cache one write staler than itself. The main write
	// set also precedes the read, while only the cost write has to follow it
	// because only cost needs the read's subscription verdict.
	updateUsageCacheFromRender(req.Input)
	applyRenderWrites(writes)

	hasLimits := hasSubscriptionLimits(req.Input)
	if !hasLimits && req.WantUsage {
		facts.Usage, facts.UsageStale = peekUsage()
		hasLimits = usageHasLimits(facts.Usage)
	}
	applyCostWrite(writes, hasLimits)
	return facts
}

// usageHasLimits reports whether the usage-cache fallback produced
// anything the bar will render as a limit bucket. It decides real cost
// from virtual cost, so it must agree exactly with the render below.
func usageHasLimits(usage *usageapi.CachedUsage) bool {
	if usage == nil {
		return false
	}
	return usage.FiveHour != nil || usage.SevenDay != nil ||
		(usage.SevenDaySonnet != nil && usage.SevenDaySonnet.Pct > 0)
}

// peekUsage reads the shared usage cache without ever refreshing it over the
// network. Both direct and brokered statusline renders use this path.
//
// A statusline callback normally publishes Claude's own rate-limit buckets
// into this cache before reading it. When the user explicitly enables
// usage.poll_anthropic_api, agentd may also refresh it in the background.
// Keeping the fallback cache-only prevents an ordinary render from acquiring
// Claude Code credentials and unexpectedly calling Anthropic's OAuth API; it
// also avoids putting network latency on a cosmetic display callback. A cache
// older than the TTL renders with the existing "~" prefix.
func peekUsage() (*usageapi.CachedUsage, bool) {
	usage := usageapi.Peek()
	if usage == nil {
		return nil, false
	}
	return usage, time.Since(usage.FetchedAt) > usageapi.CacheTTL
}
