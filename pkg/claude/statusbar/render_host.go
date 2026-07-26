package statusbar

import (
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

	derived := deriveRender(req.Input, observed, resolved)
	hasLimits := hasSubscriptionLimits(req.Input)
	// The usage cache is written before it is read — see
	// updateUsageCacheFromRender.
	updateUsageCacheFromRender(req.Input)
	if !hasLimits && req.WantUsage {
		facts.Usage, facts.UsageStale = cachedUsage()
		hasLimits = usageHasLimits(facts.Usage)
	}

	applyRenderWrites(renderWrites{
		Input:         req.Input,
		Payload:       req.Payload,
		Git:           req.Git,
		Derived:       derived,
		Owned:         owned,
		WorkspaceConv: workspaceConv,
		HasLimits:     hasLimits,
	})
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

// cachedUsage reads the usage-API cache, reporting separately whether the
// value is a stale one the refresh could not replace.
func cachedUsage() (*usageapi.CachedUsage, bool) {
	usage, err := usageapi.GetCached()
	if usage == nil {
		return nil, false
	}
	return usage, err != nil
}
