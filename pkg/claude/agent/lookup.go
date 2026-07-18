package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

// Exit codes for lookup-shaped commands.
const (
	rcOK         = 0
	rcNotFound   = 1
	rcAmbiguous  = 2
	rcInvalidArg = 3
	rcIOFailure  = 4
	rcAuth       = 5
)

// findClaudePID is the parent-process walker used to detect whether the
// invoker has a Claude Code ancestor. Indirected through a variable so
// tests can stub it (the test runner itself is usually a CC descendant).
var findClaudePID = session.FindClaudePID

// ErrAmbiguous is returned by ResolveSelector when the selector matches
// more than one conversation. The caller should surface candidates.
var ErrAmbiguous = errors.New("selector matches multiple conversations")

// errAmbiguous is the package-internal alias kept for the existing CLI
// callers, which compare against this sentinel.
var errAmbiguous = ErrAmbiguous

// errNoAgentMatch tags the miss of an `agt_`-tagged selector. It is
// terminal: unlike a conv-id / title miss, refreshing the conv index can
// never make an agent appear (agents live in the agents table, not in conv
// .jsonl files), so resolveSelector returns it straight through instead of
// paying for a full ~/.claude/projects rescan-and-retry.
var errNoAgentMatch = errors.New("no agent matches")

// Resolved is a minimal handle to a conversation reachable by a selector.
// Exported so the daemon (pkg/claude/agentd) can reuse the resolver.
type Resolved struct {
	// AgentID is the stable actor key behind ConvID — the canonical ID the
	// CLI leads with when naming WHO it resolved; ConvID is the live
	// generation behind it (kept as the snapshot/hover). "" when the conv
	// is not (yet) a known agent. Stamped at the resolver chokepoint
	// (resolveSelector) so every consumer gets it without a second lookup.
	AgentID string
	ConvID  string
	Row     *db.ConvIndexRow // may be nil if conv not yet indexed
}

// resolved is the package-private alias used by existing CLI code.
type resolved = Resolved

// resolveSelector turns a free-form string (UUID, prefix, or current title)
// into a single conv-id. Search is global (any project) by default — agent
// groups are project-agnostic.
//
// `.` and `-` resolve to the current session via $TCLAUDE_SESSION_ID, with
// the parent CC pid file as fallback.
//
// On a miss, we do a global rescan of `~/.claude/projects/*` and try once
// more so brand-new convs (or convs that were just renamed) get picked up
// without requiring a manual `tclaude conv ls -g` first.
//
// Succession redirect: after a successful resolve, if the conv-id has a
// recorded successor (because the original was reincarnated), the
// resolver returns the latest conv in the chain. This lets stale
// references from CLI / cron / handlers transparently target the live
// conv without callers having to thread `db.ResolveLatestConv` through
// every code path.
func resolveSelector(selector string) (*resolved, []*resolved, error) {
	if selector == "" {
		return nil, nil, fmt.Errorf("selector is required")
	}

	if selector == "." || selector == "-" {
		id, err := currentConvID()
		if err != nil {
			return nil, nil, err
		}
		selector = id
	}

	if r, matches, err := tryResolve(selector); err == nil || errors.Is(err, errAmbiguous) || errors.Is(err, errNoAgentMatch) {
		return stampAgentID(redirectResolvedToLatest(r)), stampAgentIDs(matches), err
	}

	// Cache miss. Refresh the index across all projects and try again.
	refreshAllProjects()
	r, matches, err := tryResolve(selector)
	return stampAgentID(redirectResolvedToLatest(r)), stampAgentIDs(matches), err
}

// stampAgentID fills in the resolved conv's stable agent_id (the canonical
// actor key) so consumers can name WHO without a second db.AgentIDForConv
// lookup. Resolved AFTER redirectResolvedToLatest so the agent_id matches
// the live generation we hand back, not a stale pre-redirect conv. A
// non-agent conv (or an unindexed/unknown one) leaves AgentID "". Returns r
// unchanged (nil-safe) so it can wrap the return expression inline.
func stampAgentID(r *resolved) *resolved {
	if r == nil {
		return nil
	}
	r.AgentID, _ = db.AgentIDForConv(r.ConvID)
	return r
}

