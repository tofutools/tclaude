package agentd

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The snapshot's load steps: each runs its reads under the request span's
// "preload" phase, in the order the Debug tab shows them, and returns what it
// read. handleDashboardSnapshot calls them in sequence.

// snapshotPreload is what the snapshot's first fan-out reads, plus the two
// views derived from it in memory (the grant-only permissions and the sorted
// default permissions). Every read is best-effort: a failed one leaves its
// zero value and the snapshot renders without it, as before.
type snapshotPreload struct {
	groups               []*db.AgentGroup
	sandboxProfiles      []*db.SandboxProfile
	globalSandboxProfile *db.SandboxProfile
	allGrants            map[string][]string
	allOverrides         map[string]map[string]string
	allOverrideScopes    map[string]map[string]string
	cfg                  *config.Config
	notifyPrefs          map[string]string
	// Every active ACTOR — the canonical roster (JOH-26 PR3b reads the
	// agent-level roster, one row per live actor at its current conv). Unlike
	// the old "online ungrouped session" probe, this includes OFFLINE agents: a
	// conv that was an agent yesterday keeps showing after its tmux pane closed,
	// instead of silently vanishing. Plain conversations that were never
	// promoted are not here — they surface in out.Conversations.
	activeConvIDs  []string
	retiredTotal   int
	agentRosterErr error
	// Active sudo grants across every agent; the roster pass buckets them per
	// conv-id for O(1) per-agent rendering.
	sudoGrants            []*db.SudoGrant
	defaults              []string
	groupsRouteMapEnabled bool
}

// loadSnapshotPreload runs the first preload fan-out under span's "preload"
// phase and derives the grant-only and default-permission views.
func loadSnapshotPreload(span *perfSpan) snapshotPreload {
	var p snapshotPreload
	// The snapshot preload is a set of independent SQLite WAL reads plus one
	// config-file read. Run them as one fan-out so its wall clock is the slowest
	// read, not the sum of ~dozen fixed round trips (which dominated even with
	// only a handful of agents).
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"groups", func() { p.groups, _ = db.ListAgentGroups() }},
		snapshotNamedLoad{"sandbox_profiles", func() { p.sandboxProfiles, _ = db.ListSandboxProfiles() }},
		snapshotNamedLoad{"global_sandbox", func() { p.globalSandboxProfile, _ = db.GetGlobalSandboxProfile() }},
		snapshotNamedLoad{"permission_overrides", func() { p.allOverrides, _ = db.ListAllAgentPermissionOverrides() }},
		snapshotNamedLoad{"permission_scopes", func() { p.allOverrideScopes, _ = db.ListAllAgentPermissionScopes() }},
		snapshotNamedLoad{"config", func() { p.cfg, _ = config.Load() }},
		snapshotNamedLoad{"notify_prefs", func() { p.notifyPrefs, _ = db.ListConvNotifyPrefs() }},
		snapshotNamedLoad{"agent_roster", func() {
			p.activeConvIDs, p.retiredTotal, p.agentRosterErr = db.ListAgentRosterState()
		}},
		snapshotNamedLoad{"sudo_grants", func() { p.sudoGrants, _ = db.ListAllActiveSudoGrants() }},
	)...)
	// The all-overrides read contains both grant and deny effects. Derive the
	// grant-only compatibility view in memory instead of scanning and sorting
	// the same permission table a second time on every poll.
	p.allGrants = map[string][]string{}
	for convID, effects := range p.allOverrides {
		for slug, effect := range effects {
			if effect == db.PermEffectGrant {
				p.allGrants[convID] = append(p.allGrants[convID], slug)
			}
		}
		sort.Strings(p.allGrants[convID])
	}
	p.defaults = []string{}
	if p.cfg != nil && p.cfg.Agent != nil {
		p.defaults = append(p.defaults, p.cfg.Agent.DefaultPermissions...)
	}
	sort.Strings(p.defaults)
	p.groupsRouteMapEnabled = p.cfg.GroupsRouteMapEnabled()
	return p
}

