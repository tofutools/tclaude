package agentd

import (
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// snapshotRowCache batches every per-conv read the dashboard snapshot needs
// (TCL-368/TCL-367). Building one member/agent/owner row used to fire ~13
// SQLite point queries, and the snapshot rebuilds the same conv 2-3× per poll
// (member loop, addAgent, owners pass) every 2s. On top of that,
// agent.ResolveLocation FULLY REPARSED a busy agent's .jsonl on nearly every
// poll (the fsnotify 500ms debounce keeps conv_index stale while an agent
// streams).
//
// The cache fixes both: the caller collects the whole conv set up front, then
// newSnapshotRowCache bulk-loads conv_index, sessions, agent_workdir,
// agent_workspace and the actor identities in one chunked IN-query each,
// precomputes every conv's location through the pure agent.ResolveLocationFromParts
// (no DB, no .jsonl, no git — this is the TCL-367 fix), collects the branch-link
// cache keys those locations need and loads git_cache in one more batch. viewFor
// then assembles a conv's full row bundle from memory and memoizes it, so the
// member loop, addAgent and the owners pass share a single computation.
type snapshotRowCache struct {
	alive      map[string]struct{}
	convIndex  map[string]*db.ConvIndexRow
	sessions   map[string][]*db.SessionRow
	workdirs   map[string]db.AgentWorkdir
	workspaces map[string]db.AgentWorkspace
	agents     map[string]db.ConvAgent
	gitCache   map[string]*db.GitCacheRow

	locs map[string]agentLocationView
	memo map[string]*convRowBundle

	codexTelemetryDuration time.Duration
}

// convRowBundle is the fully-resolved per-conv row the dashboard renders,
// computed once and shared by every surface (group member, Agents roster,
// pure-owner). Links carries the branch/PR link block WITHOUT PresentedPRs —
// those are keyed by agent-id from a separate snapshot-wide preload, so each
// caller grafts them onto its own copy of Links.
type convRowBundle struct {
	AgentID string
	Title   string
	Created string
	Loc     agentLocationView
	Links   repoLinksView
	Online  bool
	State   agentState
}

// newSnapshotRowCache bulk-loads every table the snapshot rows read, keyed by
// conv-id, and precomputes locations + the git_cache batch. convIDs is the full
// conv set the snapshot will render (group members ∪ owners ∪ grant/override
// holders ∪ active agents ∪ sudo holders); alive is the one-per-poll tmux
// liveness set.
//
// A DB error on a loader degrades that table to empty, so its rows resolve to
// their zero-value ("unknown"/offline) shape rather than failing the whole poll.
// The sessions loader (FindSessionsByConvIDs) is additionally best-effort per
// row: one undecodable session row is skipped, not fatal — so a single corrupt
// effective_sandbox_config can never blank liveness/state for the ENTIRE poll
// (it would if the loader aborted and we discarded its error here).
// record receives the named loader timings in declaration order; nil disables
// publication while retaining the same load path.
func newSnapshotRowCache(
	convIDs []string,
	alive map[string]struct{},
	record func([]perfPhase),
) *snapshotRowCache {
	rc := &snapshotRowCache{
		alive: alive,
		memo:  make(map[string]*convRowBundle, len(convIDs)),
		locs:  make(map[string]agentLocationView, len(convIDs)),
	}
	// These tables are independent WAL reads. Running them concurrently keeps
	// preload wall-clock bounded by the slowest batch instead of summing six
	// SQLite round trips on every 2-second dashboard poll.
	phases := runSnapshotNamedLoads(
		snapshotNamedLoad{"conv_index", func() { rc.convIndex, _ = db.GetConvIndexBatch(convIDs) }},
		snapshotNamedLoad{"sessions", func() { rc.sessions, _ = db.FindSessionsByConvIDs(convIDs) }},
		snapshotNamedLoad{"workdirs", func() { rc.workdirs, _ = db.ListAgentWorkdirsByConv(convIDs) }},
		snapshotNamedLoad{"workspaces", func() { rc.workspaces, _ = db.ListAgentWorkspacesByConv(convIDs) }},
		snapshotNamedLoad{"agents", func() { rc.agents, _ = db.AgentsByConv(convIDs) }},
	)

	// Precompute every conv's location so we know which (dir, branch) pairs the
	// branch-link lookups will key on, then load all their git_cache rows in one
	// batch. Deduped so a shared repo/branch across many agents is one key.
	locationsStart := time.Now()
	keySet := make(map[string]struct{}, len(convIDs))
	for _, convID := range convIDs {
		loc := rc.computeLoc(convID)
		rc.locs[convID] = loc
		if loc.CurrentDir != "" && loc.Branch != "" {
			keySet[branchLinkCacheKey(loc.CurrentDir, loc.Branch)] = struct{}{}
		}
		if loc.StartupDir != "" && loc.StartupBranch != "" {
			keySet[branchLinkCacheKey(loc.StartupDir, loc.StartupBranch)] = struct{}{}
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	phases = append(phases, perfPhase{Name: "locations", Ms: durMs(time.Since(locationsStart))})
	gitCacheStart := time.Now()
	rc.gitCache, _ = db.LoadGitCacheBatch(keys)
	phases = append(phases, perfPhase{Name: "git_cache", Ms: durMs(time.Since(gitCacheStart))})
	if record != nil {
		record(phases)
	}
	return rc
}

// computeLoc resolves a conv's location purely from the batch-loaded parts. The
// startup dir comes from the conv's most-recent session row (sessions are
// most-recent-first), mirroring ResolveLocation's FindSessionByConvID.
func (rc *snapshotRowCache) computeLoc(convID string) agentLocationView {
	startupDir := ""
	if rows := rc.sessions[convID]; len(rows) > 0 {
		startupDir = rows[0].Cwd
	}
	loc := agent.ResolveLocationFromParts(startupDir, rc.convIndex[convID], rc.workdirs[convID], rc.workspaces[convID])
	return locationViewFrom(loc)
}

// agentID returns the stable actor id for a conv from the batch, or "" for a
// non-agent conv — the same signal peerAgentID returns.
func (rc *snapshotRowCache) agentID(convID string) string {
	return rc.agents[convID].AgentID
}

// inactiveActor reports whether convID resolves to a retired actor or to a
// superseded generation of an active actor. Both facts ride the existing
// AgentsByConv batch, replacing snapshot-wide retired-roster and succession
// history preloads.
func (rc *snapshotRowCache) inactiveActor(convID string) bool {
	a, ok := rc.agents[convID]
	return ok && (a.Retired || a.Superseded || a.CurrentConvID != convID)
}

// titleFor resolves a conv's display name from the batch (custom title >
// pending name > summary > first prompt > UnknownTitle) with zero queries.
func (rc *snapshotRowCache) titleFor(convID string) string {
	return agent.CachedTitleFromParts(rc.convIndex[convID], rc.agents[convID].PendingName)
}

// createdFor returns a conv's Age timestamp from the batch, or "" when unknown.
//
// It returns the earliest valid instant from the actor's immutable birth time
// (agents.created_at) and conv_index.Created. Actor birth normally wins, is
// available before the harness writes its first .jsonl event, and remains stable
// across /clear and reincarnation. Conversation creation wins only for
// legacy/backfilled actors whose row was stamped after the conversation already
// existed. The UTC RFC3339Nano result mirrors agent.MemberCreated; sorters parse
// it and compare instants.
func (rc *snapshotRowCache) createdFor(convID string) string {
	convCreated := ""
	if row := rc.convIndex[convID]; row != nil {
		convCreated = row.Created
	}
	return db.EarliestAgeTimestamp(rc.agents[convID].CreatedAt, convCreated)
}

// viewFor assembles (and memoizes) a conv's full row bundle from the batch. A
// second call for the same conv — the member loop and addAgent both resolve the
// same conv, and a conv in several groups recurs — returns the cached bundle.
func (rc *snapshotRowCache) viewFor(convID string) *convRowBundle {
	if b, ok := rc.memo[convID]; ok {
		return b
	}
	loc := rc.locs[convID]
	b := &convRowBundle{
		AgentID: rc.agentID(convID),
		Title:   rc.titleFor(convID),
		Created: rc.createdFor(convID),
		Loc:     loc,
		Links:   branchLinksForRow(convID, loc, rc.workspaces[convID], rc.gitCache),
		Online:  isConvOnlineInSessions(rc.sessions[convID], rc.alive),
		State: stateForConvInSessionsTimed(rc.sessions[convID], rc.alive, func(d time.Duration) {
			rc.codexTelemetryDuration += d
		}),
	}
	rc.memo[convID] = b
	return b
}