// stampAgentIDs stamps every ambiguity candidate the same way as the primary
// result, so a consumer reading .AgentID off a candidate (e.g. the daemon's
// peerEntriesFromResolved) gets the stable id without its own lookup.
// Candidates aren't succession-redirected (they're alternatives, not the
// chosen head), so each is stamped from its own conv-id. Returns the slice
// for inline use.
func stampAgentIDs(matches []*resolved) []*resolved {
	for _, m := range matches {
		stampAgentID(m)
	}
	return matches
}

// redirectResolvedToLatest follows the agent_conv_succession chain
// from r.ConvID forward and returns a resolved pointing at the live
// conv. Returns r unchanged when there's no chain edge (the common
// case) or when the chain head is the same conv-id we already have.
//
// On a successful redirect we re-fetch the conv_index row for the new
// conv so display titles / metadata reflect the live conv. If that
// fetch fails (e.g. the new conv isn't indexed yet), we keep the old
// row but stamp the new ConvID — the caller can still send messages /
// look up sessions; only the title might be momentarily stale.
func redirectResolvedToLatest(r *resolved) *resolved {
	if r == nil {
		return nil
	}
	latest := db.ResolveLatestConv(r.ConvID)
	if latest == r.ConvID {
		return r
	}
	out := &resolved{ConvID: latest, Row: r.Row}
	if newRow, err := db.GetConvIndex(latest); err == nil && newRow != nil {
		out.Row = newRow
	}
	return out
}

// tryResolve runs the cached lookup chain (UUID → prefix → title →
// group-member conv-id/prefix) without touching disk beyond the SQLite
// cache.
func tryResolve(selector string) (*resolved, []*resolved, error) {
	// 0) global head-alias lookup. A handle (e.g. "po", "ceo")
	//    resolves to the current head of its conv chain — survives
	//    arbitrary reincarnation depth via ResolveLatestConv. Handles
	//    are validated to never shadow UUIDs / "group:" / "."/"-", so
	//    this branch can take precedence without ambiguity.
	if head, err := db.ResolveHeadAlias(selector); err == nil && head != "" {
		row, _ := db.GetConvIndex(head)
		return &resolved{ConvID: head, Row: row}, nil, nil
	}

	// 0.5) stable agent_id — the canonical, rotation-immune handle. A
	//      selector tagged `agt_` is taken as an explicit agent_id and
	//      resolved straight to the actor's current generation
	//      (agents.current_conv_id), regardless of how many times the
	//      agent has reincarnated. Retired actors resolve too (to their
	//      last generation) so a stable id can still reference them.
	//      Full id or unique prefix resolves; several matches surface as
	//      an ambiguity; zero is reported against the agent layer rather
	//      than falling through — the `agt_` tag is an explicit "this is
	//      an agent id", so a mistyped id gets a precise error instead of
	//      a generic miss. (The corner of a conversation deliberately
	//      titled `agt_…` is therefore not reachable by that title; an
	//      acceptable trade for the precise error on the common path.)
	if strings.HasPrefix(selector, db.AgentIDPrefix) {
		matches, err := db.FindAgentsByIDPrefix(selector)
		if err != nil {
			return nil, nil, err
		}
		switch len(matches) {
		case 1:
			conv := matches[0].CurrentConvID
			row, _ := db.GetConvIndex(conv)
			return &resolved{ConvID: conv, Row: row}, nil, nil
		case 0:
			return nil, nil, fmt.Errorf("%w %q", errNoAgentMatch, selector)
		default:
			cands := make([]*resolved, 0, len(matches))
			for _, a := range matches {
				row, _ := db.GetConvIndex(a.CurrentConvID)
				cands = append(cands, &resolved{ConvID: a.CurrentConvID, Row: row})
			}
			return nil, cands, errAmbiguous
		}
	}

	// 1) full UUID match
	if row, err := db.GetConvIndex(selector); err == nil && row != nil {
		return &resolved{ConvID: row.ConvID, Row: row}, nil, nil
	}

	// 2) prefix match (uniqueness enforced by db.FindConvIndexByPrefix)
	if row, err := db.FindConvIndexByPrefix(selector); err == nil && row != nil {
		return &resolved{ConvID: row.ConvID, Row: row}, nil, nil
	}

	// 3) match by the current display title shown on agent listing surfaces:
	//    custom > pending name > summary > first prompt. The pending-name
	//    fallback is load-bearing for a freshly spawned Codex agent: its
	//    --name is visible in group members / the dashboard before the native
	//    title-store write has populated conv_index.custom_title, so that same
	//    visible name must already be a usable selector.
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return nil, nil, err
	}
	pendingByConv, err := db.PendingNamesByConv()
	if err != nil {
		return nil, nil, err
	}
	var matches []*resolved
	indexed := make(map[string]bool, len(rows))
	for _, r := range rows {
		indexed[r.ConvID] = true
		if displayTitleFromParts(r, pendingByConv[r.ConvID]) == selector {
			matches = append(matches, &resolved{ConvID: r.ConvID, Row: r})
		}
	}
	// A just-spawned agent can be visible from its actor/group rows before it
	// has any conv_index row at all. Include those pending-only current heads;
	// once an index row exists the loop above owns the precedence decision, so
	// an authoritative custom title never leaves the old pending name as a
	// hidden alias.
	for convID, pending := range pendingByConv {
		if !indexed[convID] && pending == selector {
			matches = append(matches, &resolved{ConvID: convID})
		}
	}
	// Map iteration above is deliberately unordered. Sort every title match so
	// ambiguity diagnostics and API candidate arrays remain stable.
	sort.Slice(matches, func(i, j int) bool { return matches[i].ConvID < matches[j].ConvID })
	if len(matches) == 1 {
		return matches[0], nil, nil
	}
	if len(matches) > 1 {
		return nil, matches, errAmbiguous
	}

	// 4) fallback to agent_group_members so a freshly-spawned conv is
	//    findable by conv-id / prefix before its .jsonl gets scanned
	//    into conv_index (spawn writes the membership row immediately,
	//    but the title only lands once the /rename injection is
	//    processed and the file is scanned).
	//
	//    Distinct by conv_id: an agent can be a member of multiple
	//    groups, but it's still one conv.
	if mem, err := db.FindAgentMembersBySelector(selector); err == nil && len(mem) > 0 {
		seen := map[string]bool{}
		var unique []*resolved
		for _, m := range mem {
			if seen[m.ConvID] {
				continue
			}
			seen[m.ConvID] = true
			// Prefer to attach the conv-index row when it exists so
			// downstream renderers can show titles.
			row, _ := db.GetConvIndex(m.ConvID)
			unique = append(unique, &resolved{ConvID: m.ConvID, Row: row})
		}
		if len(unique) == 1 {
			return unique[0], nil, nil
		}
		if len(unique) > 1 {
			return nil, unique, errAmbiguous
		}
	}

	// 5) succession-chain fallback: a raw conv-id that's been pruned
	//    from conv_index and isn't a member of any group can still be
	//    resolvable if it has a recorded successor. Walking the chain
	//    forward from the input picks up "Bob's old UUID got reincarnated
	//    into Bob-r-1 which IS indexed" cases. The chain row alone is
	//    enough to declare the input a known historical id; we accept
	//    the walked head as the resolution result.
	if walked := db.ResolveLatestConv(selector); walked != selector {
		row, _ := db.GetConvIndex(walked)
		return &resolved{ConvID: walked, Row: row}, nil, nil
	}

	return nil, nil, fmt.Errorf("no conversation matches %q", selector)
}