// snapshotGroupRows is every group's members, owners and route-map rows,
// keyed by group id. Routes, leases and Darwin launches stay nil unless the
// route map is enabled (and, for launches, on darwin).
type snapshotGroupRows struct {
	membersByGroup        map[int64][]*db.AgentGroupMember
	ownersByGroup         map[int64][]*db.AgentGroupOwner
	routesByGroup         map[int64][]*db.AgentRoute
	leasesByGroup         map[int64][]*db.AgentRouteLease
	darwinLaunchesByGroup map[int64][]*db.DarwinRouteLaunch
}

// Load each group's members + owners ONCE. ListAgentGroupMembers used to
// run twice per group (the muted-notify pass below + the main group loop),
// and ListAgentGroupOwners once in the group loop; keying both by group id
// here lets inMutedGroup, the conv-set collect and the group loop all reuse
// a single read.
func loadSnapshotGroupRows(span *perfSpan, groups []*db.AgentGroup, groupsRouteMapEnabled bool) snapshotGroupRows {
	groupIDs := make([]int64, len(groups))
	for i, group := range groups {
		groupIDs[i] = group.ID
	}
	var (
		rows              snapshotGroupRows
		membersErr        error
		ownersErr         error
		routesErr         error
		leasesErr         error
		darwinLaunchesErr error
	)
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"group_members", func() { rows.membersByGroup, membersErr = db.ListAgentGroupMembersBatch(groupIDs) }},
		snapshotNamedLoad{"group_owners", func() { rows.ownersByGroup, ownersErr = db.ListAgentGroupOwnersBatch(groupIDs) }},
		snapshotNamedLoad{"routes", func() {
			if groupsRouteMapEnabled {
				rows.routesByGroup, routesErr = db.ListAgentRoutesBatch(groupIDs)
			}
		}},
		snapshotNamedLoad{"route_leases", func() {
			if groupsRouteMapEnabled {
				rows.leasesByGroup, leasesErr = db.ListAgentRouteLeasesBatch(groupIDs)
			}
		}},
		snapshotNamedLoad{"darwin_route_launches", func() {
			if groupsRouteMapEnabled && dashboardRouteMapPlatform == "darwin" {
				rows.darwinLaunchesByGroup, darwinLaunchesErr = db.ListActiveDarwinRouteLaunchesByGroup(groupIDs)
			}
		}},
	)...)
	if membersErr != nil {
		slog.Warn("snapshot: failed to preload group members", "error", membersErr)
	}
	if ownersErr != nil {
		slog.Warn("snapshot: failed to preload group owners", "error", ownersErr)
	}
	if routesErr != nil {
		slog.Warn("snapshot: failed to preload group routes", "error", routesErr)
	}
	if leasesErr != nil {
		slog.Warn("snapshot: failed to preload route leases", "error", leasesErr)
	}
	if darwinLaunchesErr != nil {
		slog.Warn("snapshot: failed to preload Darwin route launch contracts", "error", darwinLaunchesErr)
	}
	return rows
}

// Notification-filter state: the per-agent overrides plus the set
// of convs sitting in at least one muted (non-archived) group.
// notifyEffective mirrors notify.AllowedForConv — agent pref wins,
// else any muted group silences — so the bells the dashboard
// renders agree with what the notify path will actually do.
func snapshotNotifyFilter(groups []*db.AgentGroup, membersByGroup map[int64][]*db.AgentGroupMember,
	notifyPrefs map[string]string) func(convID string) bool {
	inMutedGroup := map[string]bool{}
	for _, g := range groups {
		if g.IsArchived() || g.NotifyEnabled {
			continue
		}
		for _, m := range membersByGroup[g.ID] {
			inMutedGroup[m.ConvID] = true
		}
	}
	return func(convID string) bool {
		switch notifyPrefs[convID] {
		case db.NotifyPrefOn:
			return true
		case db.NotifyPrefOff:
			return false
		}
		return !inMutedGroup[convID]
	}
}