// refreshAllProjects walks ~/.claude/projects/* and runs the same
// LoadSessionsIndex pass that `conv ls -g` does, picking up new convs
// and applying the per-file mtime check to refresh stale ones. Errors
// are intentionally swallowed — the caller will surface a clearer
// "no conversation matches" if the second tryResolve still fails.
//
// Indirected through a var so tests can assert whether the rescan-and-retry
// path fired: an `agt_` miss must skip it (terminal errNoAgentMatch), while
// a conv-id / title miss must still trigger it.
var refreshAllProjects = func() {
	projectsDir := conv.ClaudeProjectsDir()
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_, _ = conv.LoadSessionsIndex(filepath.Join(projectsDir, e.Name()))
	}
}

// ResolveSelector is the daemon-friendly entry point that mirrors the
// internal resolveSelector. Returns (resolved, candidates, err) where err
// may be ErrAmbiguous with candidates populated.
func ResolveSelector(selector string) (*Resolved, []*Resolved, error) {
	return resolveSelector(selector)
}

// ResolveSelectorCached is ResolveSelector without the global project refresh
// on a cache miss. Use it when the existence of a caller-controlled selector
// is authorization-sensitive: a miss must stay bounded to SQLite lookups
// instead of revealing itself through a filesystem walk
// and cache writes. Callers that are entitled to discovery diagnostics should
// keep using ResolveSelector.
func ResolveSelectorCached(selector string) (*Resolved, []*Resolved, error) {
	if selector == "" {
		return nil, nil, fmt.Errorf("selector is required")
	}
	if selector == "." || selector == "-" {
		id, err := currentConvID()
		if err != nil {
			return nil, nil, err
		}
		selector = id
	}
	r, matches, err := tryResolve(selector)
	return stampAgentID(redirectResolvedToLatest(r)), stampAgentIDs(matches), err
}

// CurrentConvID returns the conv-id of the conversation invoking the
// caller, expanded to a full UUID via the DB if necessary. Exported for
// callers outside this package.
func CurrentConvID() (string, error) {
	return currentConvID()
}

// DisplayTitle is the exported alias of displayTitle so other packages
// can render an agent's bare *name* consistently (custom title, else
// summary, else first prompt). This is the agent-name concept — used
// for identity, group rosters and reincarnate/clone name prefixes. For
// the "[title]: prompt" rendering that conversation-listing surfaces
// show (`conv ls`, the dashboard's plain-conversation list), use
// FreshConvTitle instead.
func DisplayTitle(r *db.ConvIndexRow) string {
	return displayTitle(r)
}

// FreshConvRow refreshes a single conv's index row before returning it.
func FreshConvRow(convID string) *db.ConvIndexRow {
	return freshConvRow(convID)
}

// FreshConvRowAt is FreshConvRow with a cwd hint. When the conv has no
// conv_index row yet (freshly-spawned by reincarnate / clone, never
// touched by `tclaude conv ls` or the watch model), we derive the
// .jsonl path from cwd + convID and scan it directly so callers can
// still read CustomTitle. Returns nil if neither the cache nor the
// derived path produced data.
//
// Why this exists: reincarnate's prevTitle lookup used to call
// FreshConvRow on the parent and silently get nil when the parent's
// conv-id had never been indexed — producing names like `reincarnate-1`
// instead of `<parent-name>-reincarnate-N`. The cwd from the parent's
// session row is the missing piece.
func FreshConvRowAt(convID, cwd string) *db.ConvIndexRow {
	if row := freshConvRow(convID); row != nil {
		return row
	}
	if cwd == "" {
		return nil
	}
	projectPath := conv.GetClaudeProjectPath(cwd)
	if projectPath == "" {
		return nil
	}
	jsonlPath := filepath.Join(projectPath, convID+".jsonl")
	conv.ScanAndUpsertFile(jsonlPath)
	return freshConvRow(convID)
}

// FreshConvRowResolved is FreshConvRow with the cwd hint sourced
// automatically from the most-recent session row for this conv. It's
// the right choice for callers that want the .jsonl-scan fallback but
// don't already have a cwd in scope (the dashboard is the prototypical
// example).
//
// Without this fallback, fresh-spawned reincarnations / clones display
// as "(unknown)" in the dashboard until the first `tclaude conv ls`
// indexes them — same root cause as the prevTitle="" reincarnate-prefix
// bug, just visible in a different surface.
func FreshConvRowResolved(convID string) *db.ConvIndexRow {
	if row := freshConvRow(convID); row != nil {
		return row
	}
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return FreshConvRowAt(convID, rows[0].Cwd)
}

// UnknownTitle is the placeholder a listing surface shows for a conv
// whose display name can't be resolved at all (no conv_index row, no
// session row to derive a cwd from, deleted .jsonl).
const UnknownTitle = "(unknown)"

// FreshTitle resolves convID to the display name shown on listing
// surfaces (`agent ls`, `groups members`, the dashboard). It refreshes
// the conv_index row from the underlying .jsonl first via
// FreshConvRowResolved, so a conv that was renamed or freshly spawned
// since the last index pass shows its real name rather than a stale
// title.
//
// Resolution priority: custom title > pending name > summary > first
// prompt > "(unknown)".
//
//   - A custom title (the agent's own /rename, or the /rename the
//     daemon injects just after spawn) is the authoritative name.
//   - When none exists yet — a freshly-spawned agent in the gap before
//     its /rename lands — the pending name recorded at spawn time
//     (agents.pending_name, the `--name` argument) stands in,
//     so the dashboard shows the intended name rather than "(unknown)".
//     It deliberately outranks summary / first-prompt: the human-given
//     --name is a stronger identity signal than an auto-summary or an
//     uncleaned first prompt (often the spawn welcome line).
//
// This is the common "name an agent for display" helper — prefer it
// over a bare db.GetConvIndex in any handler that renders agent names,
// so every surface picks up source changes uniformly.
func FreshTitle(convID string) string {
	row := FreshConvRowResolved(convID)
	if row != nil && row.CustomTitle != "" {
		return row.CustomTitle
	}
	// No custom title yet. Fall back to the pending name the spawn
	// recorded. Pure fallback: the moment a custom title lands the
	// branch above supersedes it — no flicker, nothing to clear.
	if pn := pendingName(convID); pn != "" {
		return pn
	}
	if row != nil {
		if t := displayTitle(row); t != "" {
			return t
		}
	}
	return UnknownTitle
}