// Collect the full conv set the snapshot will render — group members ∪
// owners ∪ grant/override holders ∪ active agents ∪ sudo holders — then
// batch-load every per-conv table once (TCL-368/TCL-367). rc.viewFor(convID)
// returns a memoized row bundle so the member loop, addAgent and the owners
// pass share ONE computation instead of ~13 point queries each, and the
// location resolves from the batch without a per-poll .jsonl rescan.
func collectSnapshotConvIDs(span *perfSpan, p snapshotPreload, rows snapshotGroupRows) []string {
	convSetStart := time.Now()
	convSet := map[string]struct{}{}
	addConv := func(convID string) {
		if convID != "" {
			convSet[convID] = struct{}{}
		}
	}
	for _, members := range rows.membersByGroup {
		for _, m := range members {
			addConv(m.ConvID)
		}
	}
	for _, owners := range rows.ownersByGroup {
		for _, o := range owners {
			addConv(o.ConvID)
		}
	}
	for convID := range p.allGrants {
		addConv(convID)
	}
	for convID := range p.allOverrides {
		addConv(convID)
	}
	for _, convID := range p.activeConvIDs {
		addConv(convID)
	}
	for _, g := range p.sudoGrants {
		addConv(g.ConvID)
	}
	convIDs := make([]string, 0, len(convSet))
	for convID := range convSet {
		convIDs = append(convIDs, convID)
	}
	span.addChildDuration("preload", "conv_set", time.Since(convSetStart))
	return convIDs
}

// snapshotPRIndex is everything the PR badges, task links and tags draw
// from: the agent-keyed task refs, presented PRs and tags, the authored and
// tracked PR lists, the locally known PRs with their cached titles, and one
// freshest state, check summary and review summary per PR identity.
type snapshotPRIndex struct {
	taskRefs         map[string]db.AgentTaskRef
	presentedPRs     map[string][]db.AgentPR
	allTags          map[string][]string
	cachedPRTitles   map[string]string
	authoredOpenPRs  dashboardAuthoredOpenPRs
	trackedPRAuthors []dashboardAuthoredOpenPRs
	localPRs         []locallyKnownPR
	freshPRStates    prStateIndex
	prChecks         map[string]*prChecksSummary
	prReviews        map[string]*prReviewSummary
}

func (x snapshotPRIndex) taskRefFor(agentID string) taskRefView {
	return taskRefViewFor(x.taskRefs[agentID])
}

func (x snapshotPRIndex) presentedPRsFor(agentID string) []presentedPRView {
	return presentedPRViews(x.presentedPRs[agentID])
}

// Same preload discipline as taskRefs: one ListAllAgentTags per snapshot,
// keyed by agent_id, looked up per row (not a query per member/agent in
// this 2s-polled path). The stored set is already sorted alphabetically.
func (x snapshotPRIndex) tagsFor(agentID string) tagsView {
	return tagsView{Tags: x.allTags[agentID]}
}