// CachedTitle resolves convID to its display name from the conv_index
// cache ONLY. It is FreshTitle's cache-only twin — identical resolution
// priority (custom title > pending name > summary > first prompt >
// UnknownTitle) — but it trusts the cached row instead of refreshing it
// from the .jsonl.
//
// Use it on the agentd dashboard poll and similar hot paths: the
// fsnotify monitor (agentd/fsnotify.go) keeps conv_index fresh, so a
// per-row rescan is pure waste. The pending-name fallback still names a
// just-spawned agent in the brief gap before its first index event
// lands. Off that hot path, or where the monitor may not be running
// (the CLI), use FreshTitle.
func CachedTitle(convID string) string {
	row, _ := db.GetConvIndex(convID)
	return CachedTitleFromParts(row, pendingName(convID))
}

// CachedTitleFromParts is CachedTitle's preloaded-parts twin: it applies the
// identical resolution priority (custom title > pending name > summary > first
// prompt > UnknownTitle) to a conv_index row and actor pending-name the caller
// already has in hand, reading nothing itself. The dashboard snapshot's
// per-request batch loader uses it so a member/agent/owner row resolves its
// title with zero point queries (TCL-368). Pass a nil row / empty pendingName
// for a conv with no cached index / non-agent — the result degrades to
// UnknownTitle exactly as CachedTitle would.
func CachedTitleFromParts(row *db.ConvIndexRow, pendingName string) string {
	if title := displayTitleFromParts(row, pendingName); title != "" {
		return title
	}
	return UnknownTitle
}

// displayTitleFromParts applies the shared agent-name precedence without an
// unknown placeholder: a native/custom title is authoritative; the actor's
// spawn-time pending name fills the pre-rename gap; conversation-derived
// summary / first prompt are the final fallback. Selector matching and terse
// decorations use the empty result to mean "no name", while CachedTitle adds
// UnknownTitle for listing surfaces.
func displayTitleFromParts(row *db.ConvIndexRow, pendingName string) string {
	if row != nil && row.CustomTitle != "" {
		return row.CustomTitle
	}
	if pendingName != "" {
		return pendingName
	}
	if row != nil {
		return displayTitle(row)
	}
	return ""
}

// CachedCreated returns convID's conversation creation timestamp
// (RFC3339 — the first .jsonl event's time) from the conv_index cache,
// or "" when unknown. Creation time is immutable once recorded, so the
// cache is authoritative; use this on the dashboard hot path where a
// per-row .jsonl rescan would be pure waste (the fsnotify monitor keeps
// conv_index fresh). It is FreshCreated's cache-only twin, mirroring
// CachedTitle vs FreshTitle.
func CachedCreated(convID string) string {
	if row, _ := db.GetConvIndex(convID); row != nil {
		return row.Created
	}
	return ""
}

// FreshCreated returns convID's conversation creation timestamp
// (RFC3339), refreshing the conv_index row from the underlying .jsonl
// first (FreshConvRowResolved) so a freshly-spawned, not-yet-indexed
// conv still reports a creation time. Use it off the hot path — the CLI
// `groups members` listing — exactly where FreshTitle is used.
func FreshCreated(convID string) string {
	if row := FreshConvRowResolved(convID); row != nil {
		return row.Created
	}
	return ""
}

// MemberCreated returns convID's Age timestamp for a group-member listing.
//
// It returns the earliest valid instant from the actor's immutable birth time
// (agents.created_at) and the conversation's first-.jsonl-event time. Actor birth
// normally wins and is available before the harness writes its first event, so
// freshly-spawned agents have an Age immediately and keep it across rotations.
// Conversation creation wins only for legacy/backfilled actors whose row was
// stamped after they already existed. The UTC RFC3339Nano result is
// byte-identical to the dashboard snapshot's createdFor.
func MemberCreated(convID string) string {
	actorCreated := ""
	if a, _ := db.GetAgentByConv(convID); a != nil && !a.CreatedAt.IsZero() {
		actorCreated = db.CanonicalAgeTimestampFromTime(a.CreatedAt)
	}
	return db.EarliestAgeTimestamp(actorCreated, FreshCreated(convID))
}

// pendingName returns the intended display name recorded for convID's actor at
// spawn time (agents.pending_name), or "" when the conv was not spawned with a
// name or is not an agent. Errors are swallowed — a pending name is a
// best-effort display fallback, never load-bearing.
func pendingName(convID string) string {
	a, err := db.GetAgentByConv(convID)
	if err != nil || a == nil {
		return ""
	}
	return a.PendingName
}

// FreshConvTitle resolves convID to the canonical "[title]: prompt"
// display string — the exact rendering `conv ls` / `conv ls -w` show,
// via the single source of truth convindex.FormatConvTitle —
// refreshing the conv_index row from the .jsonl first (like FreshTitle).
//
// Use this for *plain conversations* (non-agents): the web dashboard's
// promotion list renders through it so a plain conv's title matches the
// CLI instead of leaking a raw, uncleaned first prompt (issue #91).
// FreshTitle (the bare agent name) stays in use for agents — identity,
// group rosters, reincarnate/clone name prefixes.
//
// UnknownTitle is returned only when the conv can't be resolved at all
// (no conv_index row, no .jsonl). A resolved conv with no title
// material yields FormatConvTitle's canonical empty string verbatim —
// exactly what `conv ls` would print — so the two surfaces stay in
// lockstep; the dashboard's own "(untitled)" fallback covers the blank.
func FreshConvTitle(convID string) string {
	if row := FreshConvRowResolved(convID); row != nil {
		return convindex.FormatConvTitle(row.CustomTitle, row.Summary, row.FirstPrompt)
	}
	return UnknownTitle
}

// FreshBranch resolves convID to the git branch the agent is working
// on *now*. It returns ResolveLocation's CurrentBranch — the branch of
// the dir the agent last edited a file in, which tracks the agent as
// it moves between sub-repos. When the agent hasn't edited anything
// yet (or the hook predates worktree tracking) this falls back to the
// launch dir's branch, so the value degrades sensibly rather than
// going blank. Returns "" when no branch can be determined.
//
// Prefer ResolveLocation directly in any handler that also needs the
// startup branch or the directories; FreshBranch is the convenience
// shorthand for callers that only want the one value.
func FreshBranch(convID string) string {
	return ResolveLocation(convID).CurrentBranch
}

// ShortID truncates a conv-id to the first 8 hex chars.
func ShortID(convID string) string {
	return short(convID)
}

// TitleFor returns convID's cache-only display title (custom title, else the
// actor's pending name, else summary / first prompt), or "" when no name is
// known. Cheap — no conversation-store rescan — so it suits decorating
// receipts, message nudges, and inbox headers. For the refreshed,
// rescan-backed lookup used by listing surfaces, use FreshTitle instead.
func TitleFor(convID string) string {
	return titleFor(convID)
}

// displayTitle returns the title we treat as the agent's name: custom title
// if set, else summary, else first prompt.
func displayTitle(r *db.ConvIndexRow) string {
	if r.CustomTitle != "" {
		return r.CustomTitle
	}
	if r.Summary != "" {
		return r.Summary
	}
	return r.FirstPrompt
}

// freshConvRow returns the conv_index row for convID, rescanning the
// underlying .jsonl when its mtime is newer than the cached row. The actual
// freshness check lives in conv.RefreshConvIndexEntry — same pattern that
// LoadSessionsIndex uses on every `conv ls`.
func freshConvRow(convID string) *db.ConvIndexRow {
	return conv.RefreshConvIndexEntry(convID)
}

// currentConvID returns the conv-id of the conversation invoking us. We
// prefer Claude Code's per-pid session file (tracks `/clear` and `/resume`)
// and fall back to the env var that tclaude sets at session launch.
//
// $TCLAUDE_SESSION_ID is sometimes set to the 8-char prefix rather than
// the full UUID, so we expand prefixes via the DB before returning, so
// downstream callers can rely on getting the canonical full ID.
//
// Final fallback: ask the daemon via /v1/whoami. The daemon resolves
// identity from the Unix socket's peer credentials (host PID), which
// works even when our process tree is hidden by a sandbox PID
// namespace (e.g. bwrap with --unshare-pid). Cheap roundtrip — the
// daemon is local and the lookup is one query.
func currentConvID() (string, error) {
	if id := readCCSessionID(); id != "" {
		return id, nil
	}
	if id := os.Getenv("TCLAUDE_SESSION_ID"); id != "" {
		// Already a full UUID? Take it.
		if len(id) == 36 {
			return id, nil
		}
		// Prefix → full UUID via the DB.
		if row, err := db.FindConvIndexByPrefix(id); err == nil && row != nil {
			return row.ConvID, nil
		}
		return id, nil
	}
	if id := whoamiViaDaemon(); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("could not detect current conversation; pass an explicit conv ID")
}