// loadSnapshotPRIndex runs the agent-keyed and PR-cache preloads over the
// row cache's convs and visible agents, then resolves the PR indexes.
func loadSnapshotPRIndex(span *perfSpan, rc *snapshotRowCache, convIDs []string, cfg *config.Config) snapshotPRIndex {
	var (
		x              snapshotPRIndex
		cachedPRStates prStateIndex
	)
	branchPRCacheURLs := make([]string, 0, len(convIDs)*3)
	for _, convID := range convIDs {
		links := rc.viewFor(convID).Links
		// The statusline PR rides along so its badge gets the same cached
		// checks, reviews, title and background refresh as a slot PR.
		branchPRCacheURLs = append(branchPRCacheURLs, links.BranchPRURL, links.StartupPRURL, links.statuslinePR.URL)
	}
	visibleAgentIDs := make([]string, 0, len(rc.agents))
	visibleAgentSet := make(map[string]struct{}, len(rc.agents))
	for _, actor := range rc.agents {
		if actor.AgentID == "" {
			continue
		}
		if _, seen := visibleAgentSet[actor.AgentID]; seen {
			continue
		}
		visibleAgentSet[actor.AgentID] = struct{}{}
		visibleAgentIDs = append(visibleAgentIDs, actor.AgentID)
	}
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"task_refs", func() {
			x.taskRefs, _ = db.ListAgentTaskRefsByAgentIDs(visibleAgentIDs)
		}},
		snapshotNamedLoad{"presented_prs", func() { x.presentedPRs = preloadPresentedPRsForDashboard(time.Now()) }},
		snapshotNamedLoad{"tags", func() { x.allTags, _ = db.ListAllAgentTags() }},
		snapshotNamedLoad{"authored_open_prs", func() { x.authoredOpenPRs = loadAuthoredOpenPRsSnapshot(cfg) }},
		snapshotNamedLoad{"tracked_pr_authors", func() { x.trackedPRAuthors = loadTrackedPRAuthorsSnapshot(cfg) }},
	)...)
	// Every PR visualization draws state and checks from these two canonical
	// identity-keyed caches. Build the union first so branch/startup,
	// explicitly presented and footer-only PRs share one bounded batch read.
	allPRCacheURLs := append([]string(nil), branchPRCacheURLs...)
	for _, rows := range x.presentedPRs {
		for _, row := range rows {
			allPRCacheURLs = append(allPRCacheURLs, row.PRURL)
		}
	}
	x.localPRs = make([]locallyKnownPR, 0, len(allPRCacheURLs))
	for _, rawURL := range branchPRCacheURLs {
		x.localPRs = append(x.localPRs, locallyKnownPR{URL: rawURL})
	}
	for _, rows := range x.presentedPRs {
		for _, row := range rows {
			x.localPRs = append(x.localPRs, locallyKnownPR{URL: row.PRURL})
		}
	}
	for _, list := range [][]dashboardAuthoredOpenPR{x.authoredOpenPRs.Items, x.authoredOpenPRs.Recent} {
		for _, pr := range list {
			allPRCacheURLs = append(allPRCacheURLs, pr.URL)
		}
	}
	for _, view := range x.trackedPRAuthors {
		for _, list := range [][]dashboardAuthoredOpenPR{view.Items, view.Recent} {
			for _, pr := range list {
				allPRCacheURLs = append(allPRCacheURLs, pr.URL)
			}
		}
	}
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"pr_state_cache", func() { cachedPRStates, x.cachedPRTitles = cachedPresentedPRMetadata(allPRCacheURLs) }},
	)...)

	indexedPRs := make(map[string]bool, len(x.authoredOpenPRs.Items)+len(x.authoredOpenPRs.Recent))
	for _, list := range [][]dashboardAuthoredOpenPR{x.authoredOpenPRs.Items, x.authoredOpenPRs.Recent} {
		for _, pr := range list {
			indexedPRs[prStateKey(pr.URL)] = true
		}
	}
	for i := range x.localPRs {
		key := prStateKey(x.localPRs[i].URL)
		x.localPRs[i].Title = x.cachedPRTitles[key]
		// Local discoveries may not be indexed by the authored search yet,
		// or may belong to another author. Reuse the background metadata
		// refresh; snapshot rendering itself never waits for GitHub.
		if x.authoredOpenPRs.Available && strings.HasPrefix(key, "github:") &&
			!indexedPRs[key] && !presentedPRTitleCacheFresh(x.localPRs[i].URL, time.Now()) {
			schedulePresentedPRRefresh("", x.localPRs[i].URL)
		}
		indexedPRs[key] = true
	}
	// Resolve one freshest state per PR URL across every dashboard source.
	// This deliberately crosses agent rows: a presented-PR poll on one row and
	// an automatic branch link on another are observations of the same PR, not
	// independent display state.
	x.freshPRStates = make(prStateIndex)
	addAuthoredOpenPRStates(x.freshPRStates, x.authoredOpenPRs)
	for _, convID := range convIDs {
		x.freshPRStates.addRepoLinks(rc.viewFor(convID).Links)
	}
	for key, candidate := range cachedPRStates {
		current, ok := x.freshPRStates[key]
		if !ok {
			x.freshPRStates[key] = candidate
			continue
		}
		state, updatedAt := newestPRState(
			current.state, current.updatedAt, candidate.state, candidate.updatedAt)
		x.freshPRStates[key] = prStateObservation{state: state, updatedAt: updatedAt}
	}
	for _, rows := range x.presentedPRs {
		for _, row := range rows {
			x.freshPRStates.add(row.PRURL, row.State, row.UpdatedAt)
		}
	}
	// CI check summaries for every PR badge this snapshot will draw, in one
	// batched cache read. Nothing here shells out: the blobs are written by
	// the branch-link / presented-PR refreshes that already run `gh`, and by
	// the hover endpoint. Presented PR URLs join the branch/startup ones so a
	// presented badge gets the same indicator as an automatic one.
	x.prChecks = prChecksIndexFor(allPRCacheURLs)
	// Review status rides the same identity-keyed batch discipline; see
	// prreviews.go. Written only by GitHub reads that already happen.
	x.prReviews = prReviewsIndexFor(allPRCacheURLs)
	return x
}