// whoamiViaDaemon asks the running agentd "who am I?" via peer-cred.
// Returns "" if the daemon isn't running, or the caller is a human
// (no Claude ancestor from the daemon's view), or any other failure.
// Side-effect-free for the not-running case so command-line tools
// don't error on hosts without agentd.
//
// Indirected through a variable so tests can stub it (the test runner
// itself shouldn't reach a real daemon).
var whoamiViaDaemon = func() string {
	if SocketPath() == "" {
		return ""
	}
	var resp struct {
		IsHuman bool   `json:"is_human"`
		ConvID  string `json:"conv_id"`
	}
	if err := DaemonGet("/v1/whoami", &resp); err != nil {
		return ""
	}
	if resp.IsHuman {
		return ""
	}
	return resp.ConvID
}

// readCCSessionID walks up to the parent CC pid and reads
// ~/.claude/sessions/<pid>.json for its `sessionId`.
func readCCSessionID() string {
	pid := session.FindClaudePID()
	if pid == 0 {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	id, _ := m["sessionId"].(string)
	return id
}

// printAmbiguous writes the matching candidates so the user can disambiguate.
func printAmbiguous(out io.Writer, selector string, matches []*resolved) {
	fmt.Fprintf(out, "Error: selector %q matches %d conversations:\n", selector, len(matches))
	for _, m := range matches {
		title := titleFor(m.ConvID)
		fmt.Fprintf(out, "  %s  %s\n", m.ConvID[:8], title)
	}
	fmt.Fprintf(out, "Disambiguate by ID prefix.\n")
}

// --- whoami ---

type whoamiParams struct{}

func whoamiCmd() *cobra.Command {
	return boa.CmdT[whoamiParams]{
		Use:         "whoami",
		Short:       "Print the current conversation's ID and display name",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *whoamiParams, _ *cobra.Command, _ []string) {
			os.Exit(runWhoami(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// HumanIdentity is what whoami prints when the invocation is not from
// inside a Claude Code session — i.e. the coordinating human running
// tclaude directly from a shell.
const HumanIdentity = "<human>"

func runWhoami(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runWhoamiDaemon(stdout, stderr)
}

func runWhoamiDaemon(stdout, stderr io.Writer) int {
	var resp struct {
		IsHuman bool     `json:"is_human"`
		AgentID string   `json:"agent_id"`
		ConvID  string   `json:"conv_id"`
		Title   string   `json:"title"`
		Phases  []string `json:"phases"`
	}
	if err := DaemonGet("/v1/whoami", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.IsHuman {
		fmt.Fprintln(stdout, HumanIdentity)
		return rcOK
	}
	title := resp.Title
	if title == "" {
		title = "(unnamed)"
	}
	// Lead with the stable agent_id; fall back to conv-id only if the
	// caller isn't enrolled as an agent yet.
	id := resp.AgentID
	if id == "" {
		id = resp.ConvID
	}
	fmt.Fprintf(stdout, "%s\t%s\n", id, title)
	// Advisory process (JOH-242): one line per group with a process, showing
	// its current phase.
	for _, ph := range resp.Phases {
		fmt.Fprintf(stdout, "  %s\n", ph)
	}
	return rcOK
}

func runWhoamiDirect(stdout, stderr io.Writer) int {
	id, err := currentConvID()
	if err != nil {
		// No conv-id resolvable. If there's no `claude` ancestor in the
		// process tree, the invoker is the human — say so.
		if findClaudePID() == 0 {
			fmt.Fprintln(stdout, HumanIdentity)
			return rcOK
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	row := freshConvRow(id)
	title := "(unnamed)"
	if row != nil {
		if t := displayTitle(row); t != "" {
			title = t
		}
	}
	// Lead with the stable agent_id when this conv is enrolled; otherwise
	// fall back to the conv-id (the direct path can run before enrollment).
	display := id
	if aid, _ := db.AgentIDForConv(id); aid != "" {
		display = aid
	}
	fmt.Fprintf(stdout, "%s\t%s\n", display, title)
	return rcOK
}

// --- lookup ---

type lookupParams struct {
	Selector string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
}

func lookupCmd() *cobra.Command {
	return boa.CmdT[lookupParams]{
		Use:         "lookup",
		Short:       "Resolve an agent name (or ID prefix) to its stable agent_id",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *lookupParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *lookupParams, _ *cobra.Command, _ []string) {
			os.Exit(runLookup(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runLookup(p *lookupParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runLookupDaemon(p, stdout, stderr)
}

func runLookupDaemon(p *lookupParams, stdout, stderr io.Writer) int {
	var resp struct {
		ConvID  string `json:"conv_id"`
		AgentID string `json:"agent_id"`
	}
	err := DaemonGet("/v1/lookup?selector="+url.QueryEscape(p.Selector), &resp)
	if de, ok := err.(*DaemonError); ok && de.Code == "ambiguous" {
		// The daemon returns 409 with a candidates list. Surface the
		// candidates the same way the direct path does.
		fmt.Fprintf(stderr, "%s\n", de.Msg)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, lookupID(resp.AgentID, resp.ConvID))
	return rcOK
}

func runLookupDirect(p *lookupParams, stdout, stderr io.Writer) int {
	r, matches, err := resolveSelector(p.Selector)
	if errors.Is(err, errAmbiguous) {
		printAmbiguous(stderr, p.Selector, matches)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	fmt.Fprintln(stdout, lookupID(r.AgentID, r.ConvID))
	return rcOK
}

// lookupID is what `tclaude agent lookup` prints: the stable agent_id when
// the target is an agent (the canonical reference), else the conv-id for a
// plain conversation. Either is a valid selector for the rest of the CLI.
func lookupID(agentID, convID string) string {
	if agentID != "" {
		return agentID
	}
	return convID
}

// --- ls (peers in my groups) ---

type lsParams struct {
	State string `long:"state" optional:"true" help:"Filter: online | offline"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func lsCmd() *cobra.Command {
	return boa.CmdT[lsParams]{
		Use:         "ls",
		Short:       "List agents reachable to me (members of my groups)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *lsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.State).SetAlternativesFunc(completeStateFilterValues)
			return nil
		},
		RunFunc: func(p *lsParams, _ *cobra.Command, _ []string) {
			os.Exit(runLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type peerEntry struct {
	// AgentID is the stable, rotation-immune actor key — what `agent ls`
	// shows as the canonical ID. ConvID is the live generation behind it.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Role    string `json:"role,omitempty"`
	Descr   string `json:"descr,omitempty"`
	// Branch is the git branch / worktree the agent is working on, as
	// recorded in its conv_index row (Claude Code stamps gitBranch into
	// every .jsonl turn). Empty when the conv isn't indexed yet or the
	// session isn't inside a git repo.
	Branch string   `json:"branch,omitempty"`
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
}

func runLs(p *lsParams, stdout, stderr io.Writer) int {
	if _, _, err := parseStateFilter(p.State); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runLsDaemon(p, stdout, stderr)
}

func runLsDaemon(p *lsParams, stdout, stderr io.Writer) int {
	var peers []*peerEntry
	if err := DaemonGet("/v1/peers", &peers); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	wantOnline, applyState, _ := parseStateFilter(p.State)
	if applyState {
		filtered := make([]*peerEntry, 0, len(peers))
		for _, pe := range peers {
			if pe.Online == wantOnline {
				filtered = append(filtered, pe)
			}
		}
		peers = filtered
	}
	return renderPeers(p, peers, stdout)
}

func renderPeers(p *lsParams, peers []*peerEntry, stdout io.Writer) int {
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(peers); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(peers) == 0 {
		fmt.Fprintln(stdout, "(no peers — you are not in any groups, or your groups have no other members)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 12},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "ROLE", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "GROUPS", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "BRANCH", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.2, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, pe := range peers {
		// ID is the stable agent_id (short form for the table); NAME is the
		// conv's display title. conv-id is available via --json.
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(pe.Online),
			shortAgentID(pe.AgentID, pe.ConvID),
			pe.Title,
			pe.Role,
			strings.Join(pe.Groups, ","),
			pe.Branch,
			pe.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}
